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
// pull whichever fields are needed for history replay without requiring full
// type registration.
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
// messages. The system/developer message(s) stay first; history is sandwiched
// immediately after them, before the new user turn.
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
	chat.Messages = merged
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

// wire errors
var (
	ErrUnknownPrevResponse = errors.New("responses: previous_response_id not found or expired")
	ErrChainConflict       = errors.New("responses: previous_response_id does not match conversation tail")
	ErrCorruptState        = errors.New("responses: stored state row is corrupt")
)
