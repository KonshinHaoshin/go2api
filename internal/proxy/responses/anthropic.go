package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToAnthropicRequest converts the canonical ChatRequest into an Anthropic
// Messages wire body (the shape posted to upstream /messages). The system
// prompt comes from req.System (populated upstream by ToChatRequest when the
// Responses request used `instructions` or had a system-role message).
//
// max_tokens is required by Anthropic; we default to 4096 if the client did
// not set one.
func ToAnthropicRequest(req *ChatRequest, stream bool) (map[string]any, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	out := map[string]any{
		"model":  strings.TrimPrefix(req.Model, "opencode-go/"),
		"stream": stream,
	}

	if req.System != "" {
		out["system"] = req.System
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	} else {
		out["max_tokens"] = 4096
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}

	if len(req.Tools) > 0 {
		var arr []any
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			tool := map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": json.RawMessage(t.Parameters),
			}
			arr = append(arr, tool)
		}
		out["tools"] = arr
	}
	if req.ToolChoice != nil {
		// Convert common shapes; unsupported choices pass through and may
		// 400 upstream.
		switch v := req.ToolChoice.(type) {
		case string:
			switch v {
			case "auto", "any", "none":
				out["tool_choice"] = map[string]any{"type": v}
			default:
				out["tool_choice"] = map[string]any{"type": "tool", "name": v}
			}
		default:
			out["tool_choice"] = req.ToolChoice
		}
	}

	var messages []any
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue // already in top-level "system" field
		}
		am := convertMessageToAnthropic(m)
		if am != nil {
			messages = append(messages, am)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages array must contain at least one user message")
	}
	out["messages"] = messages

	return out, nil
}

func convertMessageToAnthropic(m ChatMessage) any {
	switch m.Role {
	case "user":
		return anthropicFromUserOrToolResult(m)
	case "assistant":
		return anthropicFromAssistant(m)
	case "tool":
		// We normally merge tool results into the user turn, but tolerate a
		// standalone tool message by re-emitting it as a user tool_result.
		return anthropicFromUserOrToolResult(ChatMessage{
			Role:    "user",
			Content: m.Content,
			ContentParts: []ChatContentPart{{
				Type:       "tool_result",
				ToolUseID:  m.ToolCallID,
				ToolCallID: m.ToolCallID,
				Content:    m.Content,
			}},
		})
	}
	if m.Content != "" {
		return map[string]any{"role": m.Role, "content": m.Content}
	}
	return nil
}

func anthropicFromUserOrToolResult(m ChatMessage) any {
	if len(m.ContentParts) == 0 {
		if m.Content == "" {
			return nil
		}
		return map[string]any{"role": "user", "content": m.Content}
	}
	var blocks []any
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			// Best-effort handling of data URLs only; arbitrary remote URLs
			// are not fetched by the proxy to avoid SSRF surface.
			media, data, ok := splitDataURL(p.ImageURL)
			if !ok {
				continue
			}
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": media,
					"data":       data,
				},
			})
		case "tool_result":
			id := p.ToolUseID
			if id == "" {
				id = p.ToolCallID
			}
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     p.Content,
			})
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return map[string]any{"role": "user", "content": blocks}
}

