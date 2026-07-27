package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/user/go2api/internal/config"
	"github.com/user/go2api/internal/keypool"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/proxy/responses"
	"github.com/user/go2api/internal/store"
)

// --- shared helpers --------------------------------------------------------

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return db
}

func mustPool(t *testing.T, db *store.DB, logger *slog.Logger) *keypool.Pool {
	t.Helper()
	pool, err := keypool.New(context.Background(), config.KeyPoolConfig{
		Strategy: "round_robin",
		Keys: []config.KeyConfig{{
			ID: "k1", Label: "test", APIKey: "test-key", Weight: 1,
		}},
	}, db, logger)
	if err != nil {
		t.Fatalf("keypool: %v", err)
	}
	return pool
}

func runHandler(t *testing.T, h *Responses, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer smoke-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeSSE(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var event, data string
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "" && event != "" && data != "":
			var obj map[string]any
			if err := json.Unmarshal([]byte(data), &obj); err == nil {
				events = append(events, map[string]any{"event": event, "data": obj})
			}
			event, data = "", ""
		}
	}
	return events
}

// --- OpenAI streaming fake upstream ----------------------------------------

func streamingFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "only /chat/completions supported", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		isStream, _ := req["stream"].(bool)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		emit := func(s string) {
			fmt.Fprint(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if !isStream {
			emit(`{"id":"x","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		text := "echo:hi"
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n")
		for _, ch := range text {
			emit(fmt.Sprintf(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"content":"%c"},"finish_reason":null}]}`+"\n\n", ch))
		}
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":15,"total_tokens":16}}` + "\n\n")
		emit("data: [DONE]\n\n")
	}))
}

// --- Anthropic streaming fake upstream --------------------------------------

func streamingAnthropicFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "only /messages supported", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		emit := func(s string) {
			fmt.Fprint(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		emit("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"minimax-m3\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		emit("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		for _, ch := range "anthropic-hi" {
			emit(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"%c\"}}\n\n", ch))
		}
		emit("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		emit("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":12}}\n\n")
		emit("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
}

// --- Phase 2 tests ----------------------------------------------------------

func TestResponsesStream_OpenAI(t *testing.T) {
	upstream := streamingFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}

	body, _ := json.Marshal(map[string]any{"model": "kimi-k3", "input": "hi", "stream": true})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s headers=%+v", rr.Code, rr.Body.String(), rr.Header())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected SSE Content-Type, got %s", ct)
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("downstream stream contains [DONE]")
	}
	events := decodeSSE(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events decoded, raw=%s", rr.Body.String())
	}
	seen := map[string]int{}
	for _, e := range events {
		seen[e["event"].(string)]++
	}
	if seen["response.created"] == 0 || seen["response.in_progress"] == 0 {
		t.Fatalf("missing created/in_progress events: %+v", seen)
	}
	if seen["response.completed"] != 1 {
		t.Fatalf("expected exactly 1 response.completed, got %d (all=%+v)", seen["response.completed"], seen)
	}
	if seen["response.output_text.delta"] == 0 {
		t.Fatalf("expected at least one output_text.delta, got 0 (all=%+v)", seen)
	}
	if seen["response.output_text.done"] == 0 {
		t.Fatalf("expected output_text.done (all=%+v)", seen)
	}
	lastSeq := int64(-1)
	for _, e := range events {
		obj := e["data"].(map[string]any)
		if v, ok := obj["sequence_number"]; ok {
			s := int64(v.(float64))
			if s <= lastSeq {
				t.Fatalf("non-increasing sequence number: %d after %d", s, lastSeq)
			}
			lastSeq = s
		}
	}
	var last map[string]any
	for _, e := range events {
		if e["event"] == "response.completed" {
			last = e["data"].(map[string]any)
		}
	}
	respObj := last["response"].(map[string]any)
	if respObj["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", respObj["status"])
	}
	if !strings.HasPrefix(respObj["id"].(string), "resp_") {
		t.Fatalf("expected resp_ id, got %v", respObj["id"])
	}
}

