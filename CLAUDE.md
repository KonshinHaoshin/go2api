# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

Go backend (run from repo root):

```bash
make run              # go run ./cmd/server -config configs/config.yaml
make build            # bin/go2api
make fmt && make vet  # format + vet before committing
go test ./...         # all tests
go test ./internal/proxy/responses -run TestToChatRequest -v   # single package / test
go test -race ./internal/proxy/responses ./internal/handler ./internal/store
```

Web UI (run from `web/`):

```bash
npm run dev           # tsx watch server.ts (no build step)
npm run build && npm start
npm run typecheck
```

There is no `make test` target. `go test ./...` is canonical. `make fmt` and `make vet` are the only lint steps.

## Architecture

### Two runtimes, one repo

- **Go backend** (`module github.com/user/go2api`, Go 1.25): the proxy. Entry point `cmd/server/main.go`.
- **Node web UI** (`web/`): TypeScript Express app. Reads config from the **project-root `.env`** (not `web/.env`). `GO2API_TOKEN` in `.env` must match one of `server.auth_tokens` in `config.yaml`, or UI upstream calls get 401.

`.env` and `configs/config.yaml` are independent — do not conflate them.

### Request routing

Every incoming request is routed through a **model family** → **upstream endpoint** lookup:

```
client request
    → internal/proxy/families.go :: ModelFamily(model) → FamilyOpenAI | FamilyAnthropic
    → EndpointForFamily(family)  → /chat/completions  | /messages
```

`DefaultModelFamilies` in `internal/proxy/families.go` is the canonical table. **When adding a new model, add it here first**, otherwise it defaults to OpenAI and may 404 upstream.

### Three API surfaces

| Client sends | Handler | What happens |
|---|---|---|
| `POST /v1/chat/completions` | `handler.OpenAI` | Native or cross-format (Anthropic-family models auto-converted) |
| `POST /v1/messages` | `handler.Anthropic` | Native or cross-format (OpenAI-family models auto-converted) |
| `POST /v1/responses` | `handler.Responses` | Full OpenAI Responses API; converts to Chat/Messages upstream and back |

ChatGPT App and Codex hard-code `/v1/responses` and cannot be reconfigured — the Responses handler is required for them to work.

### Responses API (`internal/proxy/responses/`)

New subpackage added to bridge the OpenAI Responses API onto existing upstreams. Architecture:

```
handler.Responses (handler/responses.go)
    ├── ValidateRequest  → rejects background=true, file_id, hosted tools, large images
    ├── ResolveHistory   → reads previous_response_id from response_state table
    ├── MergeHistory     → prepends prior assistant turns to ChatRequest.Messages
    ├── ToChatRequest    → Responses wire → canonical ChatRequest
    ├── ToOpenAIChatRequest / ToAnthropicRequest → ChatRequest → upstream wire
    ├── [forward via keypool + failover]
    ├── FromOpenAIChatResponse / FromAnthropicResponse → upstream wire → ChatResponse
    ├── FromChatResponse → ChatResponse + ResponseIDs → Responses Response object
    └── StreamConverter  → emits the SSE event chain (created→in_progress→…→completed|failed)
```

Key invariants:
- **No `[DONE]` ever reaches the downstream client.** Provider `[DONE]` is consumed internally; the client sees `response.completed` or `response.failed`.
- **Retries are pre-commit only.** Once the first SSE event is written (after HTTP 200), the response ID is locked in; switching keys would contradict it.
- **One terminal event per stream.** `response.completed` XOR `response.failed`.
- **OutputItem interface uses custom MarshalJSON.** `StreamEventData.Item` serializes to `"item": {...}` via `MarshalJSON`; the underlying `OutputItem` interface cannot be round-tripped through standard JSON unmarshal. The persistence layer stores items as `json.RawMessage` and `stateToHistory` reads back via `map[string]any` type-switching on the `type` field.

### Key non-obvious files

- `internal/proxy/families.go` — model → family routing table. Add new models here.
- `internal/proxy/convert.go` — **intentionally narrow**: OpenAI↔Anthropic text-only conversion for `handler.OpenAI` and `handler.Anthropic`. Does NOT cover tools, images, or structured output. The Responses API has its own full-fidelity converters in `internal/proxy/responses/`.
- `internal/proxy/proxy.go::rewriteModel` — strips `opencode-go/` prefix before forwarding. Don't remove this.
- `internal/handler/anthropic.go` — `/messages` requires `x-api-key` + `anthropic-version` headers. Both the handler and the streaming path in `responses.go` set these. Don't drop them.
- `internal/store/sqlite.go` — schema + migrations. Two tables added for Responses: `response_state` (24 h TTL, GC every 5 min) and `conversation` (ordered response chain).
- `internal/server/middleware.go` — auth accepts **both** static config tokens and DB-managed tokens. Don't assume only config tokens work.
- `cmd/server/main.go` — starts cache GC and response-state GC on `rootCtx`. GC for state is unconditional (not gated on `cache.enabled`).
- `web/server.ts` — strips browser JWT, rewrites `Authorization` to `GO2API_TOKEN`, proxies `/api/*` → `${GO2API_URL}/*`.

### Responses API feature scope

**Supported:** text (string or message-array input), inline image data URLs (up to 5 MiB, `image/*` only), function tools (`type=function`), `tool_choice`, streaming function-call argument deltas, reasoning/thinking text deltas, structured output (`json_schema`), `previous_response_id` history replay, `conversation` chain.

**Explicitly rejected (400):** `background=true`, `file_id`, `input_file`, hosted tools (`web_search`, `file_search`, `computer_use`, `code_interpreter`), remote image URLs, images > 5 MiB, non-image media types.

### Conventions

- `gin.SetMode(gin.ReleaseMode)` is set in `server.New`; don't add debug-mode calls.
- `WriteTimeout: 0` on the HTTP server is intentional — streaming governs via `upstream.timeout_seconds`. Don't "fix" it.
- Cache skips streaming requests. Responses API results are NOT cached through `cache.Cache`; state is persisted separately in `response_state`.
- IDs are ULID-based (`resp_<ULID>`, `msg_<ULID>`, `fc_<ULID>`, `conv_<ULID>`) via `github.com/oklog/ulid/v2`.
- Context must never be `nil` when calling store methods — `database/sql` will deadlock acquiring the connection pool lock.
- macOS `._*` AppleDouble files litter this checkout. Ignore them.
