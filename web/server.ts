// server.ts — go2api admin web UI (TypeScript).
//
// Responsibilities:
//   1. Serve the static frontend (public/).
//   2. Issue JWTs after the admin user presents the configured username/password.
//      Subsequent browser requests carry the JWT in `Authorization: Bearer ...`,
//      which the server validates before letting /api/* calls reach the upstream
//      go2api (where the server-side GO2API_TOKEN is injected).
//
// Environment variables:
//
//   PORT              — UI server port (default 8081)
//   GO2API_URL        — upstream go2api base URL (default http://localhost:8080)
//   GO2API_TOKEN      — client token forwarded upstream
//   ADMIN_USERNAME    — admin username (default "admin")
//   ADMIN_PASSWORD    — admin password (default "admin", with a startup warning)
//   JWT_SECRET        — HMAC secret for signing JWTs (auto-generated and warned if missing)
//   JWT_EXPIRES_IN    — token lifetime, e.g. "12h", "7d" (default "12h")

import path from 'node:path';
import crypto from 'node:crypto';
import fs from 'node:fs';
import express, { type Request, type Response, type NextFunction } from 'express';
import jwt, { type JwtPayload, type SignOptions } from 'jsonwebtoken';
import dotenv from 'dotenv';

// Load .env from the project root (one level up from web/) so all
// configuration lives in a single place regardless of which binary you start.
// When compiled to dist/server.js, __dirname is web/dist/ so we need an extra
// level of ".." compared to running via tsx where __dirname is web/.
const isCompiled = __dirname.endsWith(path.sep + 'dist');
const envPath = path.resolve(__dirname, '..', ...(isCompiled ? ['..'] : []), '.env');
dotenv.config({ path: envPath });

const PORT = parseInt(process.env.PORT || '8081', 10);
const UPSTREAM = (process.env.GO2API_URL || 'http://localhost:8080').replace(/\/$/, '');
const TOKEN = process.env.GO2API_TOKEN || '';
const ADMIN_USERNAME = process.env.ADMIN_USERNAME || 'admin';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || 'admin';
const JWT_EXPIRES_IN = process.env.JWT_EXPIRES_IN || '12h';

if (!process.env.JWT_SECRET) {
  console.warn('[warn] JWT_SECRET is not set — generating an ephemeral secret. Tokens will be invalidated on restart.');
}
const JWT_SECRET = process.env.JWT_SECRET || crypto.randomBytes(32).toString('hex');

if (!process.env.ADMIN_PASSWORD) {
  console.warn('[warn] ADMIN_PASSWORD is not set — using default "admin". Set it in production!');
}
if (!TOKEN) {
  console.warn('[warn] GO2API_TOKEN is not set — upstream calls will be unauthenticated.');
}

const app = express();

// --- Types -------------------------------------------------------------------

interface JwtClaims extends JwtPayload {
  sub: string;
  role: string;
}

interface LogExtra {
  [key: string]: unknown;
}

// --- Logging -----------------------------------------------------------------

function log(level: string, msg: string, extra?: LogExtra): void {
  const ts = new Date().toISOString();
  const tail = extra ? ' ' + JSON.stringify(extra) : '';
  process.stdout.write(`[${ts}] ${level} ${msg}${tail}\n`);
}

// --- Static UI ----------------------------------------------------------------

// public/ lives in web/, one level up from dist/ when compiled, or in the
// same dir when run via tsx.
const PUBLIC_DIR = path.resolve(__dirname, isCompiled ? '..' : '.', 'public');
// Serve static files but force revalidation on every request so users always
// pick up the latest HTML/JS/CSS without needing a hard refresh.
app.use(express.static(PUBLIC_DIR, {
  index: 'index.html',
  extensions: ['html'],
  setHeaders(res: Response, filePath: string) {
    if (filePath.endsWith('.html')) {
      res.setHeader('Cache-Control', 'no-cache, must-revalidate');
    } else {
      res.setHeader('Cache-Control', 'no-cache');
    }
  },
}));

// --- Body parsers ------------------------------------------------------------

app.use('/api', express.json({ limit: '64kb' }));

// --- Auth helpers ------------------------------------------------------------

function signToken(username: string): string {
  const opts: SignOptions = { expiresIn: JWT_EXPIRES_IN as unknown as number };
  return jwt.sign({ sub: username, role: 'admin' }, JWT_SECRET, opts);
}

function verifyToken(token: string): JwtClaims | null {
  try {
    const decoded = jwt.verify(token, JWT_SECRET);
    return decoded as JwtClaims;
  } catch {
    return null;
  }
}

function extractToken(req: Request): string | null {
  const header = req.headers['authorization'] || '';
  if (header.startsWith('Bearer ')) return header.slice(7);
  if (header.startsWith('bearer ')) return header.slice(7);
  return null;
}

