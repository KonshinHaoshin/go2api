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
	"time"

	"github.com/user/go2api/internal/keypool"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/proxy/responses"
	"github.com/user/go2api/internal/store"
)

// Responses handles POST /v1/responses.
//
// Phase 1 covers the non-streaming text-only path; Phase 2 adds streaming
// (SSE) text support without breaking the non-stream contract. Subsequent
// phases layer tools + reasoning (3), images (4), previous_response_id
// replay (5), and error-event fidelity (6) on top of this skeleton.
type Responses struct {
	Proxy  *proxy.Proxy
	Store  *store.DB
	Logger *slog.Logger
}

// ServeHTTP implements http.Handler.
func (h *Responses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	var req responses.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := responses.ValidateRequest(&req); err != nil {
		h.writeInvalidRequest(w, err)
		return
	}

	if req.Stream {
		h.serveStream(w, r, &req, start)
		return
	}
	h.serveNonStream(w, r, &req, start)
}

func (h *Responses) serveNonStream(w http.ResponseWriter, r *http.Request, req *responses.Request, start time.Time) {
	resp, keyID, _, err := h.dispatch(r, req)
	if err != nil {
		h.writeUpstreamError(r.Context(), w, req.Model, start, err)
		return
	}
	out, mErr := json.Marshal(resp)
	if mErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal response: "+mErr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Format-Converted", "responses<->"+string(proxy.ModelFamily(req.Model)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp:        time.Now(),
		Model:            req.Model,
		Status:           http.StatusOK,
		LatencyMs:        time.Since(start).Milliseconds(),
		KeyID:            keyID,
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		CachedTokens:     tokens(resp.Usage.InputTokensDetails),
	})
}

