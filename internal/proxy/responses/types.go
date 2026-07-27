package responses

import (
	"encoding/json"
	"strings"
	"time"
)

// Request is the top-level shape accepted on POST /v1/responses.
//
// It models the slice of the OpenAI Responses API that the chat UI surface
// emits. Capability-affecting fields that go2api cannot honor (background,
// hosted tools, file references) are explicitly rejected during validation.
//
// Wire compatibility is intentional: field names, nullability, and JSON shapes
// match OpenAI's Responses reference. Fields that v1 does not implement
// (parallel_tool_calls, prompt_cache_key, safety_identifier, …) are accepted
// via Extensions and may be ignored or rejected per ValidateRequest.
type Request struct {
	Model              string          `json:"model"`
	Input              Input           `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	Tools              []Tool          `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Conversation       *Conversation   `json:"conversation,omitempty"`
	Reasoning          *Reasoning      `json:"reasoning,omitempty"`
	Text               *TextFormat     `json:"text,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`

	// Rejected-by-validation fields go here so the JSON decoder
	// still accepts the request body without erroring at parse time.
	Background bool `json:"background,omitempty"`

	// Anything else the client sent that we don't model. Stored verbatim so
	// the converter can decide what to do on a per-field basis.
	Extensions map[string]json.RawMessage `json:"-"`
}

// Input is the union accepted by Responses. JSON shape is either:
//   - a string, or
//   - an array of input items.
type Input struct {
	Raw        json.RawMessage
	IsString   bool
	StringVal  string
	Items      []InputItem
}

// MarshalJSON renders Input as the canonical wire form.
func (i Input) MarshalJSON() ([]byte, error) {
	if i.IsString {
		return json.Marshal(i.StringVal)
	}
	if i.Items != nil {
		return json.Marshal(i.Items)
	}
	return []byte(`""`), nil
}

// UnmarshalJSON accepts either a string or an item array. Items are stored as
// RawMessage so individual item types can be dispatched on without losing
// unknown-shape fields.
func (i *Input) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		i.IsString = true
		i.StringVal = s
		i.Raw = data
		return nil
	}
	if len(trimmed) == 0 {
		i.Raw = data
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	items := make([]InputItem, 0, len(arr))
	for idx, raw := range arr {
		var probed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probed); err != nil {
			return err
		}
		switch probed.Type {
		case "message":
			var m InputMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			m.itemIndex = idx
			items = append(items, &m)
		case "function_call_output":
			var fc InputFunctionCallOutput
			if err := json.Unmarshal(raw, &fc); err != nil {
				return err
			}
			fc.itemIndex = idx
			items = append(items, &fc)
		default:
			// Preserve the raw shape for items we don't model yet (reasoning, etc.)
			items = append(items, unknownInputItem{Raw: raw, Index: idx})
		}
	}
	i.Items = items
	i.Raw = data
	return nil
}

// InputItem is the interface implemented by every input element.
type InputItem interface {
	itemType() string
	ItemIndex() int
}

// InputMessage is a Messages-role input item. Role is one of "user", "assistant",
// "system", or "developer". Content can be a string or an array of parts.
type InputMessage struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Status  string          `json:"status,omitempty"`
	Content []InputContent  `json:"-"`
	Raw     json.RawMessage `json:"-"`

	itemIndex int
}

// ItemIndex returns the original position of this item in the input array.
func (m *InputMessage) ItemIndex() int { return m.itemIndex }

// UnmarshalJSON accepts Content as either a string or an array of parts.
func (m *InputMessage) UnmarshalJSON(data []byte) error {
	type alias InputMessage
	var sh alias
	if err := json.Unmarshal(data, &sh); err != nil {
		return err
	}
	*m = InputMessage(sh)

	var probed struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &probed); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(probed.Content))
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(probed.Content, &s); err != nil {
			return err
		}
		m.Content = []InputContent{{Kind: "input_text", Text: s}}
		m.Raw = probed.Content
		return nil
	}
	if len(trimmed) == 0 {
		m.Raw = probed.Content
		return nil
	}
	var parts []struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Image       string `json:"image_url,omitempty"`
		FileID      string `json:"file_id,omitempty"`
		Detail      string `json:"detail,omitempty"`
		Annotations any    `json:"annotations,omitempty"`
	}
	if err := json.Unmarshal(probed.Content, &parts); err != nil {
		return err
	}
	for _, p := range parts {
		c := InputContent{Kind: p.Type, Text: p.Text, Detail: p.Detail}
		switch p.Type {
		case "input_image":
			c.URL = p.Image
			c.FileID = p.FileID
		case "input_text":
			c.Text = p.Text
		}
		m.Content = append(m.Content, c)
	}
	m.Raw = probed.Content
	return nil
}

