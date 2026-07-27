package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StreamConverter carries request-scoped state for a single streaming response
// and emits the Responses API SSE event chain. One instance is allocated per
// /v1/responses call, populated via Process() for each upstream delta, and
// finalized via Complete() (success) or Fail() (committed-stream failure).
//
// Wire invariants enforced here:
//
//   - Sequence numbers are strictly monotonic across the whole stream — each
//     event allocation bumps c.seq before forming the payload, so duplicates
//     and out-of-order allocations are caught at the source.
//   - Exactly one terminal event per stream: either response.completed (from
//     Complete) or response.failed (from Fail). Calling both produces a
//     contradictory terminal which the handler MUST avoid.
//   - IDs are minted at construction and remain stable across every event.
//   - Phase 3 supports multi-output-item streams: text message + function_call
//     items, plus reasoning items. Output indexes reflect the order in which
//     items first appeared.
type StreamConverter struct {
	req     *Request
	ids     ResponseIDs
	model   string
	created int64

	seq int64

	started bool

	// Output items by output_index. Index 0 is reserved for the first
	// item (typically an OutputMessage). New items are appended on demand.
	outputs []OutputItem
	// Per-item state. Stored in struct form so we can emit the right
	// close-event sequence at Complete().
	itemState   []itemState
	// Used to fold streaming tool fragments into the right tool-call item.
	toolCallsByIndex map[int]*toolCallBuilder
	// Tracks whether the converter has already emitted the output_text.done
	// sequence for the message item (so we don't double-close on multiple
	// text bursts followed by a tool call).
}

type itemState struct {
	ID         string
	Kind       string // "message" | "function_call" | "reasoning"
	TextOpen   bool   // for "message"
	Final      bool
}

type toolCallBuilder struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Index     int
	OutputIdx int
	ItemID    string
}

// NewStreamConverter allocates the converter and locks in the IDs that every
// subsequent event will reuse. Callers must supply a non-nil request and a
// stable ID set (typically minted by the handler before upstream dispatch).
func NewStreamConverter(req *Request, ids ResponseIDs) *StreamConverter {
	return &StreamConverter{
		req:              req,
		ids:              ids,
		toolCallsByIndex: map[int]*toolCallBuilder{},
	}
}

// Start locks the model + timestamp in place. If model is empty, the request
// model is used. If created is zero, time.Now() is used.
func (c *StreamConverter) Start(model string, created int64) {
	if c.started {
		return
	}
	c.started = true
	if model != "" {
		c.model = model
	} else if c.req != nil {
		c.model = c.req.Model
	}
	if created > 0 {
		c.created = created
	} else {
		c.created = time.Now().Unix()
	}
}

// Model returns the model name currently locked into the converter.
func (c *StreamConverter) Model() string {
	if c.model != "" {
		return c.model
	}
	if c.req != nil {
		return c.req.Model
	}
	return ""
}

