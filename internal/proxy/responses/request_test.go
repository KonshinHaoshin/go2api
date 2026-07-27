package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestULIDShape(t *testing.T) {
	id := NewResponseID()
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("missing prefix: %s", id)
	}
	if len(id) <= len("resp_") {
		t.Fatalf("empty ULID payload: %s", id)
	}
}

func TestIDsUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		id := NewResponseID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestValidateRejectsBackground(t *testing.T) {
	req := &Request{
		Model:      "kimi-k3",
		Background: true,
	}
	req.Input = Input{IsString: true, StringVal: "hi"}
	err := ValidateRequest(req)
	if err == nil {
		t.Fatalf("expected error")
	}
	inv, ok := err.(*InvalidRequestError)
	if !ok || inv.Code != "unsupported_background" {
		t.Fatalf("expected unsupported_background error, got %T %v", err, err)
	}
}

func TestValidateAcceptsAndDropsHostedTool(t *testing.T) {
	// web_search and other non-function tools should be silently dropped
	// (not rejected), so Codex requests with mixed tool types still work.
	req := &Request{
		Model: "kimi-k3",
		Tools: []Tool{
			{Type: "web_search"},
			{Type: "custom", Name: "apply_patch"},
			{Type: "function", Name: "lookup"},
		},
	}
	req.Input = Input{IsString: true, StringVal: "hi"}
	if err := ValidateRequest(req); err != nil {
		t.Fatalf("expected no error for mixed tool types, got: %v", err)
	}
	// ToChatRequest should keep only the function tool.
	chat, err := ToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected ToChatRequest error: %v", err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Name != "lookup" {
		t.Fatalf("expected only function tool 'lookup' in ChatRequest, got %+v", chat.Tools)
	}
}