func TestResponsesStream_Anthropic(t *testing.T) {
	upstream := streamingAnthropicFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}
	body, _ := json.Marshal(map[string]any{"model": "minimax-m3", "input": "hi", "stream": true})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("downstream contains [DONE]")
	}
	events := decodeSSE(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatalf("no events")
	}
	seen := map[string]int{}
	for _, e := range events {
		seen[e["event"].(string)]++
	}
	if seen["response.completed"] != 1 {
		t.Fatalf("expected 1 response.completed, got %d", seen["response.completed"])
	}
	if seen["response.output_text.delta"] == 0 {
		t.Fatalf("expected output_text.delta events")
	}
}

// --- Phase 3: function tool calls -------------------------------------------

func streamToolsFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		emit := func(s string) { fmt.Fprint(w, s); w.(http.Flusher).Flush() }
		const chunk = `{"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"weather in Tokyo\"}"}}]},"finish_reason":null}]}`
		const finish = `{"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":9,"total_tokens":20}}`
		emit("data: " + chunk + "\n\n")
		emit("data: " + finish + "\n\n")
		emit("data: [DONE]\n\n")
	}))
}

func TestResponsesStream_FunctionCall(t *testing.T) {
	upstream := streamToolsFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}
	body, _ := json.Marshal(map[string]any{
		"model":      "kimi-k3",
		"input":      "what is the weather in tokyo?",
		"stream":     true,
		"tool_choice": "auto",
		"tools": []any{
			map[string]any{
				"type":       "function",
				"name":       "lookup",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
			},
		},
	})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	bodyStr := rr.Body.String()
	if strings.Contains(bodyStr, "[DONE]") {
		t.Fatalf("downstream contains [DONE]: %s", bodyStr[:min(800, len(bodyStr))])
	}
	events := decodeSSE(t, bodyStr)
	seen := map[string]int{}
	functionCallSeen := false
	for _, e := range events {
		seen[e["event"].(string)]++
		if e["event"] == "response.output_item.added" {
			obj := e["data"].(map[string]any)
			if item, ok := obj["item"].(map[string]any); ok && item["type"] == "function_call" {
				functionCallSeen = true
			}
		}
	}
	if seen["response.completed"] != 1 {
		t.Fatalf("expected 1 response.completed, got %d (events=%+v body=%s)", seen["response.completed"], seen, bodyStr[:min(1500, len(bodyStr))])
	}
	if seen["response.function_call_arguments.done"] != 1 {
		t.Fatalf("expected function_call_arguments.done, got %d (events=%+v body=%s)", seen["response.function_call_arguments.done"], seen, bodyStr[:min(1500, len(bodyStr))])
	}
	if !functionCallSeen {
		t.Fatalf("expected function_call output_item.added (events=%+v body=%s)", seen, bodyStr[:min(1500, len(bodyStr))])
	}
	lastSeq := int64(-1)
	for _, e := range events {
		obj := e["data"].(map[string]any)
		if v, ok := obj["sequence_number"]; ok {
			s := int64(v.(float64))
			if s <= lastSeq {
				t.Fatalf("non-increasing sequence: %d after %d", s, lastSeq)
			}
			lastSeq = s
		}
	}
}

// --- Phase 3 add-ons: reasoning + non-stream tool round-trip ----------------

func streamReasoningFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		emit := func(s string) { fmt.Fprint(w, s); w.(http.Flusher).Flush() }
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"reasoning":"thinking about this..."},"finish_reason":null}]}` + "\n\n")
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"content":"the answer is 42"},"finish_reason":null}]}` + "\n\n")
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}` + "\n\n")
		emit("data: [DONE]\n\n")
	}))
}

func TestResponsesStream_Reasoning(t *testing.T) {
	upstream := streamReasoningFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}
	body, _ := json.Marshal(map[string]any{"model": "kimi-k3", "input": "hi", "stream": true})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	events := decodeSSE(t, rr.Body.String())
	seen := map[string]int{}
	for _, e := range events {
		seen[e["event"].(string)]++
	}
	if seen["response.reasoning_text.delta"] == 0 {
		t.Fatalf("expected at least one reasoning_text.delta, got %+v", seen)
	}
	if seen["response.output_text.delta"] == 0 {
		t.Fatalf("expected at least one output_text.delta alongside reasoning")
	}
}

// nonStreamToolsFakeUpstream emits a complete Chat Completions response
// carrying a tool_call in message.tool_calls.
func nonStreamToolsFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_xyz","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
}

func TestResponsesNonStream_FunctionCall(t *testing.T) {
	upstream := nonStreamToolsFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}
	body, _ := json.Marshal(map[string]any{
		"model":  "kimi-k3",
		"input":  "weather?",
		"stream": false,
		"tools": []any{
			map[string]any{"type": "function", "name": "lookup"},
		},
	})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(output))
	}
	fc, _ := output[0].(map[string]any)
	if fc["type"] != "function_call" {
		t.Fatalf("expected function_call item, got %v", fc["type"])
	}
	if fc["name"] != "lookup" {
		t.Fatalf("expected name=lookup, got %v", fc["name"])
	}
	if fc["status"] != "completed" {
		t.Fatalf("expected status completed, got %v", fc["status"])
	}
}

// --- Phase 5: previous_response_id + conversation ------------------------

// repeatingFakeUpstream echoes back the conversation it received, so we can
// assert history replay by inspecting the upstream request body.
func repeatingFakeUpstream(t *testing.T, captured *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

func TestResponsesNonStream_PreviousResponseID_ReplaysHistory(t *testing.T) {
	var captured string
	upstream := repeatingFakeUpstream(t, &captured)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}
	t.Logf("first call starting")
	body1, _ := json.Marshal(map[string]any{"model": "kimi-k3", "input": "what is my name?"})
	rr1 := runHandler(t, h, body1)
	t.Logf("first call returned code=%d", rr1.Code)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request failed: %s", rr1.Body.String())
	}
	var resp1 map[string]any
	_ = json.Unmarshal(rr1.Body.Bytes(), &resp1)
	rid, _ := resp1["id"].(string)
	if !strings.HasPrefix(rid, "resp_") {
		t.Fatalf("expected resp_ id, got %v", resp1["id"])
	}

	// Seed a previous turn into state so the next request has something to
	// replay. (The first call already wrote a row, but with an empty output
	// envelope — overwrite it with a richer one.)
	now := time.Now()
	prev := &responses.Response{
		Object: "response", ID: rid, CreatedAt: now.Unix(),
		Status: "completed", Model: "kimi-k3",
		Output: []responses.OutputItem{
			&responses.OutputMessage{
				Type:    "message",
				ID:      responses.NewMessageItemID(),
				Role:    "assistant",
				Status:  "completed",
				Content: []responses.OutputContent{{Type: "output_text", Text: "Your name is Ada."}},
			},
		},
	}
	envelope, _ := json.Marshal(map[string]any{"output": prev})
	if err := db.PutResponseState(context.Background(), store.ResponseStateRow{
		ID: rid, CreatedAt: now, TTLAt: now.Add(24 * time.Hour),
		Fingerprint: rid, ItemsEnvelope: envelope,
		UsageEnvelope: []byte(`{"input_tokens":3,"output_tokens":3,"total_tokens":6}`),
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// Second request: the captured upstream body should contain the prior assistant text.
	body2, _ := json.Marshal(map[string]any{
		"model":                "kimi-k3",
		"input":                "thanks",
		"previous_response_id": rid,
	})
	captured = "" // reset
	rr2 := runHandler(t, h, body2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second request failed: %d %s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(captured, "Your name is Ada.") {
		t.Fatalf("expected upstream body to contain replayed history, got: %s", captured)
	}
	if !strings.Contains(captured, "thanks") {
		t.Fatalf("expected upstream body to contain new user message, got: %s", captured)
	}
}

func TestResponsesNonStream_ConversationChain(t *testing.T) {
	upstream := repeatingFakeUpstream(t, new(string))
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}

	convID := "conv_" + responses.NewConversationID()
	body1, _ := json.Marshal(map[string]any{
		"model": "kimi-k3", "input": "first",
		"conversation": map[string]any{"id": convID},
	})
	rr1 := runHandler(t, h, body1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request failed: %s", rr1.Body.String())
	}
	var resp1 map[string]any
	_ = json.Unmarshal(rr1.Body.Bytes(), &resp1)
	rid1, _ := resp1["id"].(string)

	body2, _ := json.Marshal(map[string]any{
		"model": "kimi-k3", "input": "second",
		"conversation":       map[string]any{"id": convID},
		"previous_response_id": rid1,
	})
	rr2 := runHandler(t, h, body2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second request failed: %d %s", rr2.Code, rr2.Body.String())
	}
	// Confirm chain has both ids.
	conv, err := db.GetConversation(context.Background(), convID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if len(conv.ResponseIDs) != 2 {
		t.Fatalf("expected 2 ids in chain, got %d", len(conv.ResponseIDs))
	}
	if conv.LastResponseID != conv.ResponseIDs[1] {
		t.Fatalf("expected last == chain tail, got %q", conv.LastResponseID)
	}
}

func TestResponsesNonStream_ConversationChain_Conflict(t *testing.T) {
	upstream := repeatingFakeUpstream(t, new(string))
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}

	convID := "conv_" + responses.NewConversationID()
	body1, _ := json.Marshal(map[string]any{
		"model": "kimi-k3", "input": "first",
		"conversation": map[string]any{"id": convID},
	})
	runHandler(t, h, body1) // first call seeds the chain.

	// Second call: conversation matches but previous_response_id is bogus.
	body2, _ := json.Marshal(map[string]any{
		"model": "kimi-k3", "input": "second",
		"conversation":         map[string]any{"id": convID},
		"previous_response_id": "resp_bogus",
	})
	rr2 := runHandler(t, h, body2)
	if rr2.Code == http.StatusOK {
		t.Fatalf("expected non-200 for chain conflict, got 200")
	}
}

// --- Phase 6: error fidelity + spec conformance ----------------------------

func streamMidFailFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		emit := func(s string) { fmt.Fprint(w, s); w.(http.Flusher).Flush() }
		// Send one text delta, then drop the connection without [DONE].
		emit(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"kimi-k3","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n")
		// Close without [DONE] → handler detects premature EOF.
	}))
}

func TestResponsesStream_MidStreamFailure(t *testing.T) {
	upstream := streamMidFailFakeUpstream(t)
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}

	body, _ := json.Marshal(map[string]any{"model": "kimi-k3", "input": "hi", "stream": true})
	rr := runHandler(t, h, body)

	// HTTP 200 because headers were committed.
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("downstream contains [DONE]")
	}
	events := decodeSSE(t, rr.Body.String())
	seen := map[string]int{}
	for _, e := range events {
		seen[e["event"].(string)]++
	}
	// Exactly one terminal event: either completed (if EOF after content is
	// treated as normal completion) or failed (if treated as error).
	terminal := seen["response.completed"] + seen["response.failed"]
	if terminal != 1 {
		t.Fatalf("expected exactly 1 terminal event, got completed=%d failed=%d (all=%+v)",
			seen["response.completed"], seen["response.failed"], seen)
	}
	// Must have seen the partial text delta.
	if seen["response.output_text.delta"] == 0 {
		t.Fatalf("expected output_text.delta, got %+v", seen)
	}
}

func TestResponsesNonStream_PreStreamError_Returns4xx(t *testing.T) {
	upstream := repeatingFakeUpstream(t, new(string))
	defer upstream.Close()
	logger := newDiscardLogger()
	up := proxy.New(upstream.URL, 30*time.Second)
	db := mustStore(t)
	defer db.Close()
	pool := mustPool(t, db, logger)
	prx := proxy.NewProxy(up, pool, logger)
	h := &Responses{Proxy: prx, Store: db, Logger: logger}

	// background=true should return HTTP 400 before forwarding.
	body, _ := json.Marshal(map[string]any{
		"model":      "kimi-k3",
		"input":      "hi",
		"background": true,
	})
	rr := runHandler(t, h, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	var errResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("expected JSON error envelope, got: %s", rr.Body.String())
	}
	errObj, ok := errResp["error"].(map[string]any)
	if !ok || errObj["type"] != "invalid_request_error" {
		t.Fatalf("expected invalid_request_error envelope, got %+v", errResp)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