// Process ingests one canonical ChatDelta and returns 0..n Responses SSE
// events. Call it once per upstream delta. Handles text, reasoning, and
// tool-call deltas in the same call.
func (c *StreamConverter) Process(delta ChatDelta) ([]StreamEvent, error) {
	var events []StreamEvent
	if !c.started {
		c.Start(c.model, c.created)
		events = append(events,
			c.emitEvent("response.created", func(seq int64) StreamEventData {
				return streamEnvelopeForResponse(c.responseShell(), 0, nil)
			}),
			c.emitEvent("response.in_progress", func(seq int64) StreamEventData {
				return streamEnvelopeForResponse(c.responseShell(), 0, nil)
			}),
		)
	}

	// Order matters: a delta may carry multiple fragment types. We process
	// them in a deterministic order: reasoning → tool calls → content. The
	// order they hit the wire is also the order items get output_indexes,
	// which makes event-order assertions stable for downstream clients.

	if delta.ReasoningDelta != "" {
		evs, err := c.appendReasoning(delta.ReasoningDelta)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
	if delta.ReasoningSummaryDelta != "" {
		evs, err := c.appendReasoning(delta.ReasoningSummaryDelta)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
	for _, frag := range delta.ToolCallFragments {
		evs, err := c.appendToolCall(frag)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
	for _, tc := range delta.ToolCalls {
		// Non-streaming tool call surface (rare in streamed responses; used
		// when the provider sends one chunk containing a complete tool call).
		evs, err := c.completeToolCall(tc)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
	if delta.ContentDelta != "" {
		evs, err := c.appendText(delta.ContentDelta)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
	return events, nil
}

// Complete emits the closing chain for every active item, then the terminal
// response.completed event. `usage` is the canonical usage assembled by the
// provider normalizer.
func (c *StreamConverter) Complete(usage ChatUsage) ([]StreamEvent, *Response, error) {
	if !c.started {
		c.Start(c.model, c.created)
	}
	var events []StreamEvent
	if !c.started || true {
		// Always re-emit created+in_progress if caller skipped Process
		// (rare but possible when an empty stream ended cleanly).
		events = append(events,
			c.emitEvent("response.created", func(seq int64) StreamEventData {
				return streamEnvelopeForResponse(c.responseShell(), 0, nil)
			}),
			c.emitEvent("response.in_progress", func(seq int64) StreamEventData {
				return streamEnvelopeForResponse(c.responseShell(), 0, nil)
			}),
		)
	}
	// Close any still-open items in output_index order.
	for _, close := range c.closeOpenItems() {
		events = append(events, close...)
	}
	resp := c.assembleResponse(usage, "completed")
	completed := c.emitEvent("response.completed", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.completed",
			SequenceNumber: seq,
			Response:       resp,
		}
	})
	events = append(events, completed)
	return events, resp, nil
}

// Fail emits a single response.failed event after a stream was already
// committed. Wire-invariant: HTTP 200 was already committed; SSE carries
// the failure. Returns the events and the snapshot of the failed response.
func (c *StreamConverter) Fail(code, message string) ([]StreamEvent, *Response, error) {
	if !c.started {
		c.Start(c.model, c.created)
	}
	resp := c.assembleResponse(ChatUsage{}, "failed")
	resp.Error = &ResponseErr{Code: code, Message: message}
	return []StreamEvent{c.emitEvent("response.failed", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.failed",
			SequenceNumber: seq,
			Response:       resp,
		}
	})}, resp, nil
}

// Snapshot returns the converter's accumulated output items for persistence.
func (c *StreamConverter) Snapshot() []OutputItem {
	out := make([]OutputItem, len(c.outputs))
	copy(out, c.outputs)
	return out
}

// emitEvent allocates a sequence number, increments c.seq, and returns the
// event. Each emitted event consumes exactly one sequence number.
func (c *StreamConverter) emitEvent(name string, fn func(seq int64) StreamEventData) StreamEvent {
	c.seq++
	data := fn(c.seq)
	if data.Type == "" {
		data.Type = name
	}
	return StreamEvent{Event: name, Data: data}
}

// ensureMessageItem returns the shared message output item, allocating it
// (and pushing it onto c.outputs + c.itemState) on first use.
func (c *StreamConverter) ensureMessageItem() *OutputMessage {
	for _, o := range c.outputs {
		if m, ok := o.(*OutputMessage); ok {
			return m
		}
	}
	// First item — allocate.
	msg := &OutputMessage{
		Type:    "message",
		ID:      c.ids.MessageID,
		Role:    "assistant",
		Status:  "in_progress",
		Content: []OutputContent{{Type: "output_text", Text: ""}},
	}
	c.outputs = append(c.outputs, msg)
	c.itemState = append(c.itemState, itemState{ID: msg.ID, Kind: "message"})
	return msg
}

func (c *StreamConverter) appendText(text string) ([]StreamEvent, error) {
	var events []StreamEvent
	msg := c.ensureMessageItem()
	state := &c.itemState[0]

	if !state.TextOpen {
		// output_item.added
		idx := len(c.outputs) - 1
		addItem := c.emitEvent("response.output_item.added", func(seq int64) StreamEventData {
			return StreamEventData{
				Type:           "response.output_item.added",
				SequenceNumber: seq,
				OutputIndex:    idx,
				Item:           msg,
			}
		})
		// content_part.added
		itemID := msg.ID
		addPart := c.emitEvent("response.content_part.added", func(seq int64) StreamEventData {
			return StreamEventData{
				Type:           "response.content_part.added",
				SequenceNumber: seq,
				OutputIndex:    idx,
				ContentIndex:   0,
				ItemID:         itemID,
			}
		})
		state.TextOpen = true
		events = append(events, addItem, addPart)
	}
	idx := len(c.outputs) - 1
	itemID := msg.ID
	delta := c.emitEvent("response.output_text.delta", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.output_text.delta",
			SequenceNumber: seq,
			OutputIndex:    idx,
			ContentIndex:   0,
			ItemID:         itemID,
			Delta:          text,
		}
	})
	msg.Content[0].Text += text
	events = append(events, delta)
	return events, nil
}

