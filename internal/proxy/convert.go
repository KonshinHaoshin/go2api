package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file converts between the OpenAI Chat Completions wire format and the
// Anthropic Messages wire format. It is intentionally narrow in scope: text
// completion only. Tool calls, image inputs, structured outputs, and other
// advanced features are NOT covered by these helpers — those need separate
// conversion logic if/when they become required.
//
// Conversion strategy:
//
//   - Models default to OpenAI-compatible families. Models in the Anthropic
//     family (MiniMax, Qwen) are handled by the OpenCode Go /v1/messages
//     endpoint.
//   - When a client posts an OpenAI request for an Anthropic-family model,
//     we convert to Anthropic format, forward to /messages, then convert
//     the response back to OpenAI format so the client sees no difference.
//   - The mirror direction (Anthropic request → OpenAI upstream) is also
//     implemented for symmetry but the primary use case is OpenAI → Anthropic.

// ---------------------------------------------------------------------------
// Request conversion: OpenAI → Anthropic
// ---------------------------------------------------------------------------

// OpenAIToAnthropicRequest rewrites an OpenAI Chat Completions request body
// into the equivalent Anthropic Messages request body. It mutates and
// returns a new map; the original is left untouched.
//
// Required-by-Anthropic defaults we synthesize:
//   - max_tokens: 4096 if the client didn't set one (Anthropic rejects
//     requests without max_tokens).
func OpenAIToAnthropicRequest(openaiBody map[string]any) (map[string]any, error) {
	out := map[string]any{}

	// Model: strip the opencode-go/ prefix before sending.
	if m, _ := openaiBody["model"].(string); m != "" {
		out["model"] = strings.TrimPrefix(m, "opencode-go/")
	} else {
		return nil, fmt.Errorf("missing model field")
	}

	// System message extraction. OpenAI carries the system prompt inside the
	// messages array; Anthropic wants it as a top-level string.
	var systemParts []string
	anthropicMessages := []any{}
	if msgs, _ := openaiBody["messages"].([]any); msgs != nil {
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" {
				if c, ok := m["content"].(string); ok {
					systemParts = append(systemParts, c)
				}
				continue
			}
			// Anthropic only allows user/assistant roles. Drop "function"
			// messages and anything else unexpected.
			if role != "user" && role != "assistant" {
				continue
			}
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    role,
				"content": m["content"],
			})
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(anthropicMessages) == 0 {
		return nil, fmt.Errorf("messages array must contain at least one user message")
	}
	out["messages"] = anthropicMessages

	// max_tokens is required by Anthropic. Default if absent.
	if v, ok := openaiBody["max_tokens"]; ok {
		out["max_tokens"] = toInt(v)
	} else {
		out["max_tokens"] = 4096
	}

	if v, ok := openaiBody["temperature"]; ok {
		out["temperature"] = toFloat(v)
	}
	if v, ok := openaiBody["top_p"]; ok {
		out["top_p"] = toFloat(v)
	}
	if v, ok := openaiBody["stop"]; ok {
		switch s := v.(type) {
		case string:
			out["stop_sequences"] = []any{s}
		case []any:
			out["stop_sequences"] = s
		}
	}

	// Pass through stream flag and any tools the caller may have set.
	if v, ok := openaiBody["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := openaiBody["tools"]; ok {
		out["tools"] = convertOpenAIToolsToAnthropic(v)
	}

	return out, nil
}