func (m *InputMessage) itemType() string { return "message" }

// InputFunctionCallOutput carries the result of a tool call that the client
// executed locally. The Responses API sends these as input items with
// type="function_call_output"; go2api converts them to Chat Completions
// tool-role messages so the upstream API receives a valid sequence.
type InputFunctionCallOutput struct {
	Type      string `json:"type"`    // "function_call_output"
	CallID    string `json:"call_id"` // matches the function_call's call_id
	Output    string `json:"output"`  // the result text/JSON from the tool

	itemIndex int
}

func (f *InputFunctionCallOutput) itemType() string { return "function_call_output" }
func (f *InputFunctionCallOutput) ItemIndex() int   { return f.itemIndex }

// InputContent is a single content part inside an input message. The Kind
// discriminates "input_text", "input_image", etc.
type InputContent struct {
	Kind   string
	Text   string
	URL    string
	FileID string
	Detail string
}

// unknownInputItem is the catch-all for input items whose type we don't model.
type unknownInputItem struct {
	Raw   json.RawMessage
	Index int
}

func (u unknownInputItem) itemType() string {
	var probed struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(u.Raw, &probed)
	return probed.Type
}
func (u unknownInputItem) ItemIndex() int { return u.Index }

// Tool is the union accepted in Request.Tools. Only "function" and one
// tool_choice "reasoning" flavor are modeled in v1; hosted tools go through
// ValidateRequest which rejects them.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`

	// For hosted tools (web_search, file_search, computer_use, code_interpreter).
	// Captured raw so the validator can reject them with a precise error path.
	HostedConfig json.RawMessage `json:"-"`
}

// Conversation references either an existing conversation or creates a new one.
type Conversation struct {
	ID string `json:"id,omitempty"`
}

// Reasoning requests the upstream emit a reasoning/think trail.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TextFormat is the structured-output configuration.
type TextFormat struct {
	Format *ResponseFormat `json:"format,omitempty"`
}

// ResponseFormat mirrors the spec: type=="text" or type=="json_schema".
type ResponseFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	Description string          `json:"description,omitempty"`
}

// Response is the non-streaming output of /v1/responses.
type Response struct {
	Object             string        `json:"object"` // always "response"
	ID                 string        `json:"id"`
	CreatedAt          int64         `json:"created_at"`
	Status             string        `json:"status"` // "completed" | "failed" | "incomplete" | "queued" | "in_progress"
	Model              string        `json:"model"`
	Output             []OutputItem  `json:"output"`
	OutputText         string        `json:"output_text,omitempty"` // convenience: concatenated text from message items
	Usage              Usage         `json:"usage"`
	PreviousResponseID string        `json:"previous_response_id,omitempty"`
	Conversation       *Conversation `json:"conversation,omitempty"`
	Error              *ResponseErr  `json:"error,omitempty"`
	IncompleteDetails  *Incomplete   `json:"incomplete_details,omitempty"`

	// Echoed client config — present in OpenAI's spec for some keys; v1 emits it.
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool        `json:"parallel_tool_calls,omitempty"`
}

// OutputItem is the interface implemented by every Response output element.
type OutputItem interface {
	outputType() string
}

// OutputMessage is the assistant text output. Status is "in_progress" | "completed".
type OutputMessage struct {
	Type    string         `json:"type"` // "message"
	ID      string         `json:"id"`
	Role    string         `json:"role"` // "assistant"
	Status  string         `json:"status"`
	Content []OutputContent `json:"content"`
}

func (*OutputMessage) outputType() string { return "message" }

// OutputFunctionCall carries a completed function invocation. After streaming,
// arguments should be a complete JSON object string.
type OutputFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

func (*OutputFunctionCall) outputType() string { return "function_call" }

// OutputReasoning is an opaque blob the upstream used to expose thinking.
type OutputReasoning struct {
	Type   string         `json:"type"` // "reasoning"
	ID     string         `json:"id"`
	Summary []OutputContent `json:"summary,omitempty"`
	Content []OutputContent `json:"content,omitempty"`
	Status string         `json:"status"`
}

func (*OutputReasoning) outputType() string { return "reasoning" }

// OutputContent mirrors ResponseContentPart wire shape. Output text uses
// "output_text" and may carry annotations.
type OutputContent struct {
	Type        string          `json:"type"` // "output_text" | "reasoning_text"
	Text        string          `json:"text"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Logprobs    json.RawMessage `json:"logprobs,omitempty"`
}

// Usage matches the Responses spec shape with input/output/total plus details.
type Usage struct {
	InputTokens         int64                 `json:"input_tokens"`
	OutputTokens        int64                 `json:"output_tokens"`
	TotalTokens         int64                 `json:"total_tokens"`
	InputTokensDetails  *InputTokensDetails   `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetails  `json:"output_tokens_details,omitempty"`
}