// appendReasoning pushes a new reasoning item (or extends the most recent)
// and emits response.reasoning_text.delta. Reasoning items start a fresh
// output_index per occurrence.
func (c *StreamConverter) appendReasoning(text string) ([]StreamEvent, error) {
	// Look for an active reasoning item to extend.
	for i, o := range c.outputs {
		if r, ok := o.(*OutputReasoning); ok && c.itemState[i].Kind == "reasoning" {
			itemID := r.ID
			idx := i
			delta := c.emitEvent("response.reasoning_text.delta", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.reasoning_text.delta",
					SequenceNumber: seq,
					OutputIndex:    idx,
					ItemID:         itemID,
					Delta:          text,
				}
			})
			r.Content = append(r.Content, OutputContent{Type: "reasoning_text", Text: text})
			return []StreamEvent{delta}, nil
		}
	}
	// New reasoning item.
	rid := NewReasoningItemID()
	r := &OutputReasoning{
		Type:    "reasoning",
		ID:      rid,
		Status:  "in_progress",
		Content: []OutputContent{{Type: "reasoning_text", Text: text}},
	}
	c.outputs = append(c.outputs, r)
	c.itemState = append(c.itemState, itemState{ID: rid, Kind: "reasoning"})
	idx := len(c.outputs) - 1
	addItem := c.emitEvent("response.output_item.added", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.output_item.added",
			SequenceNumber: seq,
			OutputIndex:    idx,
			Item:           r,
		}
	})
	itemID := r.ID
	delta := c.emitEvent("response.reasoning_text.delta", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.reasoning_text.delta",
			SequenceNumber: seq,
			OutputIndex:    idx,
			ItemID:         itemID,
			Delta:          text,
		}
	})
	return []StreamEvent{addItem, delta}, nil
}