// Public auth endpoints — never rejected by the auth middleware.
app.post('/api/auth/login', (req: Request, res: Response) => {
  const { username, password } = req.body || {};
  if (typeof username !== 'string' || typeof password !== 'string') {
    return res.status(400).json({ error: { message: '请输入用户名和密码', type: 'invalid_request' } });
  }
  // Constant-time compare: timingSafeEqual requires equal-length buffers, so
  // we always compare against the configured values padded to the same size
  // and additionally compare lengths separately.
  function safeEqual(provided: string, expected: string): boolean {
    const a = Buffer.from(provided);
    const b = Buffer.from(expected);
    if (a.length !== b.length) return false;
    return crypto.timingSafeEqual(a, b);
  }
  const ok = safeEqual(username, ADMIN_USERNAME) && safeEqual(password, ADMIN_PASSWORD);
  if (!ok) {
    return res.status(401).json({ error: { message: '用户名或密码错误', type: 'auth_error' } });
  }
  const token = signToken(ADMIN_USERNAME);
  log('info', 'login success', { user: ADMIN_USERNAME });
  res.json({ token, expires_in: JWT_EXPIRES_IN });
});

app.get('/api/auth/me', (req: Request, res: Response) => {
  const token = extractToken(req);
  const claims = token ? verifyToken(token) : null;
  if (!claims) return res.status(401).json({ error: { message: '未登录', type: 'auth_error' } });
  res.json({ username: claims.sub, role: claims.role });
});

// Logout is a no-op for stateless JWTs, but we expose it so the frontend can call it.
app.post('/api/auth/logout', (_req: Request, res: Response) => res.json({ ok: true }));

// --- Auth middleware ----------------------------------------------------------

// Routes that are reachable without a JWT.
const PUBLIC_API = new Set(['/api/auth/login']);

function requireAuth(req: Request, res: Response, next: NextFunction): void {
  if (req.method === 'OPTIONS') return next();
  if (!req.path.startsWith('/api/')) return next();
  if (PUBLIC_API.has(req.path)) return next();
  const token = extractToken(req);
  if (!token) {
    res.status(401).json({ error: { message: '缺少认证令牌', type: 'auth_error' } });
    return;
  }
  const claims = verifyToken(token);
  if (!claims) {
    res.status(401).json({ error: { message: '令牌无效或已过期', type: 'auth_error' } });
    return;
  }
  (req as Request & { user: JwtClaims }).user = claims;
  next();
}

app.use(requireAuth);

// --- API proxy ----------------------------------------------------------------

// Translate /api/* -> ${UPSTREAM}${req.url.replace(/^\/api/, '')}
async function proxyToUpstream(req: Request, res: Response): Promise<void> {
  const target = UPSTREAM + req.url.replace(/^\/api/, '');
  const headers: Record<string, string> = { ...req.headers as Record<string, string> };
  delete headers.host;
  delete headers['content-length'];
  delete headers.authorization; // strip the user's JWT — we'll inject the upstream token instead
  delete headers.cookie;

  if (TOKEN) {
    headers['authorization'] = `Bearer ${TOKEN}`;
  }

  const init: RequestInit = { method: req.method, headers };
  if (req.method !== 'GET' && req.method !== 'HEAD' && req.body !== undefined) {
    init.body = typeof req.body === 'string' ? req.body : JSON.stringify(req.body);
    if (!headers['content-type']) {
      headers['content-type'] = 'application/json';
    }
  }

  let upstreamRes: globalThis.Response;
  try {
    upstreamRes = await fetch(target, init);
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    log('error', 'upstream fetch failed', { target, err: msg });
    res.status(502).json({ error: { message: '上游服务不可达', type: 'upstream_error' } });
    return;
  }

  upstreamRes.headers.forEach((value: string, key: string) => {
    if (key.toLowerCase() === 'content-encoding') return;
    res.setHeader(key, value);
  });
  res.status(upstreamRes.status);

  if (!upstreamRes.body) {
    res.end();
    return;
  }

  const reader = upstreamRes.body.getReader();
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    res.write(Buffer.from(value as Uint8Array));
  }
  res.end();
}

app.all('/api', proxyToUpstream);
app.all('/api/*', proxyToUpstream);

// 404 JSON for unknown /api paths (after auth middleware so it still 401s first).
app.use('/api', (_req: Request, res: Response) => {
  res.status(404).json({ error: { message: '接口不存在', type: 'not_found' } });
});

// --- SPA fallback -------------------------------------------------------------

// Anything else that isn't /api and isn't a static file falls back to index.html
// so the SPA can handle the route. This prevents the "Cannot GET /foo" 404.
app.get(/^\/(?!api).*/, (_req: Request, res: Response, next: NextFunction) => {
  res.sendFile(path.join(PUBLIC_DIR, 'index.html'), (err: Error | null) => {
    if (err) next(err);
  });
});

// --- Startup ------------------------------------------------------------------

const ENV_FILE = path.resolve(__dirname, '..', ...(isCompiled ? ['..'] : []), '.env');
const ENV_LOADED = fs.existsSync(ENV_FILE);

app.listen(PORT, () => {
  log('info', `go2api-web listening`, {
    port: PORT,
    upstream: UPSTREAM,
    admin: ADMIN_USERNAME,
    token_set: !!TOKEN,
    jwt_secret_set: !!process.env.JWT_SECRET,
    env_file: ENV_LOADED ? ENV_FILE : '(none — using process env)',
  });
  if (!ENV_LOADED) {
    log('info', `tip: copy .env.example (project root) to .env to keep configuration in one place`);
  }
});
