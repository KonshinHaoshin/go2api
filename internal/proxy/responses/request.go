package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxImageBytes caps the size of a single inline image (decoded base64) that
// the proxy will accept. Above this, the request is rejected so the upstreams
// (and our own state) don't get flooded. 5 MiB matches what most chat
// upstreams accept without truncation.
const MaxImageBytes = 5 * 1024 * 1024

// ValidateRequest returns a typed error if the request cannot be honored.
// Capability-affecting fields that v1 doesn't support are explicitly rejected
// (HTTP 400), not silently dropped — silent drops would change model behavior
// while falsely reporting success.
func ValidateRequest(req *Request) error {
	if req == nil {
		return &InvalidRequestError{Message: "request is empty"}
	}
	if strings.TrimSpace(req.Model) == "" {
		return &InvalidRequestError{Message: "model is required", Param: "model"}
	}
	// background is a flag that promises async execution + a retrieval endpoint.
	// We don't implement either, so explicit rejection is safer than fake-OK.
	if req.Background {
		return &InvalidRequestError{
			Message: "background is not supported by go2api",
			Param:   "background",
			Code:    "unsupported_background",
		}
	}
	for i, t := range req.Tools {
		if err := validateTool(i, t); err != nil {
			return err
		}
	}
	// Reject hosted tools in tool_choice.
	if t, ok := req.ToolChoice.(string); ok && t == "required" && len(req.Tools) == 0 {
		return &InvalidRequestError{
			Message: "tool_choice=required without tools is invalid",
			Param:   "tool_choice",
		}
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens <= 0 {
		return &InvalidRequestError{
			Message: "max_output_tokens must be positive",
			Param:   "max_output_tokens",
		}
	}
	if req.Input.IsString {
		if strings.TrimSpace(req.Input.StringVal) == "" {
			return &InvalidRequestError{Message: "input is empty", Param: "input"}
		}
	}
	if !req.Input.IsString {
		if len(req.Input.Items) == 0 {
			return &InvalidRequestError{Message: "input is empty", Param: "input"}
		}
		if err := validateInputItems(req.Input.Items); err != nil {
			return err
		}
	}
	if req.Text != nil && req.Text.Format != nil {
		if err := validateResponseFormat(req.Text.Format); err != nil {
			return err
		}
	}
	return nil
}

// validateInputItems walks each message's content parts and rejects:
//   - input_image.file_id (no /v1/files support)
//   - non-data-URL image sources (we do not fetch arbitrary remote URLs,
//     to avoid SSRF and download-limit surprises)
//   - input_image.data URLs that don't decode, aren't image/* mime, or
//     exceed MaxImageBytes.
func validateInputItems(items []InputItem) error {
	for _, item := range items {
		msg, ok := item.(*InputMessage)
		if !ok {
			continue
		}
		for i, c := range msg.Content {
			path := fmt.Sprintf("input[%d].content[%d]", msg.ItemIndex(), i)
			switch c.Kind {
			case "input_image":
				if c.FileID != "" {
					return &InvalidRequestError{
						Message: "input_image.file_id is not supported; use input_image.image_url with an inline data URL",
						Param:   path + ".file_id",
						Code:    "unsupported_file_reference",
					}
				}
				if c.URL == "" {
					return &InvalidRequestError{
						Message: "input_image.image_url is required",
						Param:   path + ".image_url",
					}
				}
				if !strings.HasPrefix(c.URL, "data:") {
					return &InvalidRequestError{
						Message: "only inline data URLs are accepted for input_image (no remote URL fetching)",
						Param:   path + ".image_url",
						Code:    "unsupported_image_source",
					}
				}
				mediaType, payload, ok := splitDataURL(c.URL)
				if !ok {
					return &InvalidRequestError{
						Message: "input_image data URL is malformed",
						Param:   path + ".image_url",
					}
				}
				if !strings.HasPrefix(mediaType, "image/") {
					return &InvalidRequestError{
						Message: fmt.Sprintf("input_image media type %q not supported (need image/*)", mediaType),
						Param:   path + ".image_url",
						Code:    "unsupported_media_type",
					}
				}
				// base64-decoded size estimate: 3/4 of the encoded length.
				if est := 3 * len(payload) / 4; est > MaxImageBytes {
					return &InvalidRequestError{
						Message: fmt.Sprintf("input_image too large (%d bytes > %d)", est, MaxImageBytes),
						Param:   path + ".image_url",
						Code:    "image_too_large",
					}
				}
			case "input_file":
				return &InvalidRequestError{
					Message: "input_file is not supported",
					Param:   path,
					Code:    "unsupported_file_reference",
				}
			}
		}
	}
	return nil
}

// splitDataURL parses a `data:<media>;base64,<payload>` URL. Exposed as a
// package-internal helper so the request validator and the Anthropic
// converter agree on the wire shape.
func splitDataURL(url string) (media, payload string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	body := url[5:]
	semi := strings.IndexByte(body, ';')
	if semi < 0 {
		return "", "", false
	}
	media = body[:semi]
	rest := body[semi+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return "", "", false
	}
	return media, rest[len("base64,"):], true
}

// clientSideToolTypes contains tool types that Codex and similar clients
// handle locally — they don't need to be forwarded to the upstream LLM.
// Instead of rejecting the whole request with 400, we silently drop these
// so the model still sees the function tools it can actually call.
var clientSideToolTypes = map[string]bool{
	"custom":      true, // Codex: apply_patch, etc.
	"namespace":   true, // Codex: plugin namespaces (image_gen, codex_app)
	"tool_search": true, // Codex: marketplace tool search
	"web_search":  true, // hosted search — not implemented on upstream
	"file_search": true, // hosted file search — not implemented
	"computer_use": true,
	"code_interpreter": true,
}

func validateTool(i int, t Tool) error {
	if t.Type == "function" {
		if t.Name == "" {
			return &InvalidRequestError{
				Message: fmt.Sprintf("tools[%d].name is required for type=function", i),
				Param:   fmt.Sprintf("tools[%d].name", i),
			}
		}
		return nil
	}
	// Client-side and hosted tools are silently dropped in ToChatRequest;
	// no validation error needed — the request proceeds with function tools only.
	return nil
}

func validateResponseFormat(rf *ResponseFormat) error {
	switch rf.Type {
	case "text", "":
		return nil
	case "json_schema":
		if rf.Schema == nil {
			return &InvalidRequestError{
				Message: "text.format.schema is required when type=json_schema",
				Param:   "text.format.schema",
			}
		}
	default:
		return &InvalidRequestError{
			Message: fmt.Sprintf("text.format.type=%q is not supported", rf.Type),
			Param:   "text.format.type",
		}
	}
	return nil
}

// InvalidRequestError is the spec-shaped 400 returned by ValidateRequest.
type InvalidRequestError struct {
	Message string
	Param   string
	Code    string
}

// Error implements `error`.
func (e *InvalidRequestError) Error() string {
	return fmt.Sprintf("invalid_request_error: %s (param=%s)", e.Message, e.Param)
}

// Body returns the JSON envelope shaped like an OpenAI Responses error.
func (e *InvalidRequestError) Body() []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.Message,
			"type":    "invalid_request_error",
			"param":   nullString(e.Param),
			"code":    nullString(e.Code),
		},
	})
	return b
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ToChatRequest converts a Responses-style request into the canonical Chat
// shape used internally. v1 supports text-only input (string or message-array
// of plain user text). Function tools, reasoning settings, structured output,
// and stop sequences are mapped onto ChatRequest fields so the provider
// adapters (openai_chat.go / anthropic.go) own wire-level translation.
func ToChatRequest(req *Request) (*ChatRequest, error) {
	if err := ValidateRequest(req); err != nil {
		return nil, err
	}
	out := &ChatRequest{
		Model:          req.Model,
		Stream:         req.Stream,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxOutputTokens: req.MaxOutputTokens,
		ToolChoice:     req.ToolChoice,
		Reasoning:      req.Reasoning,
		ResponseFormat: nil,
	}

	for _, t := range req.Tools {
		// Only forward function tools to the upstream LLM. Client-side tool
		// types (custom, namespace, tool_search, etc.) are handled by the
		// Codex client locally and must not be sent to the upstream — the
		// upstream will reject them. Silently drop anything that isn't
		// type=function.
		if t.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, ChatTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Strict:      t.Strict != nil && *t.Strict,
		})
	}

	if req.Text != nil && req.Text.Format != nil {
		out.ResponseFormat = req.Text.Format
	}

	// Map the instruction string to a leading system message; provider
	// adapters will move it to whichever top-level field the wire requires.
	if s := strings.TrimSpace(req.Instructions); s != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: s})
		out.System = s
	}

	// Translate input into canonical messages. For phase 1, string input
	// becomes a single user message; message-array input is preserved as
	// roles and concatenated text. Unknown item types are dropped with a
	// descriptive error so callers don't get silent data loss.
	if req.Input.IsString {
		out.Messages = append(out.Messages, ChatMessage{
			Role:    "user",
			Content: req.Input.StringVal,
		})
	} else {
		for _, item := range req.Input.Items {
			switch it := item.(type) {
			case *InputMessage:
				role := strings.ToLower(strings.TrimSpace(it.Role))
				if role == "" {
					role = "user"
				}
				content, parts := inputMessageContent(it)
				m := ChatMessage{
					Role:        role,
					Content:     content,
					ContentParts: parts,
				}
				out.Messages = append(out.Messages, m)
			case *InputFunctionCallOutput:
				// The Responses API client sends tool results as
				// function_call_output items. Convert to a Chat Completions
				// tool-role message so the upstream sees the required
				//   assistant(tool_calls) → tool(result) → user
				// sequence and doesn't reject the request with 400.
				out.Messages = append(out.Messages, ChatMessage{
					Role:       "tool",
					Content:    it.Output,
					ToolCallID: it.CallID,
				})
			case unknownInputItem:
				// Silently drop items we don't model (e.g. reasoning summaries)
				// so the request can still proceed with the fields we do handle.
				// Only reject if the type is something that would clearly corrupt
				// the conversation (none identified yet).
			default:
				return nil, &InvalidRequestError{
					Message: "unsupported input item",
					Param:   fmt.Sprintf("input[%d]", item.ItemIndex()),
					Code:    "unsupported_input",
				}
			}
		}
	}

	return out, nil
}

