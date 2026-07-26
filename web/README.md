# go2api-web

A small Node.js admin UI for [go2api](../). Serves the static dashboard and reverse-proxies `/api/*` calls to the upstream go2api server, injecting the JWT-derived identity and the upstream token server-side so neither leaks to the browser.

## Layout

```
go2api/
├── .env            # ← shared config lives here at the project root
├── .env.example    # template, copy to .env
├── web/
│   ├── server.ts   # Express app: auth + static + /api/* proxy (TypeScript)
│   ├── tsconfig.json
│   ├── package.json
│   ├── dist/       # compiled JS output (built by `npm run build`)
│   └── public/     # static frontend (no build step)
│       ├── index.html
│       ├── styles.css
│       └── app.js
└── ...
```

## Run

```bash
# 1. install dependencies (express, jsonwebtoken, dotenv, typescript)
cd web
npm install

# 2. configure — copy and edit at the project root
cd ..
cp .env.example .env
# edit .env: ADMIN_PASSWORD, GO2API_TOKEN, JWT_SECRET, ...

# 3. build TypeScript
cd web && npm run build

# 4. start the web UI on http://localhost:8081
cd web && npm start
```

For development with auto-reload:

```bash
cd web && npm run dev
# uses tsx watch — no build step needed during development
```

Then open <http://localhost:8081> and log in with the credentials from `.env`.

### Environment variables

All variables are read from the environment (or the project-root `.env` if present).

| Variable          | Default                 | Description                                              |
| ----------------- | ----------------------- | -------------------------------------------------------- |
| `PORT`            | `8081`                  | Port the web UI listens on                               |
| `GO2API_URL`      | `http://localhost:8080` | Upstream go2api base URL                                 |
| `GO2API_TOKEN`    | (unset)                 | Client token forwarded to go2api as `Authorization: Bearer …` |
| `ADMIN_USERNAME`  | `admin`                 | Username accepted by `/api/auth/login`                   |
| `ADMIN_PASSWORD`  | `admin`                 | Password accepted by `/api/auth/login`                   |
| `JWT_SECRET`      | (random per start)      | HMAC secret for signing JWTs. **Set this in production.** |
| `JWT_EXPIRES_IN`  | `12h`                   | Token lifetime (any [`ms`](https://github.com/vercel/ms) string) |

> If `GO2API_TOKEN` is empty, upstream calls are unauthenticated and go2api will return 401.
> If `ADMIN_PASSWORD` is empty, the default `admin` is used (a warning is printed).
> If `JWT_SECRET` is empty, a random secret is generated per process — all tokens are invalidated when the server restarts.

### Generating a JWT secret

```bash
openssl rand -hex 32
```

Paste the output into `JWT_SECRET=` in `.env`.

### Inline overrides

Anything in `.env` can still be overridden on the command line:

```bash
PORT=9000 ADMIN_PASSWORD=hunter2 npm start
```

Command-line values take precedence over `.env`.

### Why .env at the project root?

Both the go2api backend and the web frontend can share a single config file. The Node side reads it via `dotenv`; if you later want the Go binary to read it too, you only need to add one library (e.g. `github.com/joho/godotenv`) and the same `.env` continues to work.

## Pages

- **Dashboard** — KPI cards (active keys, cooldown count, 24h requests, hit rate) plus cache snapshot.
- **Keys** — table of all configured keys with status pill, quota, reset time, last error. Add / enable / disable / delete actions.
- **Models** — grouped list of OpenAI-compatible and Anthropic-compatible model IDs.
- **Cache** — cache stats with a one-click flush action.

## Auth model

1. User submits username/password on the login screen.
2. Server validates against `ADMIN_USERNAME` / `ADMIN_PASSWORD` using constant-time compare.
3. On success, server signs a JWT (`{ sub, role, iat, exp }`) with `JWT_SECRET` and returns it to the browser.
4. Browser stores the JWT in `localStorage` and attaches it as `Authorization: Bearer …` on every subsequent request.
5. Server validates the JWT before letting `/api/*` calls reach the upstream go2api. The upstream `Authorization` header is rewritten to use the server-side `GO2API_TOKEN`.

## How the proxy works

The Express server receives `/api/<anything>` and forwards it to `${GO2API_URL}/<anything>`, preserving method, query string, and content. The browser's JWT is stripped and replaced with the server-configured upstream token. Response bodies stream straight through.