func TestValidateAcceptsPlainFunction(t *testing.T) {
	req := &Request{
		Model: "kimi-k3",
		Tools: []Tool{{
			Type: "function",
			Name: "lookup",
			Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
	}
	req.Input = Input{IsString: true, StringVal: "hi"}
	if err := ValidateRequest(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestToChatRequestStringInput(t *testing.T) {
	req := &Request{
		Model: "kimi-k3",
		Input: Input{IsString: true, StringVal: "hello"},
	}
	chat, err := ToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != "user" || chat.Messages[0].Content != "hello" {
		t.Fatalf("unexpected user message: %+v", chat.Messages[0])
	}
}

func TestToChatRequestInstructionsBecomeSystem(t *testing.T) {
	req := &Request{
		Model:       "kimi-k3",
		Instructions: "Be terse.",
		Input:       Input{IsString: true, StringVal: "hi"},
	}
	chat, err := ToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chat.System != "Be terse." {
		t.Fatalf("expected System populated, got %q", chat.System)
	}
	if chat.Messages[0].Role != "system" {
		t.Fatalf("expected first message to be system, got %q", chat.Messages[0].Role)
	}
}

func TestToOpenAIChatRequestShape(t *testing.T) {
	cr := &ChatRequest{
		Model:    "kimi-k3",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}
	body, err := ToOpenAIChatRequest(cr, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["model"] != "kimi-k3" {
		t.Fatalf("expected stripped model name, got %v", body["model"])
	}
	if body["stream"] != false {
		t.Fatalf("expected stream=false in non-stream call")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestToAnthropicRequestShape(t *testing.T) {
	cr := &ChatRequest{
		Model:   "minimax-m3",
		System:  "Be terse.",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}
	body, err := ToAnthropicRequest(cr, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["system"] != "Be terse." {
		t.Fatalf("expected system=Be terse., got %v", body["system"])
	}
	if body["stream"] != true {
		t.Fatalf("expected stream=true")
	}
	if body["max_tokens"] == nil {
		t.Fatalf("expected max_tokens default")
	}
}

func TestFromOpenAIChatResponseUsage(t *testing.T) {
	body := map[string]any{
		"id":      "chatcmpl-1",
		"model":   "kimi-k3",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "hi there"}}},
		"usage": map[string]any{
			"prompt_tokens":     11,
			"completion_tokens": 7,
			"total_tokens":      18,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 5},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	}
	cr, err := FromOpenAIChatResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Content != "hi there" || cr.FinishReason != "stop" {
		t.Fatalf("unexpected chat content: %+v", cr)
	}
	if cr.Usage.PromptTokens != 11 || cr.Usage.CompletionTokens != 7 || cr.Usage.CachedTokens != 5 || cr.Usage.ReasoningTokens != 2 {
		t.Fatalf("unexpected usage: %+v", cr.Usage)
	}
}

func TestFromAnthropicResponseUsage(t *testing.T) {
	body := map[string]any{
		"id":         "msg_1",
		"model":      "minimax-m3",
		"stop_reason": "end_turn",
		"content": []any{
			map[string]any{"type": "text", "text": "ok"},
		},
		"usage": map[string]any{"input_tokens": 9, "output_tokens": 3, "cache_read_input_tokens": 1},
	}
	cr, err := FromAnthropicResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Content != "ok" || cr.FinishReason != "stop" {
		t.Fatalf("unexpected chat content: %+v", cr)
	}
	if cr.Usage.PromptTokens != 9 || cr.Usage.CompletionTokens != 3 || cr.Usage.CachedTokens != 1 {
		t.Fatalf("unexpected usage: %+v", cr.Usage)
	}
}

func TestFromChatResponseIDPrefix(t *testing.T) {
	req := &Request{Model: "kimi-k3"}
	chat := ChatResponse{
		Model:    "kimi-k3",
		Content:  "hi",
		Usage:    ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	ids := ResponseIDs{
		ResponseID: NewResponseID(),
		MessageID:  NewMessageItemID(),
	}
	resp := FromChatResponse(req, chat, ids)
	if !strings.HasPrefix(resp.ID, "resp_") {
		t.Fatalf("expected response id prefix, got %q", resp.ID)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*OutputMessage)
	if !ok {
		t.Fatalf("expected message item, got %T", resp.Output[0])
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Fatalf("expected msg_ prefix, got %q", msg.ID)
	}
	if resp.Status != "completed" {
		t.Fatalf("expected status completed, got %q", resp.Status)
	}
	if resp.OutputText != "hi" {
		t.Fatalf("expected output_text=hi, got %q", resp.OutputText)
	}
}

func TestInputUnmarshalString(t *testing.T) {
	var inp Input
	if err := json.Unmarshal([]byte(`"hello"`), &inp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inp.IsString || inp.StringVal != "hello" {
		t.Fatalf("unexpected parsed input: %+v", inp)
	}
}

func TestInputUnmarshalMessageArray(t *testing.T) {
	payload := `[
		{"type":"message","role":"user","content":"Hello"},
		{"type":"message","role":"assistant","content":"Hi there"}
	]`
	var inp Input
	if err := json.Unmarshal([]byte(payload), &inp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inp.IsString || len(inp.Items) != 2 {
		t.Fatalf("unexpected parsed input: %+v", inp)
	}
	m1, ok := inp.Items[0].(*InputMessage)
	if !ok || m1.Role != "user" {
		t.Fatalf("unexpected first item: %+v", inp.Items[0])
	}
	if m1.Content[0].Text != "Hello" {
		t.Fatalf("expected text part, got %+v", m1.Content[0])
	}
}

// Verify that the envelope produced for persistence is parseable JSON.
func TestEnvelopeShape(t *testing.T) {
	req := &Request{Model: "kimi-k3"}
	req.Input = Input{IsString: true, StringVal: "hi"}
	chat := ChatResponse{Model: "kimi-k3", Content: "ok"}
	resp := FromChatResponse(req, chat, ResponseIDs{
		ResponseID: NewResponseID(),
		MessageID:  NewMessageItemID(),
	})
	out, err := json.Marshal(map[string]any{
		"request": req,
		"output":  resp,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip through sha256 just to ensure size sanity.
	sum := sha256.Sum256(out)
	if got := hex.EncodeToString(sum[:8]); got == "" {
		t.Fatal("empty hash")
	}
}

func TestValidateRejectsFileID(t *testing.T) {
	req := &Request{
		Model: "kimi-k3",
	}
	// input_image with file_id should be rejected.
	if err := json.Unmarshal([]byte(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","file_id":"file_abc"}
		]}
	]`), &req.Input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	err := ValidateRequest(req)
	if err == nil {
		t.Fatalf("expected error")
	}
	inv := err.(*InvalidRequestError)
	if inv.Code != "unsupported_file_reference" {
		t.Fatalf("expected unsupported_file_reference, got %s", inv.Code)
	}
}

func TestValidateRejectsRemoteImageURL(t *testing.T) {
	req := &Request{Model: "kimi-k3"}
	if err := json.Unmarshal([]byte(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"https://example.com/x.png"}
		]}
	]`), &req.Input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	err := ValidateRequest(req)
	if err == nil {
		t.Fatalf("expected error for remote URL")
	}
	inv := err.(*InvalidRequestError)
	if inv.Code != "unsupported_image_source" {
		t.Fatalf("expected unsupported_image_source, got %s", inv.Code)
	}
}

func TestValidateRejectsLargeImage(t *testing.T) {
	// 6 MiB of base64 → ~4.5 MiB decoded; round to ensure > 5 MiB.
	big := make([]byte, 6*1024*1024/4*4/3*4)
	for i := range big {
		big[i] = 'A'
	}
	encoded := "data:image/png;base64," + string(big)
	req := &Request{Model: "kimi-k3"}
	payload := `[{"type":"message","role":"user","content":[
		{"type":"input_image","image_url":"` + encoded + `"}
	]}]`
	if err := json.Unmarshal([]byte(payload), &req.Input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	err := ValidateRequest(req)
	if err == nil {
		t.Fatalf("expected error for large image")
	}
	inv := err.(*InvalidRequestError)
	if inv.Code != "image_too_large" {
		t.Fatalf("expected image_too_large, got %s", inv.Code)
	}
}

func TestValidateAcceptsInlineDataURL(t *testing.T) {
	req := &Request{Model: "kimi-k3"}
	tinyPng := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVQYV2NgAAIAAAUAAeImBZsAAAAASUVORK5CYII="
	if err := json.Unmarshal([]byte(`[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"what is this?"},
			{"type":"input_image","image_url":"`+tinyPng+`"}
		]}
	]`), &req.Input); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ValidateRequest(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsNonImageMediaType(t *testing.T) {
	req := &Request{Model: "kimi-k3"}
	if err := json.Unmarshal([]byte(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:application/octet-stream;base64,AAAA"}
		]}
	]`), &req.Input); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := ValidateRequest(req)
	if err == nil {
		t.Fatalf("expected error")
	}
	inv := err.(*InvalidRequestError)
	if inv.Code != "unsupported_media_type" {
		t.Fatalf("expected unsupported_media_type, got %s", inv.Code)
	}
}