func anthropicFromAssistant(m ChatMessage) any {
	var blocks []any
	if m.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	for _, tc := range m.ToolCalls {
		var input any = map[string]any{}
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				input = tc.Arguments
			}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function,
			"input": input,
		})
	}
	if len(blocks) == 0 {
		return nil
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

// splitDataURL is defined in request.go (kept here only as a comment so
// future readers know where the canonical helper lives).

// AnthropicStreamState accumulates per-stream context across Anthropic
// Messages events. One instance per /v1/responses call.
type AnthropicStreamState struct {
	Model            string
	PromptTokens     int64
	OutputTokens     int64
	StopReason       string
	ActiveBlockIndex int
	ActiveBlockType  string
	ActiveToolID     string
	ActiveToolName   string
}

// NewAnthropicStreamState returns a zero-value state object.
func NewAnthropicStreamState() *AnthropicStreamState { return &AnthropicStreamState{} }

// Usage assembles the latest accumulated usage snapshot.
func (s *AnthropicStreamState) Usage() ChatUsage {
	return ChatUsage{
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.OutputTokens,
		TotalTokens:      s.PromptTokens + s.OutputTokens,
	}
}

// ApplyAnthropicEvent normalizes one Anthropic SSE event into a ChatDelta.
// `state` carries block-level context (current block index / type / accumulated
// tool arguments) across events. Phase 3 surfaces tool_use / input_json_delta
// and reasoning/thinking blocks.
func ApplyAnthropicEvent(eventType string, payload map[string]any, state *AnthropicStreamState) ChatDelta {
	if state == nil {
		state = &AnthropicStreamState{}
	}
	var delta ChatDelta

	switch eventType {
	case "message_start":
		if m, _ := payload["message"].(map[string]any); m != nil {
			if mn, _ := m["model"].(string); mn != "" {
				state.Model = mn
			}
			if u, ok := m["usage"].(map[string]any); ok {
				in := toInt64(u["input_tokens"])
				state.PromptTokens = in
				state.OutputTokens = toInt64(u["output_tokens"])
			}
		}
	case "content_block_start":
		idx := toInt64(payload["index"])
		if block, _ := payload["content_block"].(map[string]any); block != nil {
			state.ActiveBlockIndex = int(idx)
			if t, ok := block["type"].(string); ok {
				state.ActiveBlockType = t
				if t == "tool_use" {
					state.ActiveToolID = asString(block["id"])
					state.ActiveToolName = asString(block["name"])
				}
			}
		}
	case "content_block_delta":
		deltaPayload, _ := payload["delta"].(map[string]any)
		if deltaPayload == nil {
			break
		}
		switch dt, _ := deltaPayload["type"].(string); dt {
		case "text_delta":
			if t, ok := deltaPayload["text"].(string); ok {
				delta.ContentDelta = t
			}
		case "thinking_delta":
			if t, ok := deltaPayload["thinking"].(string); ok {
				delta.ReasoningDelta = t
			}
		case "signature_delta":
			// Anthropic emits opaque signature tokens that mark the boundary
			// of a reasoning block. We do not surface them on the wire; they're
			// required for upstream round-trips on subsequent turns.
		case "input_json_delta":
			// We map Anthropic tool_use arguments onto the Responses API's
			// function_call output item. The block index is treated as the
			// tool-call "index" so multiple parallel tools don't merge.
			frag := ChatToolCallFragment{
				Index:     state.ActiveBlockIndex,
				ID:        state.ActiveToolID,
				Name:      state.ActiveToolName,
				Arguments: asString(deltaPayload["partial_json"]),
			}
			delta.ToolCallFragments = append(delta.ToolCallFragments, frag)
		}
	case "content_block_stop":
		// Closing a tool block: emit nothing further; the converter
		// accumulates fragments and emits the matching close events itself.
		state.ActiveToolID = ""
		state.ActiveToolName = ""
		state.ActiveBlockType = ""
	case "message_delta":
		if d, ok := payload["delta"].(map[string]any); ok {
			if sr, ok := d["stop_reason"].(string); ok && sr != "" {
				state.StopReason = sr
				delta.FinishReason = mapAnthropicStopReason(sr)
			}
		}
		if u, ok := payload["usage"].(map[string]any); ok {
			state.OutputTokens = toInt64(u["output_tokens"])
		}
	case "message_stop":
		if state.StopReason != "" {
			delta.FinishReason = mapAnthropicStopReason(state.StopReason)
		}
		delta.Usage = ChatUsage{
			PromptTokens:     state.PromptTokens,
			CompletionTokens: state.OutputTokens,
			TotalTokens:      state.PromptTokens + state.OutputTokens,
		}
	}
	return delta
}

// FromAnthropicResponse converts a non-streaming Anthropic Messages response
// into a canonical ChatResponse.
func FromAnthropicResponse(body map[string]any) (ChatResponse, error) {
	out := ChatResponse{}
	if id, ok := body["id"].(string); ok {
		out.ID = id
	}
	if model, ok := body["model"].(string); ok {
		out.Model = model
	}
	stopReason, _ := body["stop_reason"].(string)
	out.FinishReason = mapAnthropicStopReason(stopReason)

	if blocks, ok := body["content"].([]any); ok {
		var textParts []string
		for _, raw := range blocks {
			b, _ := raw.(map[string]any)
			if b == nil {
				continue
			}
			switch b["type"] {
			case "text":
				if t, _ := b["text"].(string); t != "" {
					textParts = append(textParts, t)
				}
			case "tool_use":
				tc := ChatToolCall{
					ID:       asString(b["id"]),
					Type:     "function",
					Function: asString(b["name"]),
				}
				if b["input"] != nil {
					b2, err := json.Marshal(b["input"])
					if err == nil {
						tc.Arguments = string(b2)
					}
				}
				out.ToolCalls = append(out.ToolCalls, tc)
			}
		}
		out.Content = strings.Join(textParts, "")
	}
	if u, ok := body["usage"].(map[string]any); ok {
		out.Usage = usageFromAnthropic(u)
	}
	return out, nil
}

func usageFromAnthropic(u map[string]any) ChatUsage {
	out := ChatUsage{}
	out.PromptTokens = toInt64(u["input_tokens"])
	out.CompletionTokens = toInt64(u["output_tokens"])
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	out.CachedTokens = toInt64(u["cache_read_input_tokens"])
	return out
}

func mapAnthropicStopReason(s string) string {
	switch s {
	case "end_turn", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	}
	return s
}