// appendToolCall accumulates a streaming tool-call fragment and emits
// response.function_call_arguments.delta. The first fragment for a given
// (Index, ID) tuple also emits response.output_item.added(type=function_call).
func (c *StreamConverter) appendToolCall(frag ChatToolCallFragment) ([]StreamEvent, error) {
	tc, ok := c.toolCallsByIndex[frag.Index]
	if !ok {
		fcid := frag.ID
		if fcid == "" {
			fcid = NewFunctionCallItemID()
		}
		tc = &toolCallBuilder{
			ID:        fcid,
			Name:      frag.Name,
			Index:     frag.Index,
			OutputIdx: len(c.outputs),
			ItemID:    fcid,
		}
		if frag.Name != "" {
			tc.Name = frag.Name
		}
		c.toolCallsByIndex[frag.Index] = tc
		// Allocate output_item.
		call := &OutputFunctionCall{
			Type:      "function_call",
			ID:        fcid,
			CallID:    frag.ID,
			Name:      frag.Name,
			Arguments: "",
			Status:    "in_progress",
		}
		if call.Name == "" {
			call.Name = tc.Name
		}
		c.outputs = append(c.outputs, call)
		c.itemState = append(c.itemState, itemState{ID: fcid, Kind: "function_call"})
		tc.OutputIdx = len(c.outputs) - 1
		addItem := c.emitEvent("response.output_item.added", func(seq int64) StreamEventData {
			return StreamEventData{
				Type:           "response.output_item.added",
				SequenceNumber: seq,
				OutputIndex:    tc.OutputIdx,
				Item:           call,
			}
		})
		return []StreamEvent{addItem}, nil
	}
	if frag.ID != "" && tc.ID != frag.ID {
		tc.ID = frag.ID
	}
	if frag.Name != "" && tc.Name == "" {
		tc.Name = frag.Name
	}
	if frag.Arguments == "" {
		return nil, nil
	}
	tc.Arguments.WriteString(frag.Arguments)
	idx := tc.OutputIdx
	itemID := tc.ItemID
	delta := c.emitEvent("response.function_call_arguments.delta", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.function_call_arguments.delta",
			SequenceNumber: seq,
			OutputIndex:    idx,
			ItemID:         itemID,
			Arguments:      frag.Arguments,
		}
	})
	return []StreamEvent{delta}, nil
}

// completeToolCall emits the close events for a tool call whose full payload
// arrived in one chunk (rare in streaming, common in non-streaming).
func (c *StreamConverter) completeToolCall(call ChatToolCall) ([]StreamEvent, error) {
	frag := ChatToolCallFragment{ID: call.ID, Name: call.Function, Arguments: call.Arguments}
	evs, err := c.appendToolCall(frag)
	if err != nil {
		return nil, err
	}
	// Replace the in-progress call arguments with the final ones and
	// emit arguments.done + output_item.done.
	tc, ok := c.toolCallsByIndex[len(c.toolCallsByIndex)]
	_ = ok
	_ = tc
	idx := 0
	for i, o := range c.outputs {
		if fc, ok2 := o.(*OutputFunctionCall); ok2 && fc.CallID == call.ID {
			idx = i
			fc.Arguments = call.Arguments
			fc.Status = "completed"
		}
	}
	// Find call by ID.
	var item *OutputFunctionCall
	for _, o := range c.outputs {
		if fc, ok2 := o.(*OutputFunctionCall); ok2 && fc.CallID == call.ID {
			item = fc
			break
		}
	}
	if item == nil {
		return evs, nil
	}
	done := c.emitEvent("response.function_call_arguments.done", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.function_call_arguments.done",
			SequenceNumber: seq,
			OutputIndex:    idx,
			ItemID:         item.ID,
			Arguments:      item.Arguments,
		}
	})
	itemDone := c.emitEvent("response.output_item.done", func(seq int64) StreamEventData {
		return StreamEventData{
			Type:           "response.output_item.done",
			SequenceNumber: seq,
			OutputIndex:    idx,
			ItemID:         item.ID,
			Item:           item,
		}
	})
	c.itemState[idx].Final = true
	return append(evs, done, itemDone), nil
}