func convertOpenAIToolsToAnthropic(openaiTools any) []any {
	arr, _ := openaiTools.([]any)
	if arr == nil {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, raw := range arr {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		// OpenAI: {type:"function", function:{name, description, parameters}}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		converted := map[string]any{}
		if name, _ := fn["name"].(string); name != "" {
			converted["name"] = name
		}
		if desc, _ := fn["description"].(string); desc != "" {
			converted["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			converted["input_schema"] = params
		}
		out = append(out, converted)
	}
	return out
}

// ---------------------------------------------------------------------------
// Response conversion: Anthropic → OpenAI
// ---------------------------------------------------------------------------

// AnthropicToOpenAIResponse rewrites a non-streaming Anthropic Messages
// response into an OpenAI Chat Completions response.
func AnthropicToOpenAIResponse(anthropicBody map[string]any) (map[string]any, error) {
	// Content blocks → a single concatenated string (and tool_calls if present).
	var textParts []string
	var toolCalls []any
	if blocks, _ := anthropicBody["content"].([]any); blocks != nil {
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block == nil {
				continue
			}
			switch t, _ := block["type"].(string); t {
			case "text":
				if txt, _ := block["text"].(string); txt != "" {
					textParts = append(textParts, txt)
				}
			case "tool_use":
				tc := map[string]any{
					"id":   block["id"],
					"type": "function",
					"function": map[string]any{
						"name":      block["name"],
						"arguments": marshalJSON(block["input"]),
					},
				}
				toolCalls = append(toolCalls, tc)
			}
		}
	}

	message := map[string]any{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	// Map Anthropic stop_reason → OpenAI finish_reason.
	finish := mapAnthropicStopReason(anthropicBody["stop_reason"])

	out := map[string]any{
		"id":      randomID("chatcmpl-"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   anthropicBody["model"],
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}

	if u, ok := anthropicBody["usage"].(map[string]any); ok {
		in, _ := u["input_tokens"].(float64)
		outTok, _ := u["output_tokens"].(float64)
		out["usage"] = map[string]any{
			"prompt_tokens":     int(in),
			"completion_tokens": int(outTok),
			"total_tokens":      int(in + outTok),
		}
	}

	return out, nil
}

func mapAnthropicStopReason(sr any) string {
	s, _ := sr.(string)
	switch s {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		if s == "" {
			return "stop"
		}
		return s
	}
}

// ---------------------------------------------------------------------------
// Streaming conversion: Anthropic SSE → OpenAI SSE
// ---------------------------------------------------------------------------

// AnthropicEventToOpenAIChunk converts a single Anthropic streaming event
// (parsed JSON) into one or more OpenAI streaming chunks. Some Anthropic
// events produce no output (e.g. ping, content_block_start); in that case
// the returned slice is empty.
//
// Each OpenAI chunk has the standard chat.completion.chunk shape:
//
//	{
//	  "id": "...", "object":"chat.completion.chunk", "created":...,
//	  "model": "...",
//	  "choices":[{"index":0,"delta":{...},"finish_reason":null}]
//	}
func AnthropicEventToOpenAIChunk(event map[string]any) []map[string]any {
	eventType, _ := event["type"].(string)
	model, _ := event["message"].(map[string]any)
	modelName := ""
	if model != nil {
		if mn, ok := model["model"].(string); ok {
			modelName = mn
		}
	}
	if modelName == "" {
		modelName = "unknown"
	}

	common := func() map[string]any {
		return map[string]any{
			"id":      randomID("chatcmpl-"),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": nil,
				},
			},
		}
	}

	switch eventType {
	case "message_start":
		// Emit a chunk that announces the assistant role.
		chunk := common()
		chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"] = "assistant"
		return []map[string]any{chunk}

	case "content_block_start":
		// For text blocks, emit an empty content chunk to signal the
		// beginning of a text segment. We don't emit anything for tool_use
		// blocks (the corresponding tool_call_delta will carry it).
		block, _ := event["content_block"].(map[string]any)
		if block == nil {
			return nil
		}
		if t, _ := block["type"].(string); t == "text" {
			chunk := common()
			chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] = ""
			return []map[string]any{chunk}
		}
		return nil

	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch dType, _ := delta["type"].(string); dType {
		case "text_delta":
			txt, _ := delta["text"].(string)
			chunk := common()
			chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] = txt
			return []map[string]any{chunk}
		case "input_json_delta":
			// tool_call streaming: emit as incremental arguments string.
			partial, _ := delta["partial_json"].(string)
			block, _ := event["content_block"].(map[string]any)
			toolID, _ := block["id"].(string)
			toolName, _ := block["name"].(string)
			chunk := common()
			tc := map[string]any{
				"index": 0,
				"id":    toolID,
				"type":  "function",
				"function": map[string]any{
					"name":      toolName,
					"arguments": partial,
				},
			}
			chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"] = []any{tc}
			return []map[string]any{chunk}
		}
		return nil

	case "content_block_stop":
		return nil

	case "message_delta":
		// The stop_reason and final usage arrive here.
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		chunk := common()
		choice := chunk["choices"].([]any)[0].(map[string]any)
		if sr, ok := delta["stop_reason"]; ok {
			choice["finish_reason"] = mapAnthropicStopReason(sr)
		}
		return []map[string]any{chunk}

	case "message_stop":
		// Emit a final empty chunk followed by the [DONE] marker; the
		// caller handles the [DONE] sentinel.
		chunk := common()
		chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{}
		return []map[string]any{chunk}

	case "ping":
		return nil

	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Mirror direction: Anthropic request → OpenAI upstream
// (Used when an Anthropic client requests an OpenAI-family model.)
// ---------------------------------------------------------------------------

// AnthropicToOpenAIRequest converts an Anthropic Messages request into the
// equivalent OpenAI Chat Completions request.
func AnthropicToOpenAIRequest(anthropicBody map[string]any) (map[string]any, error) {
	out := map[string]any{
		"model": "opencode-go/" + asString(anthropicBody["model"]),
	}

	// System prompt becomes the first system message.
	messages := []any{}
	if sys, ok := anthropicBody["system"].(string); ok && sys != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": sys,
		})
	}
	if arr, _ := anthropicBody["messages"].([]any); arr != nil {
		for _, raw := range arr {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			content := m["content"]
			// Anthropic tool_result content blocks become OpenAI tool messages.
			if role == "user" {
				if blocks, ok := content.([]any); ok {
					var toolResults []map[string]any
					var textParts []string
					for _, b := range blocks {
						bm, _ := b.(map[string]any)
						if bm == nil {
							continue
						}
						if t, _ := bm["type"].(string); t == "tool_result" {
							contentStr := asString(bm["content"])
							if c, ok := bm["content"].([]any); ok {
								b2, _ := json.Marshal(c)
								contentStr = string(b2)
							}
							toolResults = append(toolResults, map[string]any{
								"role":         "tool",
								"tool_call_id": bm["tool_use_id"],
								"content":      contentStr,
							})
						} else if t == "text" {
							textParts = append(textParts, asString(bm["text"]))
						}
					}
					if len(textParts) > 0 {
						messages = append(messages, map[string]any{
							"role":    "user",
							"content": strings.Join(textParts, ""),
						})
					}
					for _, tr := range toolResults {
						messages = append(messages, tr)
					}
					continue
				}
			}
			if role == "assistant" {
				if blocks, ok := content.([]any); ok {
					var textParts []string
					var toolCalls []any
					for _, b := range blocks {
						bm, _ := b.(map[string]any)
						if bm == nil {
							continue
						}
						switch t, _ := bm["type"].(string); t {
						case "text":
							textParts = append(textParts, asString(bm["text"]))
						case "tool_use":
							argsJSON := asString(marshalJSON(bm["input"]))
							if j, ok := bm["input"].(map[string]any); ok {
								if b, err := json.Marshal(j); err == nil {
									argsJSON = string(b)
								}
							}
							toolCalls = append(toolCalls, map[string]any{
								"id":   bm["id"],
								"type": "function",
								"function": map[string]any{
									"name":      bm["name"],
									"arguments": argsJSON,
								},
							})
						}
					}
					assistantMsg := map[string]any{"role": "assistant"}
					if len(textParts) > 0 {
						assistantMsg["content"] = strings.Join(textParts, "")
					}
					if len(toolCalls) > 0 {
						assistantMsg["tool_calls"] = toolCalls
					}
					messages = append(messages, assistantMsg)
					continue
				}
			}
			messages = append(messages, map[string]any{
				"role":    role,
				"content": content,
			})
		}
	}
	out["messages"] = messages

	if v, ok := anthropicBody["max_tokens"]; ok {
		out["max_tokens"] = toInt(v)
	}
	if v, ok := anthropicBody["temperature"]; ok {
		out["temperature"] = toFloat(v)
	}
	if v, ok := anthropicBody["top_p"]; ok {
		out["top_p"] = toFloat(v)
	}
	if v, ok := anthropicBody["stop_sequences"]; ok {
		out["stop"] = v
	}
	if v, ok := anthropicBody["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := anthropicBody["tools"]; ok {
		out["tools"] = convertAnthropicToolsToOpenAI(v)
	}
	return out, nil
}

