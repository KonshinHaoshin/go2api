package handler

import (
	"io"
	"net/http"
)

// Models handles GET /v1/models and lists the known OpenCode Go models.
// We do not forward to upstream on every call; the catalog is static.
func Models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, catalogJSON)
}

// catalogJSON is the static model catalog for both OpenAI-compatible and
// Anthropic-compatible endpoints. The "family" field tells callers which
// endpoint the model lives behind.
const catalogJSON = `{
  "object": "list",
  "data": [
    {"id": "grok-4.5",            "object": "model", "family": "openai-compatible"},
    {"id": "glm-5.2",             "object": "model", "family": "openai-compatible"},
    {"id": "glm-5.1",             "object": "model", "family": "openai-compatible"},
    {"id": "kimi-k3",             "object": "model", "family": "openai-compatible"},
    {"id": "kimi-k2.7-code",      "object": "model", "family": "openai-compatible"},
    {"id": "kimi-k2.6",           "object": "model", "family": "openai-compatible"},
    {"id": "deepseek-v4-pro",     "object": "model", "family": "openai-compatible"},
    {"id": "deepseek-v4-flash",   "object": "model", "family": "openai-compatible"},
    {"id": "mimo-v2.5",           "object": "model", "family": "openai-compatible"},
    {"id": "mimo-v2.5-pro",       "object": "model", "family": "openai-compatible"},
    {"id": "hy3",                 "object": "model", "family": "openai-compatible"},
    {"id": "minimax-m3",          "object": "model", "family": "anthropic-compatible"},
    {"id": "minimax-m2.7",        "object": "model", "family": "anthropic-compatible"},
    {"id": "minimax-m2.5",        "object": "model", "family": "anthropic-compatible"},
    {"id": "qwen3.7-max",         "object": "model", "family": "anthropic-compatible"},
    {"id": "qwen3.7-plus",        "object": "model", "family": "anthropic-compatible"},
    {"id": "qwen3.6-plus",        "object": "model", "family": "anthropic-compatible"}
  ]
}`