// closeOpenItems emits the close sequence for every item still in flight
// (text message parts, function-call arguments, reasoning items). Returns
// a slice of event batches in output_index order.
func (c *StreamConverter) closeOpenItems() [][]StreamEvent {
	var out [][]StreamEvent
	for i, o := range c.outputs {
		switch item := o.(type) {
		case *OutputMessage:
			if !c.itemState[i].TextOpen {
				continue
			}
			item.Status = "completed"
			ev1 := c.emitEvent("response.output_text.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.output_text.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ContentIndex:   0,
					ItemID:         item.ID,
					Item:           item,
				}
			})
			ev2 := c.emitEvent("response.content_part.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.content_part.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ContentIndex:   0,
					ItemID:         item.ID,
					Item:           item,
				}
			})
			ev3 := c.emitEvent("response.output_item.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.output_item.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ItemID:         item.ID,
					Item:           item,
				}
			})
			c.itemState[i].Final = true
			out = append(out, []StreamEvent{ev1, ev2, ev3})
		case *OutputFunctionCall:
			if c.itemState[i].Final {
				continue
			}
			item.Status = "completed"
			ev1 := c.emitEvent("response.function_call_arguments.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.function_call_arguments.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ItemID:         item.ID,
					Arguments:      item.Arguments,
				}
			})
			ev2 := c.emitEvent("response.output_item.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.output_item.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ItemID:         item.ID,
					Item:           item,
				}
			})
			c.itemState[i].Final = true
			out = append(out, []StreamEvent{ev1, ev2})
		case *OutputReasoning:
			if c.itemState[i].Final {
				continue
			}
			item.Status = "completed"
			ev := c.emitEvent("response.output_item.done", func(seq int64) StreamEventData {
				return StreamEventData{
					Type:           "response.output_item.done",
					SequenceNumber: seq,
					OutputIndex:    i,
					ItemID:         item.ID,
					Item:           item,
				}
			})
			c.itemState[i].Final = true
			out = append(out, []StreamEvent{ev})
		}
	}
	return out
}

func (c *StreamConverter) responseShell() *Response {
	resp := &Response{
		Object:             "response",
		ID:                 c.ids.ResponseID,
		CreatedAt:          c.created,
		Status:             "in_progress",
		Model:              c.Model(),
		PreviousResponseID: c.req.PreviousResponseID,
		Temperature:        c.req.Temperature,
		TopP:               c.req.TopP,
		MaxOutputTokens:    c.req.MaxOutputTokens,
		Tools:              c.req.Tools,
		ToolChoice:         c.req.ToolChoice,
		ParallelToolCalls:  c.req.ParallelToolCalls,
	}
	if c.req != nil && c.req.Conversation != nil && c.req.Conversation.ID != "" {
		resp.Conversation = &Conversation{ID: c.req.Conversation.ID}
	}
	return resp
}

func (c *StreamConverter) assembleResponse(usage ChatUsage, status string) *Response {
	resp := c.responseShell()
	resp.Status = status
	resp.Usage = Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if usage.CachedTokens > 0 {
		resp.Usage.InputTokensDetails = &InputTokensDetails{CachedTokens: usage.CachedTokens}
	}
	if usage.ReasoningTokens > 0 {
		resp.Usage.OutputTokensDetails = &OutputTokensDetails{ReasoningTokens: usage.ReasoningTokens}
	}
	resp.Output = make([]OutputItem, len(c.outputs))
	copy(resp.Output, c.outputs)
	// Concatenated text convenience field, drawn from message items only.
	for _, o := range c.outputs {
		if m, ok := o.(*OutputMessage); ok && len(m.Content) > 0 {
			resp.OutputText += m.Content[0].Text
		}
	}
	// Tool-call argument fall-back: if a streaming tool call didn't see a
	// closing fragment, the builder's accumulated string still holds the
	// truth. Sync it back so persistence has complete arguments.
	for _, o := range resp.Output {
		if fc, ok := o.(*OutputFunctionCall); ok {
			if fc.Arguments == "" {
				for _, b := range c.toolCallsByIndex {
					if b.ID == fc.ID {
						fc.Arguments = b.Arguments.String()
					}
				}
			}
		}
	}
	_ = strings.HasPrefix  // keep "strings" import live during refactors
	return resp
}

// streamEnvelopeForResponse wraps a Response object in a StreamEventData for
// the lifecycle events (response.created, response.in_progress). Only used
// via emitEvent, so callers don't override the type discriminator.
func streamEnvelopeForResponse(r *Response, outputIndex int, item OutputItem) StreamEventData {
	return StreamEventData{
		Response:    r,
		OutputIndex: outputIndex,
	}
}

// MarshalEvent serializes an event's Data payload to JSON suitable for
// `data: <payload>\n\n`.
func MarshalEvent(ev StreamEvent) ([]byte, error) {
	b, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal event %s: %w", ev.Event, err)
	}
	return b, nil
}