// serveStream dispatches a streaming request. It honors the same conversion
// pipeline as serveNonStream but emits Server-Sent Events instead of a JSON
// envelope. The wire invariant is exactly one terminal event per request —
// either `response.completed` (on success) or `response.failed` (if a stream
// error happens after we have committed 200). HTTP status is fixed to 200
// from the moment we write the first SSE frame.
func (h *Responses) serveStream(w http.ResponseWriter, r *http.Request, req *responses.Request, start time.Time) {
	chat, err := responses.ToChatRequest(req)
	if err != nil {
		h.writeInvalidRequest(w, err)
		return
	}
	// Phase 5: replay previous_response_id history before forwarding.
	history, _, herr := responses.ResolveHistory(r.Context(), h.Store, req)
	if herr != nil {
		h.writeUpstreamError(r.Context(), w, req.Model, start, herr)
		return
	}
	responses.MergeHistory(chat, history)

	family := proxy.ModelFamily(req.Model)
	endpoint := proxy.EndpointForFamily(family)

	var upstreamBody []byte
	switch family {
	case proxy.FamilyAnthropic:
		m, err := responses.ToAnthropicRequest(chat, true)
		if err != nil {
			h.writeInvalidRequest(w, err)
			return
		}
		upstreamBody, err = json.Marshal(m)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal upstream: "+err.Error())
			return
		}
	default:
		m, err := responses.ToOpenAIChatRequest(chat, true)
		if err != nil {
			h.writeInvalidRequest(w, err)
			return
		}
		upstreamBody, err = json.Marshal(m)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal upstream: "+err.Error())
			return
		}
	}

	plan := proxy.ForwardPlan{
		Endpoint: endpoint,
		Method:   http.MethodPost,
		Body:     upstreamBody,
		Model:    req.Model,
		Stream:   true,
	}

	if err := h.streamWithRetries(r.Context(), plan, req, family, w, r); err != nil {
		h.Logger.Warn("responses: stream aborted",
			"model", req.Model, "family", string(family), "err", err)
	}
	_ = h.Store.LogRequest(r.Context(), store.LogRow{
		Timestamp: time.Now(),
		Model:     req.Model,
		Status:    http.StatusOK,
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

// streamWithRetries performs a streaming forward to upstream with key pool
// retries before headers are committed. Once any Responses SSE event is
// written, retries stop — re-running with a different key would duplicate the
// response ID and contradict the client's view of the stream.
//
// Pre-commit error paths (upstream 5xx, transport errors before headers) are
// translated into a JSON error envelope, identical to the non-stream path.
// Post-commit errors become response.failed on the SSE channel.
func (h *Responses) streamWithRetries(ctx context.Context, plan proxy.ForwardPlan, req *responses.Request, family proxy.UpstreamFamily, w http.ResponseWriter, r *http.Request) error {
	maxAttempts := 1
	if h.Proxy.Pool().Enabled() {
		maxAttempts = 1 + h.Proxy.Pool().MaxRetries()
	}
	if maxAttempts > 10 {
		maxAttempts = 10
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := h.Proxy.PickKey(ctx)
		if err != nil {
			return err
		}
		if attempt > 0 {
			h.Logger.Info("retrying with next key", "attempt", attempt+1, "key", key.ID)
		}
		err = h.runOneStreamAttempt(ctx, plan, req, family, key, w, r)
		if err == nil {
			h.Proxy.MarkSuccess(ctx, key)
			return nil
		}
		lastErr = err
		status := 0
		var ue *proxy.ErrUpstream
		if errors.As(err, &ue) {
			status = ue.Status
		}
		h.Proxy.MarkFailure(ctx, key, status, err.Error())
		if !h.Proxy.Pool().IsRetryableStatus(status) {
			return err
		}
	}
	return lastErr
}

// runOneStreamAttempt executes a single attempt. It returns (nil, err) when
// the stream committed but produced an upstream-level error — the SSE channel
// already carried `response.failed` and the caller should not retry.
func (h *Responses) runOneStreamAttempt(
	ctx context.Context,
	plan proxy.ForwardPlan,
	req *responses.Request,
	family proxy.UpstreamFamily,
	key *keypool.Key,
	w http.ResponseWriter,
	r *http.Request,
) error {
	req2, err := http.NewRequestWithContext(ctx, plan.Method, h.Proxy.BaseURL()+plan.Endpoint, bytes.NewReader(plan.Body))
	if err != nil {
		return err
	}
	req2.Header.Set("Authorization", "Bearer "+key.APIKey)
	if plan.Endpoint == "/messages" {
		req2.Header.Set("x-api-key", key.APIKey)
		req2.Header.Set("anthropic-version", "2023-06-01")
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := h.Proxy.Client().Do(req2)
	if err != nil {
		return fmt.Errorf("upstream do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return &proxy.ErrUpstream{Status: resp.StatusCode, Body: body}
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Format-Converted", "responses<->"+string(family))
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	ids := responses.ResponseIDs{
		ResponseID: responses.NewResponseID(),
		MessageID:  responses.NewMessageItemID(),
	}
	conv := responses.NewStreamConverter(req, ids)

	reader := bufio.NewReaderSize(resp.Body, 16*1024)
	oaiState := responses.NewOpenAIChunkState()
	antState := responses.NewAnthropicStreamState()
	finalUsage := responses.ChatUsage{}

	for {
		if err := ctx.Err(); err != nil {
			h.Logger.Info("responses: client cancelled",
				"response_id", ids.ResponseID, "err", err)
			return err
		}
		msg, rerr := responses.ReadSSE(reader)
		if rerr != nil && rerr != io.EOF {
			h.writeFailedEvent(w, conv, "stream_read_error", rerr.Error())
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if len(msg.Data) == 0 {
			if rerr == io.EOF {
				break
			}
			continue
		}
		if responses.IsDonePayload(msg.Data) {
			break
		}

		var delta responses.ChatDelta
		switch family {
		case proxy.FamilyAnthropic:
			var payload map[string]any
			if err := json.Unmarshal(msg.Data, &payload); err != nil {
				h.writeFailedEvent(w, conv, "stream_parse_error", err.Error())
				if flusher != nil {
					flusher.Flush()
				}
				return nil
			}
			eventType := msg.Event
			if eventType == "" && payload != nil {
				if t, _ := payload["type"].(string); t != "" {
					eventType = t
				}
			}
			delta = responses.ApplyAnthropicEvent(eventType, payload, antState)
		default:
			var chunk map[string]any
			if err := json.Unmarshal(msg.Data, &chunk); err != nil {
				h.writeFailedEvent(w, conv, "stream_parse_error", err.Error())
				if flusher != nil {
					flusher.Flush()
				}
				return nil
			}
			delta = responses.ApplyOpenAIChatChunk(chunk, oaiState)
		}
		// Keep the latest usage snapshot; the converter includes it in
		// the final response.completed envelope.
		if delta.Usage.TotalTokens > 0 || delta.Usage.PromptTokens > 0 || delta.Usage.CompletionTokens > 0 {
			finalUsage = delta.Usage
		}
		evs, err := conv.Process(delta)
		if err != nil {
			h.writeFailedEvent(w, conv, "stream_parse_error", err.Error())
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if err := h.writeStreamEvents(w, evs, flusher); err != nil {
			return err
		}
		if rerr == io.EOF {
			break
		}
	}

	// Stream ended; emit the closing chain + completion.
	if finalUsage.TotalTokens == 0 {
		finalUsage = oaiState.Usage
	}
	if finalUsage.TotalTokens == 0 {
		finalUsage = antState.Usage()
	}
	closing, finalResp, err := conv.Complete(finalUsage)
	if err != nil {
		h.writeFailedEvent(w, conv, "stream_parse_error", err.Error())
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if err := h.writeStreamEvents(w, closing, flusher); err != nil {
		return err
	}

	// Persist the completed response. Phase 5 also appends to the
	// conversation chain when one was supplied.
	envelope, _ := json.Marshal(map[string]any{"request": req, "output": finalResp})
	usageJSON, _ := json.Marshal(finalResp.Usage)
	now := time.Now()
	if perr := h.Store.PutResponseState(ctx, store.ResponseStateRow{
		ID:            finalResp.ID,
		CreatedAt:     now,
		TTLAt:         now.Add(24 * time.Hour),
		Fingerprint:   finalResp.ID,
		ItemsEnvelope: envelope,
		UsageEnvelope: usageJSON,
	}); perr != nil {
		h.Logger.Warn("responses: persist state failed", "response_id", finalResp.ID, "err", perr)
	}
	if req.Conversation != nil && req.Conversation.ID != "" {
		if perr := responses.AppendConversationResponse(ctx, h.Store,
			req.Conversation.ID, req.PreviousResponseID, finalResp.ID); perr != nil {
			h.Logger.Warn("responses: conversation append failed", "err", perr)
		}
	}
	return nil
}

// writeStreamEvents marshals and flushes each Responses SSE event.
func (h *Responses) writeStreamEvents(w http.ResponseWriter, evs []responses.StreamEvent, flusher http.Flusher) error {
	for _, ev := range evs {
		b, err := responses.MarshalEvent(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, b); err != nil {
			return err
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// writeFailedEvent emits a single response.failed event after the stream was
// already committed.
func (h *Responses) writeFailedEvent(w http.ResponseWriter, conv *responses.StreamConverter, code, message string) {
	if evs, _, err := conv.Fail(code, message); err == nil {
		if err := h.writeStreamEvents(w, evs, nil); err != nil {
			h.Logger.Warn("responses: write failed event", "err", err)
		}
	}
}

// dispatch (non-stream path)
func (h *Responses) dispatch(r *http.Request, req *responses.Request) (*responses.Response, string, time.Duration, error) {
	chat, err := responses.ToChatRequest(req)
	if err != nil {
		var inv *responses.InvalidRequestError
		if errors.As(err, &inv) {
			return nil, "", 0, inv
		}
		return nil, "", 0, fmt.Errorf("convert request: %w", err)
	}
	// Resolve previous_response_id → history replay (Phase 5).
	history, _, errR := responses.ResolveHistory(r.Context(), h.Store, req)
	if errR != nil {
		return nil, "", 0, errR
	}
	responses.MergeHistory(chat, history)

	family := proxy.ModelFamily(req.Model)
	endpoint := proxy.EndpointForFamily(family)
	var upstreamBody []byte
	switch family {
	case proxy.FamilyAnthropic:
		m, err := responses.ToAnthropicRequest(chat, false)
		if err != nil {
			return nil, "", 0, err
		}
		upstreamBody, err = json.Marshal(m)
		if err != nil {
			return nil, "", 0, err
		}
	default:
		m, err := responses.ToOpenAIChatRequest(chat, false)
		if err != nil {
			return nil, "", 0, err
		}
		upstreamBody, err = json.Marshal(m)
		if err != nil {
			return nil, "", 0, err
		}
	}

	plan := proxy.ForwardPlan{
		Endpoint: endpoint,
		Method:   http.MethodPost,
		Body:     upstreamBody,
		Model:    req.Model,
		Stream:   false,
	}
	result, err := h.Proxy.Forward(r.Context(), plan, nil)
	if err != nil {
		return nil, "", 0, err
	}

	var upstream map[string]any
	if err := json.Unmarshal(result.Body, &upstream); err != nil {
		return nil, result.KeyID, 0, fmt.Errorf("decode upstream: %w", err)
	}
	var chatOut responses.ChatResponse
	switch family {
	case proxy.FamilyAnthropic:
		chatOut, err = responses.FromAnthropicResponse(upstream)
	default:
		chatOut, err = responses.FromOpenAIChatResponse(upstream)
	}
	if err != nil {
		return nil, result.KeyID, 0, err
	}

	ids := responses.ResponseIDs{
		ResponseID: responses.NewResponseID(),
		MessageID:  responses.NewMessageItemID(),
	}
	respOut := responses.FromChatResponse(req, chatOut, ids)
	envelope := buildEnvelope(req, &respOut)
	usageJSON, _ := json.Marshal(respOut.Usage)
	now := time.Now()
	if perr := h.Store.PutResponseState(r.Context(), store.ResponseStateRow{
		ID:            respOut.ID,
		CreatedAt:     now,
		TTLAt:         now.Add(24 * time.Hour),
		Fingerprint:   respOut.ID,
		ItemsEnvelope: envelope,
		UsageEnvelope: usageJSON,
	}); perr != nil {
		h.Logger.Warn("responses: persist state failed", "response_id", respOut.ID, "err", perr)
	}
	// Conversation chain append (Phase 5).
	if req.Conversation != nil && req.Conversation.ID != "" {
		if perr := responses.AppendConversationResponse(r.Context(), h.Store,
			req.Conversation.ID, req.PreviousResponseID, respOut.ID); perr != nil {
			h.Logger.Warn("responses: conversation append failed", "err", perr)
		}
	}
	return &respOut, result.KeyID, 0, nil
}

func (h *Responses) writeInvalidRequest(w http.ResponseWriter, err error) {
	var inv *responses.InvalidRequestError
	if errors.As(err, &inv) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(inv.Body())
		return
	}
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

func (h *Responses) writeUpstreamError(ctx context.Context, w http.ResponseWriter, model string, start time.Time, err error) {
	status := http.StatusBadGateway
	var ue *proxy.ErrUpstream
	if errors.As(err, &ue) {
		status = ue.Status
	}
	var inv *responses.InvalidRequestError
	if errors.As(err, &inv) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(inv.Body())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(errorBody(err))
	h.Logger.Warn("responses upstream error", "model", model, "status", status, "err", err)
	if h.Store != nil {
		_ = h.Store.LogRequest(ctx, store.LogRow{
			Timestamp: time.Now(),
			Model:     model,
			Status:    status,
			LatencyMs: time.Since(start).Milliseconds(),
			Error:     err.Error(),
		})
	}
}

func buildEnvelope(req *responses.Request, resp *responses.Response) json.RawMessage {
	out, _ := json.Marshal(map[string]any{"request": req, "output": resp})
	return out
}

func tokens(d *responses.InputTokensDetails) int64 {
	if d == nil {
		return 0
	}
	return d.CachedTokens
}