// InputTokensDetails mirrors spec; cached_tokens surface the OpenAI cache hit.
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

// OutputTokensDetails surfaces reasoning token counts where the upstream reports them.
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// ResponseErr is the spec-shaped error block on a Response.
type ResponseErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Incomplete surfaces the partial-completion reason (max_output_tokens / etc).
type Incomplete struct {
	Reason string `json:"reason"`
}

// StreamEvent is a single SSE line forwarded to the Responses client. The
// Event field is the SSE `event:` line; Data is marshaled as the `data:` line.
type StreamEvent struct {
	Event string
	Data  StreamEventData
}

// StreamEventData is a discriminated union on `type`. Most events embed a
// SequenceNumber and an OutputIndex where applicable. The `item` field is
// serialized from the Item interface via a custom MarshalJSON below; callers
// should set Item (the typed struct) instead of ItemJSON.
type StreamEventData struct {
	Type           string          `json:"type"`
	SequenceNumber int64           `json:"sequence_number,omitempty"`
	Response       *Response       `json:"response,omitempty"`
	OutputIndex    int             `json:"output_index,omitempty"`
	Item           OutputItem      `json:"-"`
	ContentIndex   int             `json:"content_index,omitempty"`
	Delta          string          `json:"delta,omitempty"`
	Arguments      string          `json:"arguments,omitempty"`
	ItemID         string          `json:"item_id,omitempty"`
	Error          *ResponseErr    `json:"error,omitempty"`
}

// MarshalJSON renders Item as the `item` field when set, alongside the other
// primitive fields. If Item is nil, `item` is omitted.
func (e StreamEventData) MarshalJSON() ([]byte, error) {
	type alias StreamEventData
	out := struct {
		alias
		Item json.RawMessage `json:"item,omitempty"`
	}{alias: alias(e)}
	if e.Item != nil {
		b, err := json.Marshal(e.Item)
		if err != nil {
			return nil, err
		}
		out.Item = b
	}
	return json.Marshal(out)
}

// ChatRequest is the canonical Chat representation used internally. It maps
// onto either OpenAI Chat Completions or Anthropic Messages format. This
// package owns all rules for translating to/from the wire.
type ChatRequest struct {
	Model     string
	Messages  []ChatMessage
	Tools     []ChatTool
	ToolChoice any
	Stream    bool

	Temperature     *float64
	TopP            *float64
	MaxOutputTokens *int
	Stop            []string
	Reasoning       *Reasoning

	// ResponseFormat is opaque; downstream providers convert it as supported.
	ResponseFormat *ResponseFormat

	// For Anthropic upstream.
	System string
}

// ChatMessage is one message in a canonical Chat. Content is homogeneous
// after provider conversion; here ContentParts may mix text/image/tool_result.
type ChatMessage struct {
	Role       string
	Content    string
	ContentParts []ChatContentPart
	ToolCalls  []ChatToolCall
	ToolCallID string
	Name       string
	Reasoning  string
}

// ChatContentPart is one part of an assistant/user message. After upstream
// conversion this is provider-native (text, image, tool_result).
type ChatContentPart struct {
	Type     string // "text" | "image_url" | "image" | "tool_result"
	Text     string
	ImageURL string
	Detail   string
	MediaType string // anthropic
	Data     string // anthropic base64

	ToolCallID string // anthropic tool_result
	ToolUseID  string // anthropic tool_result (mirror)
	Content    string // anthropic tool_result content (string form)
}

// ChatTool is the canonical tool descriptor shared between providers.
type ChatTool struct {
	Type        string // "function"
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      bool
}

// ChatToolCall is one function invocation in an assistant message.
type ChatToolCall struct {
	ID        string
	Type      string // "function"
	Function  string
	Arguments string
}

// ChatResponse is a single non-streaming response message. StreamConverter
// reduces deltas to the same shape; FromChatResponse assembles wire form.
type ChatResponse struct {
	ID           string
	Model        string
	CreatedAt    time.Time
	Content      string
	ContentParts []ChatContentPart
	ToolCalls    []ChatToolCall
	Reasoning    string
	FinishReason string // "stop" | "length" | "tool_calls" | ""
	Usage        ChatUsage
}

// ChatUsage is provider-neutral. Provider adapters translate as needed.
type ChatUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	ReasoningTokens  int64
}
