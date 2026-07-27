// Package responses provides the OpenAI Responses API surface (POST /v1/responses)
// on top of the existing Chat Completions and Anthropic Messages upstreams.
//
// The package ports the wire models and state-machine from Ollama's openai
// adapter (MIT) without depending on Ollama itself; canonical Chat structs
// stand in for Ollama's api.ChatRequest / api.ChatResponse. IDs are
// ULID-backed so they get collision-resistant, lexically-sortable identifiers
// across response, conversation, message, function-call, and reasoning items.
//
// The handler in /internal/handler/responses.go owns HTTP routing, key pool,
// retries, SSE framing, persistence, and cancellation. This package owns
// request conversion, response conversion, and stateful streaming.
package responses

import (
	"time"

	"github.com/oklog/ulid/v2"
)

// ID prefixes used in the Responses API wire format. IDs are surfaced to
// clients; they must look like "resp_…", "msg_…", etc.
const (
	PrefixResponse     = "resp_"
	PrefixConversation = "conv_"
	PrefixMessage      = "msg_"
	PrefixFunctionCall = "fc_"
	PrefixReasoning    = "rs_"
)

func mintID(prefix string) string {
	return prefix + ulid.MustNewDefault(time.Now()).String()
}

// NewResponseID returns a fresh "resp_<ULID>" identifier.
func NewResponseID() string { return mintID(PrefixResponse) }

// NewConversationID returns a fresh "conv_<ULID>" identifier.
func NewConversationID() string { return mintID(PrefixConversation) }

// NewMessageItemID returns a fresh "msg_<ULID>" identifier for a message output item.
func NewMessageItemID() string { return mintID(PrefixMessage) }

// NewFunctionCallItemID returns a fresh "fc_<ULID>" identifier for a function_call item.
func NewFunctionCallItemID() string { return mintID(PrefixFunctionCall) }

// NewReasoningItemID returns a fresh "rs_<ULID>" identifier for a reasoning item.
func NewReasoningItemID() string { return mintID(PrefixReasoning) }
