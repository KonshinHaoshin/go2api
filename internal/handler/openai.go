// Package handler contains the HTTP route handlers exposed by go2api.
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
	"github.com/user/go2api/internal/pricing"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/store"
)

// OpenAI handles POST /v1/chat/completions.
//
// It accepts OpenAI-format requests for ANY model. When the model belongs to
// the Anthropic family (e.g. minimax-m3, qwen3.7-max) the request is
// transparently converted to Anthropic Messages format, forwarded to the
// upstream /messages endpoint, and the response is converted back. The
// client sees no difference.
type OpenAI struct {
	Proxy  *proxy.Proxy
	Cache  *cache.Cache
	Store  *store.DB
	Logger *slog.Logger
}

// ServeHTTP implements http.Handler.
func (h *OpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	// Probe just enough to extract model + stream flag without parsing the rest.
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

	// Anthropic-family model: cross-format flow. Cache is skipped because the
	// post-conversion body is in a different format than what the client sent.
	if family == proxy.FamilyAnthropic {
		h.serveCrossFormatAsAnthropic(w, r, body, probe.Model, probe.Stream, start)
		return
	}

	// Native OpenAI flow.
	plan := proxy.ForwardPlan{
		Endpoint: "/chat/completions",
		Method:   http.MethodPost,
		Body:     body,
		Model:    probe.Model,
		Stream:   probe.Stream,
	}

	cacheKey := ""
	if !probe.Stream {
		if key, kerr := cache.KeyFor(cache.Fingerprint{
			Endpoint: "/v1/chat/completions",
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
		LatencyMs:        time.Since(start).Milliseconds(),
		KeyID:            result.KeyID,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		CachedTokens:     result.Usage.CachedPromptTokens,
		Cost:             pricing.Cost(probe.Model, result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.CachedPromptTokens),
	})
}

// serveCrossFormatAsAnthropic converts an incoming OpenAI request to
// Anthropic format, forwards to /messages, and converts the response back.
func (h *OpenAI) serveCrossFormatAsAnthropic(w http.ResponseWriter, r *http.Request, openaiBody []byte, model string, stream bool, start time.Time) {
	h.Logger.Info("cross-format: OpenAI request for Anthropic-family model",
		"model", model, "stream", stream)
	var openaiMap map[string]any
	if err := json.Unmarshal(openaiBody, &openaiMap); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	anthropicMap, err := proxy.OpenAIToAnthropicRequest(openaiMap)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	anthropicBody, err := json.Marshal(anthropicMap)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal anthropic body: "+err.Error())
		return
	}

	plan := proxy.ForwardPlan{
		Endpoint: "/messages",
		Method:   http.MethodPost,
		Body:     anthropicBody,
		Model:    model,
		Stream:   stream,
	}

	if stream {
		h.streamOpenAIToAnthropic(w, r, plan, model, start)
		return
	}

	result, err := h.Proxy.Forward(r.Context(), plan, nil)
	if err != nil {
		h.writeUpstreamError(w, r, model, start, err)
		return
	}

	// Convert the Anthropic response into an OpenAI response.
	var anthropicResp map[string]any
	if err := json.Unmarshal(result.Body, &anthropicResp); err != nil {
		writeJSONError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
	openaiResp, err := proxy.AnthropicToOpenAIResponse(anthropicResp)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "convert response: "+err.Error())
		return
	}
	out, _ := json.Marshal(openaiResp)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Format-Converted", "openai->anthropic->openai")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(), Model: model,
		Status: http.StatusOK, CacheHit: false,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

// streamOpenAIToAnthropic handles a streaming request where the upstream is
// Anthropic-format but the client expects OpenAI SSE.
func (h *OpenAI) streamOpenAIToAnthropic(w http.ResponseWriter, r *http.Request, plan proxy.ForwardPlan, model string, start time.Time) {
	// Build a custom streaming pipeline: upstream Anthropic SSE → per-event
	// conversion → OpenAI SSE → client.
	if err := h.streamConverted(r.Context(), plan, w, convertAnthropicEventToOpenAI); err != nil {
		h.writeUpstreamError(w, r, model, start, err)
		return
	}
	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(), Model: model,
		Status: http.StatusOK, CacheHit: false,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

func (h *OpenAI) writeUpstreamError(w http.ResponseWriter, r *http.Request, model string, start time.Time, err error) {
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
}

// streamConverted is a streaming helper that picks a key, sends the request
// to the upstream, and forwards the response through per-event converters
// before writing to w. converter is called on each parsed Anthropic event
// map and returns one or more OpenAI chunks.
func (h *OpenAI) streamConverted(ctx context.Context, plan proxy.ForwardPlan, w http.ResponseWriter, convert func(map[string]any) []map[string]any) error {
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

		// Pick the correct upstream URL based on the endpoint in the plan.
		upstreamURL := h.Proxy.BaseURL() + plan.Endpoint
		req, err := http.NewRequestWithContext(ctx, plan.Method, upstreamURL, bytes.NewReader(plan.Body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key.APIKey)
		// /messages (Anthropic) needs both x-api-key and anthropic-version
		// in addition to Authorization. Without these the upstream returns
		// 401 "Missing API key." even though Authorization is set.
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
		w.Header().Set("X-Format-Converted", "openai<->anthropic")
		w.WriteHeader(resp.StatusCode)

		flusher, _ := w.(http.Flusher)
		reader := bufio.NewReaderSize(resp.Body, 16*1024)
		var dataBuf strings.Builder

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
						// Forward the [DONE] marker.
						_, _ = io.WriteString(w, "data: [DONE]\n\n")
						if flusher != nil {
							flusher.Flush()
						}
					}
				} else {
					var event map[string]any
					if jsonErr := json.Unmarshal([]byte(payload), &event); jsonErr == nil {
						chunks := convert(event)
						for _, c := range chunks {
							b, _ := json.Marshal(c)
							_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
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

// convertAnthropicEventToOpenAI bridges a single Anthropic SSE event into
// one or more OpenAI chunks.
func convertAnthropicEventToOpenAI(event map[string]any) []map[string]any {
	return proxy.AnthropicEventToOpenAIChunk(event)
}

func writeCached(w http.ResponseWriter, item cache.Item) {
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(item.Status)
	_, _ = w.Write(item.Body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(errorBodyStr(msg))
}

func errorBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "proxy_error",
		},
	})
	return b
}

func errorBodyStr(msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request",
		},
	})
	return b
}
