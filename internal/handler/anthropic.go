package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/user/go2api/internal/cache"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/store"
)

// Anthropic handles POST /v1/messages.
//
// It accepts Anthropic-format requests for ANY model. When the model belongs
// to the OpenAI family (e.g. kimi-k3, glm-5.2) the request is transparently
// converted to OpenAI Chat Completions format, forwarded to the upstream
// /chat/completions endpoint, and the response is converted back.
type Anthropic struct {
	Proxy  *proxy.Proxy
	Cache  *cache.Cache
	Store  *store.DB
	Logger *slog.Logger
}

// ServeHTTP implements http.Handler.
func (h *Anthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if probe.Model == "" {
		writeJSONError(w, http.StatusBadRequest, "model is required")
		return
	}

	family := proxy.ModelFamily(probe.Model)

	// OpenAI-family model: cross-format flow (Anthropic -> OpenAI -> Anthropic).
	if family == proxy.FamilyOpenAI {
		h.serveCrossFormatAsOpenAI(w, r, body, probe.Model, probe.Stream, start)
		return
	}

	// Native Anthropic flow.
	plan := proxy.ForwardPlan{
		Endpoint: "/messages",
		Method:   http.MethodPost,
		Body:     body,
		Model:    probe.Model,
		Stream:   probe.Stream,
	}

	cacheKey := ""
	if !probe.Stream {
		if key, kerr := cache.KeyFor(cache.Fingerprint{
			Endpoint: "/v1/messages",
			Model:    probe.Model,
			Stream:   false,
			RawBody:  body,
		}); kerr == nil {
			cacheKey = key
			if item, gerr := h.Cache.Get(r.Context(), key); gerr == nil {
				writeCached(w, item)
				_ = h.Store.LogRequest(r.Context(), store.LogRow{
					Timestamp: time.Now(), Model: probe.Model,
					Status: item.Status, CacheHit: true,
					LatencyMs: time.Since(start).Milliseconds(),
				})
				return
			} else if !errors.Is(gerr, cache.ErrMiss) {
				h.Logger.Warn("cache get failed", "err", gerr)
			}
		}
	}

	result, err := h.Proxy.Forward(r.Context(), plan, w)
	if err != nil {
		status := http.StatusBadGateway
		var ue *proxy.ErrUpstream
		if errors.As(err, &ue) {
			status = ue.Status
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(errorBody(err))
		_ = h.Store.LogRequest(r.Context(), store.LogRow{
			Timestamp: time.Now(), Model: probe.Model,
			Status: status, CacheHit: false,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     err.Error(),
		})
		return
	}

	if !probe.Stream && cacheKey != "" {
		if perr := h.Cache.Put(r.Context(), cacheKey, probe.Model, body, cache.Item{
			Status:      result.Status,
			ContentType: result.ContentType,
			Body:        result.Body,
		}); perr != nil {
			h.Logger.Warn("cache put failed", "err", perr)
		}
		w.Header().Set("Content-Type", result.ContentType)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	} else if !probe.Stream {
		w.Header().Set("Content-Type", result.ContentType)
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}

	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(), Model: probe.Model,
		Status: result.Status, CacheHit: false,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

// serveCrossFormatAsOpenAI converts an incoming Anthropic request to OpenAI
// Chat Completions, forwards to /chat/completions, and converts the
// response back to Anthropic Messages.
func (h *Anthropic) serveCrossFormatAsOpenAI(w http.ResponseWriter, r *http.Request, anthropicBody []byte, model string, stream bool, start time.Time) {
	h.Logger.Info("cross-format: Anthropic request for OpenAI-family model",
		"model", model, "stream", stream)
	var anthropicMap map[string]any
	if err := json.Unmarshal(anthropicBody, &anthropicMap); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	openaiMap, err := proxy.AnthropicToOpenAIRequest(anthropicMap)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	openaiBody, err := json.Marshal(openaiMap)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal openai body: "+err.Error())
		return
	}

	plan := proxy.ForwardPlan{
		Endpoint: "/chat/completions",
		Method:   http.MethodPost,
		Body:     openaiBody,
		Model:    model,
		Stream:   stream,
	}

	if stream {
		h.streamAnthropicToOpenAI(w, r, plan, model, start)
		return
	}

	result, err := h.Proxy.Forward(r.Context(), plan, nil)
	if err != nil {
		status := http.StatusBadGateway
		var ue *proxy.ErrUpstream
		if errors.As(err, &ue) {
			status = ue.Status
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(errorBody(err))
		_ = h.Store.LogRequest(r.Context(), store.LogRow{
			Timestamp: time.Now(), Model: model,
			Status: status, CacheHit: false,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     err.Error(),
		})
		return
	}

	// Convert OpenAI response back to Anthropic.
	var openaiResp map[string]any
	if err := json.Unmarshal(result.Body, &openaiResp); err != nil {
		writeJSONError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
	anthropicResp, err := proxy.OpenAIToAnthropicResponse(openaiResp)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "convert response: "+err.Error())
		return
	}
	out, _ := json.Marshal(anthropicResp)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Format-Converted", "anthropic->openai->anthropic")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(), Model: model,
		Status: http.StatusOK, CacheHit: false,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

// streamAnthropicToOpenAI handles streaming where the upstream is OpenAI
// format but the client expects Anthropic SSE.
func (h *Anthropic) streamAnthropicToOpenAI(w http.ResponseWriter, r *http.Request, plan proxy.ForwardPlan, model string, start time.Time) {
	if err := h.streamAnthropicConverted(r.Context(), plan, w); err != nil {
		status := http.StatusBadGateway
		var ue *proxy.ErrUpstream
		if errors.As(err, &ue) {
			status = ue.Status
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(errorBody(err))
		_ = h.Store.LogRequest(r.Context(), store.LogRow{
			Timestamp: time.Now(), Model: model,
			Status: status, CacheHit: false,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     err.Error(),
		})
		return
	}
	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(), Model: model,
		Status: http.StatusOK, CacheHit: false,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

// streamAnthropicConverted pipes an OpenAI SSE upstream through per-chunk
// conversion and writes Anthropic SSE events to w.
func (h *Anthropic) streamAnthropicConverted(ctx context.Context, plan proxy.ForwardPlan, w http.ResponseWriter) error {
	maxAttempts := 1
	if h.Proxy.Pool().Enabled() {
		maxAttempts = 1 + h.Proxy.Pool().MaxRetries()
	}
	if maxAttempts > 10 {
		maxAttempts = 10
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		key, err := h.Proxy.PickKey(ctx)
		if err != nil {
			return err
		}
		if attempt > 0 {
			h.Logger.Info("retrying with next key", "attempt", attempt+1, "key", key.ID)
		}

		upstreamURL := h.Proxy.BaseURL() + plan.Endpoint
		req, err := http.NewRequestWithContext(ctx, plan.Method, upstreamURL, bytes.NewReader(plan.Body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key.APIKey)
		// /messages (Anthropic) needs x-api-key + anthropic-version.
		if plan.Endpoint == "/messages" {
			req.Header.Set("x-api-key", key.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		resp, err := h.Proxy.Client().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upstream do: %w", err)
			h.Proxy.MarkFailure(ctx, key, 0, lastErr.Error())
			continue
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
			resp.Body.Close()
			ue := &proxy.ErrUpstream{Status: resp.StatusCode, Body: body}
			h.Proxy.MarkFailure(ctx, key, resp.StatusCode, ue.Error())
			if !h.Proxy.Pool().IsRetryableStatus(resp.StatusCode) {
				return ue
			}
			lastErr = ue
			continue
		}

		// Headers and 200.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-Format-Converted", "anthropic<->openai")
		w.WriteHeader(resp.StatusCode)

		flusher, _ := w.(http.Flusher)
		reader := bufio.NewReaderSize(resp.Body, 16*1024)
		var dataBuf strings.Builder
		state := &proxy.OpenAIStreamState{}

		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				h.Proxy.MarkFailure(ctx, key, 0, err.Error())
				return err
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				dataBuf.WriteString(strings.TrimPrefix(line, "data:"))
				dataBuf.WriteString("\n")
			} else if line == "" && dataBuf.Len() > 0 {
				payload := strings.TrimSpace(dataBuf.String())
				dataBuf.Reset()
				if payload == "" || payload == "[DONE]" {
					if payload == "[DONE]" {
						// Emit final close events for the Anthropic stream.
						for _, ev := range proxy.FinalizeOpenAIStream(state) {
							b, _ := json.Marshal(ev)
							_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
						}
						if flusher != nil {
							flusher.Flush()
						}
					}
				} else {
					var chunk map[string]any
					if jsonErr := json.Unmarshal([]byte(payload), &chunk); jsonErr == nil {
						events := proxy.OpenAIChunkToAnthropicEvents(chunk, state)
						for _, ev := range events {
							b, _ := json.Marshal(ev)
							et, _ := ev["type"].(string)
							_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", et, b)
						}
						if flusher != nil {
							flusher.Flush()
						}
					}
				}
			}
			if err == io.EOF {
				break
			}
		}
		h.Proxy.MarkSuccess(ctx, key)
		return nil
	}
	return lastErr
}
