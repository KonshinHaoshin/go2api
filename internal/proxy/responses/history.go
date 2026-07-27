package responses

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/user/go2api/internal/store"
)

// storedEnvelope is the on-disk shape of a persisted response state row.
// We store output items as raw JSON so we can read them back without knowing
// the concrete type at unmarshal time (OutputItem is an interface).
type storedEnvelope struct {
	Output *storedResponse `json:"output"`
}

type storedResponse struct {
	Output []json.RawMessage `json:"output"`
}

// ResolveHistory loads prior response state and converts it back to a
// list of canonical ChatMessages. The returned slice, when prepended to
// the new request's chat messages, reconstructs the conversation the
// original caller is referring to via `previous_response_id`. Returns
// (nil, nil, nil) when no previous_response_id is set.
//
// Errors:
//   - state row not found or expired → ErrUnknownPrevResponse
//   - conversation tail mismatch    → ErrChainConflict
//   - state row malformed           → ErrCorruptState
func ResolveHistory(ctx context.Context, db *store.DB, req *Request) ([]ChatMessage, *store.ResponseStateRow, error) {
	if req.PreviousResponseID == "" {
		return nil, nil, nil
	}
	if db == nil {
		return nil, nil, nil
	}
	row, err := db.GetResponseState(ctx, req.PreviousResponseID)
	if err != nil {
		return nil, nil, ErrUnknownPrevResponse
	}
	// Enforce conversation tail constraint when both are supplied.
	if req.Conversation != nil && req.Conversation.ID != "" {
		conv, err := db.GetConversation(ctx, req.Conversation.ID)
		if err == nil {
			if conv.LastResponseID != "" && conv.LastResponseID != req.PreviousResponseID {
				return nil, &row, ErrChainConflict
			}
		}
		// If the conversation row doesn't exist yet, that's fine for the first
		// read pass — the caller will create it at write time.
	}

	var env storedEnvelope
	if err := json.Unmarshal(row.ItemsEnvelope, &env); err != nil {
		return nil, &row, ErrCorruptState
	}
	if env.Output == nil {
		// Legacy envelope from Phase 1 may not have the "output" key; treat
		// as an empty prior history (no replay, but not an error).
		return nil, &row, nil
	}
	history := stateToHistory(env.Output.Output)
	return history, &row, nil
}

// stateToHistory converts a slice of raw JSON output items back into canonical
// ChatMessages. Each item is decoded minimally: we read its "type" field and
// pull whichever fields are needed for history replay.
//
// IMPORTANT: When a function_call item appears in history, a synthetic tool
// result message is inserted immediately after it. Without this the resulting
// message sequence would be:
//
//   user → assistant(tool_calls) → user   ← upstream rejects with 400
//
// With the synthetic result:
//
//   user → assistant(tool_calls) → tool("") → user  ← valid
//
// If the new request's input carries a real function_call_output item (the
// client executed the tool and sent the result), ToChatRequest converts it to
// a tool message that replaces the empty placeholder. That replacement is
// positional — the empty synthetic message is appended first (from history),
// and the real result comes from the input array, so the real result lands
// AFTER the empty one. To avoid duplication, synthetic messages are omitted
// when the corresponding call_id is already present in the input.
// For simplicity in v1 we always insert the empty placeholder; if the client
// sends the real result via function_call_output the upstream will see both,
// which most models handle gracefully (they use the last one).
func stateToHistory(rawItems []json.RawMessage) []ChatMessage {
	if len(rawItems) == 0 {
		return nil
	}
	var out []ChatMessage
	for _, raw := range rawItems {
		var probed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probed); err != nil {
			continue
		}
		switch probed.Type {
		case "message":
			var m struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			var sb strings.Builder
			for i, c := range m.Content {
				if c.Type == "output_text" || c.Type == "text" {
					if i > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(c.Text)
				}
			}
			out = append(out, ChatMessage{Role: "assistant", Content: sb.String()})
		case "function_call":
			var fc struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &fc); err != nil {
				continue
			}
			if fc.CallID == "" {
				continue
			}
			tc := ChatToolCall{
				ID:        fc.CallID,
				Type:      "function",
				Function:  fc.Name,
				Arguments: fc.Arguments,
			}
			out = append(out, ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{tc}})
		case "reasoning":
			var r struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				continue
			}
			var sb strings.Builder
			for _, c := range r.Content {
				sb.WriteString(c.Text)
			}
			if sb.Len() > 0 {
				out = append(out, ChatMessage{Role: "assistant", Reasoning: sb.String()})
			}
		}
	}
	return out
}

// MergeHistory prepends history messages to the chat request's existing
// messages, then calls ensureToolCallsAnswered to patch any assistant messages
// with tool_calls that are not followed by matching tool-role messages.
func MergeHistory(chat *ChatRequest, history []ChatMessage) {
	if len(history) == 0 || chat == nil {
		return
	}
	systemLen := 0
	for _, m := range chat.Messages {
		if m.Role == "system" {
			systemLen++
			continue
		}
		break
	}
	prefix := chat.Messages[:systemLen]
	rest := chat.Messages[systemLen:]
	merged := make([]ChatMessage, 0, len(prefix)+len(history)+len(rest))
	merged = append(merged, prefix...)
	merged = append(merged, history...)
	merged = append(merged, rest...)
	chat.Messages = ensureToolCallsAnswered(merged)
}

// AppendConversationResponse ensures the conversation row exists and appends
// the new response ID atomically. Returns ErrChainConflict when
// previous_response_id disagrees with the recorded tail.
func AppendConversationResponse(ctx context.Context, db *store.DB, conversationID, previousID, newID string) error {
	if db == nil || conversationID == "" {
		return nil
	}
	conv, err := db.GetConversation(ctx, conversationID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrConversationNotFound) || err != nil {
		// First response in this conversation: create the row.
		return db.PutConversation(ctx, store.ConversationRow{
			ID:             conversationID,
			CreatedAt:      time.Now(),
			LastResponseID: newID,
			ResponseIDs:    []string{newID},
		})
	}
	// Validate chain ordering.
	if len(conv.ResponseIDs) > 0 {
		if conv.LastResponseID != previousID {
			return ErrChainConflict
		}
	} else if previousID != "" {
		return ErrChainConflict
	}
	return db.AppendConversationResponse(ctx, conversationID, newID)
}

// ensureToolCallsAnswered scans a message slice and inserts a synthetic empty
// tool-result message after every assistant message with tool_calls that is
// NOT already followed by matching tool messages. This prevents the upstream
// from rejecting the request with 400 due to an invalid message sequence
// (assistant with tool_calls must be followed by tool messages before any
// subsequent user turn).
//
// If the client already sent real function_call_output items (converted to
// tool messages by ToChatRequest), those are preserved and no duplicate
// placeholder is inserted.
func ensureToolCallsAnswered(msgs []ChatMessage) []ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]ChatMessage, 0, len(msgs)+4)
	for i, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		// Collect which call IDs already have a tool-role answer in the
		// immediately following messages.
		answered := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			answered[msgs[j].ToolCallID] = true
		}
		// For each unanswered tool call, insert a synthetic empty result.
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, ChatMessage{
					Role:       "tool",
					Content:    "",
					ToolCallID: tc.ID,
				})
			}
		}
	}
	return out
}
var (
	ErrUnknownPrevResponse = errors.New("responses: previous_response_id not found or expired")
	ErrChainConflict       = errors.New("responses: previous_response_id does not match conversation tail")
	ErrCorruptState        = errors.New("responses: stored state row is corrupt")
)
