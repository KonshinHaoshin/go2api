package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToOpenAIChatRequest converts the canonical ChatRequest into an OpenAI
// Chat Completions wire body (the shape posted to upstream
// /chat/completions). Function tools are reshaped into the OpenAI nested
// {type=function, function={...}} form, and reasoning/text fields are
// passed through as-is; the upstream model decides whether they apply.
//
// `stream` is set explicitly so the upstream knows to emit SSE.
func ToOpenAIChatRequest(req *ChatRequest, stream bool) (map[string]any, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	out := map[string]any{
		"model":  strings.TrimPrefix(req.Model, "opencode-go/"),
		"stream": stream,
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if len(req.Stop) > 0 {
		out["stop"] = req.Stop
	}

	// Reasoning is provider-specific. The kimi / deepseek family has not
	// adopted the OpenAI reasoning_effort field, but we pass it through if
	// set so an experimental upstream may honor it.
	if req.Reasoning != nil {
		out["reasoning_effort"] = req.Reasoning.Effort
	}

	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "", "text":
			out["response_format"] = map[string]any{"type": "text"}
		case "json_schema":
			rf := map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   derefString(req.ResponseFormat.Name, "response"),
					"schema": json.RawMessage(req.ResponseFormat.Schema),
				},
			}
			if req.ResponseFormat.Strict != nil {
				rf["json_schema"].(map[string]any)["strict"] = *req.ResponseFormat.Strict
			}
			out["response_format"] = rf
		}
	}

	if len(req.Tools) > 0 {
		var arr []any
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			fn := map[string]any{
				"name": t.Name,
			}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				fn["parameters"] = json.RawMessage(t.Parameters)
			}
			if t.Strict {
				fn["strict"] = true
			}
			arr = append(arr, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = arr
	}
	if req.ToolChoice != nil {
		out["tool_choice"] = req.ToolChoice
	}

	var msgs []any
	for _, m := range req.Messages {
		om := convertMessageToOpenAI(m)
		if om != nil {
			msgs = append(msgs, om)
		}
	}
	out["messages"] = msgs

	return out, nil
}

func convertMessageToOpenAI(m ChatMessage) any {
	// Tool-result message.
	if m.Role == "tool" {
		return map[string]any{
			"role":         "tool",
			"content":      m.Content,
			"tool_call_id": m.ToolCallID,
		}
	}

	out := map[string]any{"role": m.Role}
	if len(m.ContentParts) > 0 {
		parts := make([]any, 0, len(m.ContentParts))
		for _, p := range m.ContentParts {
			switch p.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			case "image_url":
				iu := map[string]any{"url": p.ImageURL}
				if p.Detail != "" {
					iu["detail"] = p.Detail
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": iu})
			}
		}
		out["content"] = parts
	} else if m.Content != "" {
		out["content"] = m.Content
	}
	if len(m.ToolCalls) > 0 {
		var arr []any
		for _, tc := range m.ToolCalls {
			arr = append(arr, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Function,
					"arguments": tc.Arguments,
				},
			})
		}
		out["tool_calls"] = arr
	}
	return out
}

// ChatDelta is one normalized upstream stream delta. Provider adapters
// (OpenAI/Anthropic) produce one of these per upstream SSE event; the
// StreamConverter consumes them to emit Responses API events.
type ChatDelta struct {
	ContentDelta     string             // additional text fragment to append
	ReasoningDelta   string             // raw reasoning/thinking fragment
	ReasoningSummaryDelta string         // summary-style reasoning fragment (when upstream exposes it)
	ToolCalls        []ChatToolCall     // complete (id+name+args) tool calls surfacing in this delta
	ToolCallFragments []ChatToolCallFragment // partial tool-call id/name/args fragments (streaming tool calls)
	FinishReason string                 // filled only on the final delta
	Usage        ChatUsage              // populated on the message_delta / final delta
}

// ChatToolCallFragment is one streaming tool-call delta. Index is the
// tool_calls array index the upstream assigned.
type ChatToolCallFragment struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// OpenAIChunkState accumulates per-stream context across OpenAI Chat
// Completions chunks. One instance per /v1/responses call.
type OpenAIChunkState struct {
	Model        string
	FinishReason string
	Usage        ChatUsage
}

