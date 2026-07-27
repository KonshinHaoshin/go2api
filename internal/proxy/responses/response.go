package responses

import "time"

// FromChatResponse assembles a non-streaming Response object from a canonical
// ChatResponse. The IDs are reused from the supplied ResponseIDs so they remain
// stable across subsequent stream events and persisted state.
//
// The output mirrors the OpenAI Responses wire shape: object "response",
// status "completed", a concatenated `output_text` convenience field, and a
// fully populated `usage` block.
func FromChatResponse(req *Request, chat ChatResponse, ids ResponseIDs) Response {
	now := time.Now().Unix()

	resp := Response{
		Object:        "response",
		ID:            ids.ResponseID,
		CreatedAt:     now,
		Status:        "completed",
		Model:         chat.Model,
		PreviousResponseID: req.PreviousResponseID,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		MaxOutputTokens: req.MaxOutputTokens,
		Tools:         req.Tools,
		ToolChoice:    req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
		Usage: Usage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
			TotalTokens:  chat.Usage.TotalTokens,
		},
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if chat.Usage.CachedTokens > 0 {
		resp.Usage.InputTokensDetails = &InputTokensDetails{
			CachedTokens: chat.Usage.CachedTokens,
		}
	}
	if chat.Usage.ReasoningTokens > 0 {
		resp.Usage.OutputTokensDetails = &OutputTokensDetails{
			ReasoningTokens: chat.Usage.ReasoningTokens,
		}
	}

	if req.Conversation != nil && req.Conversation.ID != "" {
		resp.Conversation = &Conversation{ID: req.Conversation.ID}
	}

	if chat.Content != "" {
		msg := &OutputMessage{
			Type:   "message",
			ID:     ids.MessageID,
			Role:   "assistant",
			Status: "completed",
			Content: []OutputContent{{
				Type: "output_text",
				Text: chat.Content,
			}},
		}
		resp.Output = append(resp.Output, msg)
		resp.OutputText = chat.Content
	}

	for idx, tc := range chat.ToolCalls {
		fcid := tc.ID
		if fcid == "" {
			fcid = ids.FunctionCallIDs[idx]
		}
		if fcid == "" {
			fcid = NewFunctionCallItemID()
		}
		resp.Output = append(resp.Output, &OutputFunctionCall{
			Type:      "function_call",
			ID:        fcid,
			CallID:    tc.ID,
			Name:      tc.Function,
			Arguments: tc.Arguments,
			Status:    "completed",
		})
	}

	if chat.Reasoning != "" {
		rid := NewReasoningItemID()
		resp.Output = append(resp.Output, &OutputReasoning{
			Type:    "reasoning",
			ID:      rid,
			Content: []OutputContent{{Type: "reasoning_text", Text: chat.Reasoning}},
			Status:  "completed",
		})
	}

	return resp
}

// ResponseIDs carries the stable identifiers used inside one response stream.
// They are minted by the handler before upstream dispatch so events and the
// final persisted row all share the same IDs.
type ResponseIDs struct {
	ResponseID       string
	MessageID        string
	FunctionCallIDs  []string
}
