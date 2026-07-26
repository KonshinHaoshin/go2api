# AGENTS.md

Compact guidance for OpenCode sessions working in this repo. Read before editing.

## What this is

`go2api` aggregates multiple OpenCode Go subscription keys into a single API endpoint compatible with OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) formats, with SQLite-backed caching, key-pool scheduling, and failover. A separate Node.js admin UI (`web/`) sits in front of it.

## Two runtimes, one repo

- **Go backend** (`module github.com/user/go2api`, Go 1.25): the proxy itself. Entry point `cmd/server/main.go`.
- **Node web UI** (`web/`, Node >=18): TypeScript Express app (`server.ts`), compiled with `tsc` for production or run via `tsx watch` for development. Static dashboard + JWT auth + reverse proxy to the Go backend. Reads config from the **project-root `.env`** (not `web/.env`) via `dotenv`.

`.env` is shared config for the web UI. `configs/config.yaml` is Go backend config. They are independent — do not conflate them. `GO2API_TOKEN` in `.env` must match one of `server.auth_tokens` in `config.yaml`, or the web UI's upstream calls get 401.

## Commands

Go backend (run from repo root):

```bash
make run              # go run ./cmd/server -config configs/config.yaml
make build            # bin/go2api
make fmt && make vet  # format + vet before committing
go test ./...         # all tests
go test ./internal/cache -run TestKeyFor -v   # single package / single test
```

Web UI (run from `web/`):

```bash
npm install
npm run build         # tsc, outputs to dist/
npm start             # node dist/server.js, listens :8081
npm run dev           # tsx watch server.ts — no build step needed
npm run typecheck     # tsc --noEmit
```

There is no `make test` target and no CI config — `go test ./...` is the canonical test command. `make fmt` and `make vet` are the only lint-like steps.

## Layout that's not obvious from filenames

- `internal/proxy/families.go` — maps model IDs (e.g. `grok-4.5`, `minimax-m3`) to upstream families (`openai` vs `anthropic`), which decides whether a request hits `/chat/completions` or `/messages`. **When adding support for a new model, add it to `DefaultModelFamilies` here**, otherwise it defaults to OpenAI and may 404 upstream.
- `internal/proxy/convert.go` — request/response conversion between OpenAI and Anthropic formats.
- `internal/proxy/proxy.go` — `rewriteModel` **strips** the `opencode-go/` prefix before forwarding. The upstream HTTP API expects bare model IDs (e.g. `minimax-m3`); the `opencode-go/<id>` form is only for OpenCode's client config file, not API request bodies.
- `internal/proxy/proxy.go` + `internal/handler/anthropic.go` — the `/messages` (Anthropic) upstream endpoint requires `x-api-key` + `anthropic-version` headers in addition to `Authorization: Bearer`. Both call sites set these when `plan.Endpoint == "/messages"`. Don't drop them or Anthropic-family models (minimax, qwen) return `AuthError: Missing API key`.
- `internal/store/sqlite.go` — schema lives here; `data/*.db` is gitignored runtime state. `make clean` wipes `bin/` and `data/*.db`.
- `internal/server/middleware.go` — auth accepts **both** static config tokens and DB-managed tokens (per-token quota). Don't assume only config tokens work.
- `web/server.ts` — the Express app strips the browser's JWT and rewrites `Authorization` to the server-side `GO2API_TOKEN` before proxying to `${GO2API_URL}`. `/api/*` maps to `${GO2API_URL}/*` (note: `/api` prefix is stripped).

## Conventions

- Go module path is `github.com/user/go2api` (not a real upstream — don't try to fetch it). Internal packages follow the standard `internal/` layout.
- `gin.SetMode(gin.ReleaseMode)` is set explicitly in `server.New`; don't add debug-mode calls.
- `WriteTimeout` is intentionally `0` on the HTTP server so streaming responses aren't cut off — the upstream proxy timeout (`upstream.timeout_seconds`, default 300s) governs instead. Don't "fix" this.
- Cache skips streaming requests and has a 4 MiB default `max_response_bytes`. Both are configurable in `config.yaml`.
- macOS `._*` AppleDouble files litter this checkout (artifact of copying through a non-HFS-aware tool). Ignore them; they are not source.