// NewOpenAIChunkState returns a zero-value state object.
func NewOpenAIChunkState() *OpenAIChunkState { return &OpenAIChunkState{} }

// ApplyOpenAIChatChunk normalizes one OpenAI Chat Completions chunk into a
// canonical ChatDelta. Multiple chunks between finish_reason and the trailing
// [DONE] are typical; this function tolerates empty choice sets and refreshes
// the model name in the caller's state object.
//
// `state` accumulates the model and a final-usage snapshot across chunks.
func ApplyOpenAIChatChunk(chunk map[string]any, state *OpenAIChunkState) ChatDelta {
	if state == nil {
		state = &OpenAIChunkState{}
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		state.Model = model
	}
	var delta ChatDelta

	if u, ok := chunk["usage"].(map[string]any); ok {
		state.Usage = usageFromOpenAI(u)
		delta.Usage = state.Usage
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return delta
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return delta
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" && fr != "null" {
		state.FinishReason = fr
		delta.FinishReason = fr
	}
	d, _ := c["delta"].(map[string]any)
	if d == nil {
		return delta
	}
	if t, ok := d["content"].(string); ok && t != "" {
		delta.ContentDelta = t
	}
	if r, ok := d["reasoning"].(string); ok && r != "" {
		delta.ReasoningDelta = r
	}
	if rs, ok := d["reasoning_summary"].(string); ok && rs != "" {
		delta.ReasoningSummaryDelta = rs
	}
	if tcs, ok := d["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tm, _ := raw.(map[string]any)
			if tm == nil {
				continue
			}
			idx := toInt64(tm["index"])
			fn, _ := tm["function"].(map[string]any)
			frag := ChatToolCallFragment{
				Index: int(idx),
				ID:    asString(tm["id"]),
				Name:  asString(fn["name"]),
			}
			if a, ok := fn["arguments"].(string); ok {
				frag.Arguments = a
			}
			delta.ToolCallFragments = append(delta.ToolCallFragments, frag)
		}
	}
	return delta
}

// FromOpenAIChatResponse converts a non-streaming Chat Completions response
// into a canonical ChatResponse. Usage, content, and tool_calls are pulled
// out of the response body; finish_reason maps directly through.
func FromOpenAIChatResponse(body map[string]any) (ChatResponse, error) {
	out := ChatResponse{}
	if id, ok := body["id"].(string); ok {
		out.ID = id
	}
	if model, ok := body["model"].(string); ok {
		out.Model = model
	}
	if choices, _ := body["choices"].([]any); len(choices) > 0 {
		c, _ := choices[0].(map[string]any)
		if c != nil {
			if fr, ok := c["finish_reason"].(string); ok {
				out.FinishReason = fr
			}
			if msg, ok := c["message"].(map[string]any); ok {
				if txt, ok := msg["content"].(string); ok {
					out.Content = txt
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tm, _ := raw.(map[string]any)
						if tm == nil {
							continue
						}
						fn, _ := tm["function"].(map[string]any)
						out.ToolCalls = append(out.ToolCalls, ChatToolCall{
							ID:        asString(tm["id"]),
							Type:      asString(tm["type"]),
							Function:  asString(fn["name"]),
							Arguments: asString(fn["arguments"]),
						})
					}
				}
				if r, ok := msg["reasoning"].(string); ok && r != "" {
					out.Reasoning = r
				}
			}
		}
	}
	if u, ok := body["usage"].(map[string]any); ok {
		out.Usage = usageFromOpenAI(u)
	}
	return out, nil
}

func usageFromOpenAI(u map[string]any) ChatUsage {
	out := ChatUsage{}
	out.PromptTokens = toInt64(u["prompt_tokens"])
	out.CompletionTokens = toInt64(u["completion_tokens"])
	out.TotalTokens = toInt64(u["total_tokens"])
	if out.TotalTokens == 0 && (out.PromptTokens > 0 || out.CompletionTokens > 0) {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
		out.CachedTokens = toInt64(det["cached_tokens"])
	}
	if det, ok := u["completion_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = toInt64(det["reasoning_tokens"])
	}
	return out
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case json.RawMessage:
		return string(s)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func derefString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