func convertAnthropicToolsToOpenAI(tools any) []any {
	arr, _ := tools.([]any)
	if arr == nil {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, raw := range arr {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		fn := map[string]any{}
		if name, _ := t["name"].(string); name != "" {
			fn["name"] = name
		}
		if desc, _ := t["description"].(string); desc != "" {
			fn["description"] = desc
		}
		if schema, ok := t["input_schema"]; ok {
			fn["parameters"] = schema
		}
		out = append(out, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

// OpenAIToAnthropicResponse converts an OpenAI non-streaming response into
// the Anthropic Messages response shape.
func OpenAIToAnthropicResponse(openaiBody map[string]any) (map[string]any, error) {
	out := map[string]any{
		"id":    randomID("msg_"),
		"type":  "message",
		"role":  "assistant",
		"model": strings.TrimPrefix(asString(openaiBody["model"]), "opencode-go/"),
	}

	var contentBlocks []any
	stopReason := "end_turn"
	if choices, _ := openaiBody["choices"].([]any); len(choices) > 0 {
		c, _ := choices[0].(map[string]any)
		if c != nil {
			if msg, ok := c["message"].(map[string]any); ok {
				if txt, ok := msg["content"].(string); ok && txt != "" {
					contentBlocks = append(contentBlocks, map[string]any{
						"type": "text",
						"text": txt,
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						tm, _ := tc.(map[string]any)
						if tm == nil {
							continue
						}
						fn, _ := tm["function"].(map[string]any)
						if fn == nil {
							continue
						}
						argsStr := asString(fn["arguments"])
						var input any = map[string]any{}
						if argsStr != "" {
							if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
								input = argsStr
							}
						}
						contentBlocks = append(contentBlocks, map[string]any{
							"type":  "tool_use",
							"id":    tm["id"],
							"name":  fn["name"],
							"input": input,
						})
					}
				}
			}
			switch fr, _ := c["finish_reason"].(string); fr {
			case "stop":
				stopReason = "end_turn"
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}
	out["content"] = contentBlocks
	out["stop_reason"] = stopReason

	if u, ok := openaiBody["usage"].(map[string]any); ok {
		in, _ := u["prompt_tokens"].(float64)
		outTok, _ := u["completion_tokens"].(float64)
		out["usage"] = map[string]any{
			"input_tokens":  int(in),
			"output_tokens": int(outTok),
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mirror streaming: OpenAI SSE → Anthropic SSE
// ---------------------------------------------------------------------------

// OpenAIChunkToAnthropicEvents converts a single OpenAI streaming chunk into
// 0..n Anthropic streaming events. The returned slice may be empty (e.g. for
// chunks that carry only finish_reason changes).
//
// We maintain minimal state in the caller via the returned `state` map (use
// OpenAIStreamState to allocate one per request).
type OpenAIStreamState struct {
	started       bool
	textBlockOpen bool
	finishReason  string
}

func OpenAIChunkToAnthropicEvents(chunk map[string]any, st *OpenAIStreamState) []map[string]any {
	if st == nil {
		st = &OpenAIStreamState{}
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return nil
	}
	delta, _ := c["delta"].(map[string]any)
	modelName := asString(chunk["model"])

	var events []map[string]any
	if !st.started {
		st.started = true
		events = append(events, map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            randomID("msg_"),
				"type":          "message",
				"role":          "assistant",
				"model":         modelName,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}

	if delta != nil {
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if !st.textBlockOpen {
				events = append(events, map[string]any{
					"type":          "content_block_start",
					"index":         0,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
				st.textBlockOpen = true
			}
			events = append(events, map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": txt},
			})
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			for _, tc := range tcs {
				tm, _ := tc.(map[string]any)
				if tm == nil {
					continue
				}
				fn, _ := tm["function"].(map[string]any)
				events = append(events, map[string]any{
					"type":  "content_block_start",
					"index": 1,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tm["id"],
						"name":  asString(fn["name"]),
						"input": map[string]any{},
					},
				})
				if args, _ := fn["arguments"].(string); args != "" {
					events = append(events, map[string]any{
						"type":  "content_block_delta",
						"index": 1,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					})
				}
			}
		}
	}

	if fr, ok := c["finish_reason"].(string); ok && fr != "" && fr != "null" {
		st.finishReason = fr
	}

	// Final close events are emitted by the caller when the stream ends.
	return events
}

// FinalizeOpenAIStream emits the close events for the Anthropic stream.
func FinalizeOpenAIStream(st *OpenAIStreamState) []map[string]any {
	if st == nil {
		return nil
	}
	var events []map[string]any
	if st.textBlockOpen {
		events = append(events, map[string]any{"type": "content_block_stop", "index": 0})
	}
	stopReason := "end_turn"
	switch st.finishReason {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}
	events = append(events, map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	})
	events = append(events, map[string]any{"type": "message_stop"})
	return events
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
	}
	return 0
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		b, err := json.Marshal(s)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func randomID(prefix string) string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