// inputMessageContent reduces a message's content to a (string, []parts) pair.
// Plain-text messages collapse to a single string for the OpenAI wire path;
// multi-part content (text + image) keeps ContentParts populated for the
// Anthropic path.
func inputMessageContent(m *InputMessage) (string, []ChatContentPart) {
	if len(m.Content) == 0 {
		return "", nil
	}
	hasMulti := false
	for _, c := range m.Content {
		if c.Kind != "input_text" {
			hasMulti = true
			break
		}
	}
	if !hasMulti {
		var sb strings.Builder
		for i, c := range m.Content {
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
		return sb.String(), nil
	}
	var textStr strings.Builder
	parts := make([]ChatContentPart, 0, len(m.Content))
	for _, c := range m.Content {
		switch c.Kind {
		case "input_text":
			if textStr.Len() > 0 {
				textStr.WriteByte('\n')
			}
			textStr.WriteString(c.Text)
			parts = append(parts, ChatContentPart{Type: "text", Text: c.Text})
		case "input_image":
			parts = append(parts, ChatContentPart{
				Type:      "image_url",
				ImageURL:  c.URL,
				Detail:    c.Detail,
			})
		default:
			// unknown part: best-effort text
			if c.Text != "" {
				if textStr.Len() > 0 {
					textStr.WriteByte('\n')
				}
				textStr.WriteString(c.Text)
			}
		}
	}
	return textStr.String(), parts
}
