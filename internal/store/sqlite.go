// Package store wraps the SQLite database used for keys, cache, quotas, and logs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB is a thin wrapper around *sql.DB providing the schema and helpers used by go2api.
type DB struct {
	*sql.DB
}

// Open creates / opens the SQLite file at the given path, applies WAL mode, and runs migrations.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("store: path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite is single-writer; serialize
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &DB{db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    id            TEXT PRIMARY KEY,
    label         TEXT NOT NULL,
    api_key       TEXT NOT NULL,
    weight        INTEGER NOT NULL DEFAULT 1,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    cooldown_until INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT
);

CREATE TABLE IF NOT EXISTS cache (
    hash        TEXT PRIMARY KEY,
    model       TEXT NOT NULL,
    request     BLOB NOT NULL,
    response    BLOB NOT NULL,
    content_type TEXT NOT NULL,
    status      INTEGER NOT NULL,
    hits        INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_expires ON cache(expires_at);

CREATE TABLE IF NOT EXISTS quota_state (
    key_id      TEXT PRIMARY KEY,
    remaining   REAL NOT NULL DEFAULT 0,
    limit_amt   REAL NOT NULL DEFAULT 0,
    reset_at    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS request_logs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    ts               INTEGER NOT NULL,
    key_id           TEXT,
    model            TEXT,
    status           INTEGER,
    cache_hit        INTEGER NOT NULL DEFAULT 0,
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    prompt_tokens    INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens    INTEGER NOT NULL DEFAULT 0,
    cost             REAL NOT NULL DEFAULT 0,
    error            TEXT
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_key_ts ON request_logs(key_id, ts);

CREATE TABLE IF NOT EXISTS tokens (
    id           TEXT PRIMARY KEY,
    label        TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,
    quota_limit  INTEGER NOT NULL DEFAULT 0,    -- 0 = unlimited
    quota_used   INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL DEFAULT 0,    -- 0 = no expiry
    last_used_at INTEGER NOT NULL DEFAULT 0,
    revoked      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(token_hash);
`

func (s *DB) migrate() error {
	if _, err := s.Exec(schema); err != nil {
		return err
	}
	return s.migrateColumns()
}

// migrateColumns adds columns that were introduced after the initial schema.
// SQLite's ALTER TABLE ADD COLUMN is idempotent-safe when wrapped in a column
// existence check. This lets existing databases upgrade in place.
func (s *DB) migrateColumns() error {
	addIfMissing := func(table, col, decl string) error {
		var n int
		err := s.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			_, err = s.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
		}
		return err
	}
	cols := [][3]string{
		{"request_logs", "prompt_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "completion_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "cost", "REAL NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		if err := addIfMissing(c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	_, err := s.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_key_ts ON request_logs(key_id, ts)`)
	return err
}

// UpsertKey inserts or updates an API key record.
func (s *DB) UpsertKey(ctx context.Context, k KeyRow) error {
	_, err := s.ExecContext(ctx, `
        INSERT INTO api_keys (id, label, api_key, weight, disabled, created_at, cooldown_until, last_error)
        VALUES (?,?,?,?,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET
            label=excluded.label,
            api_key=excluded.api_key,
            weight=excluded.weight,
            disabled=excluded.disabled`,
		k.ID, k.Label, k.APIKey, k.Weight, boolToInt(k.Disabled), k.CreatedAt.Unix(), 0, "",
	)
	return err
}

// ListKeys returns all configured keys.
func (s *DB) ListKeys(ctx context.Context) ([]KeyRow, error) {
	rows, err := s.QueryContext(ctx, `
        SELECT id, label, api_key, weight, disabled, created_at, cooldown_until, COALESCE(last_error,'')
        FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRow
	for rows.Next() {
		var k KeyRow
		var disabled int
		var createdAt, cooldown int64
		if err := rows.Scan(&k.ID, &k.Label, &k.APIKey, &k.Weight, &disabled, &createdAt, &cooldown, &k.LastError); err != nil {
			return nil, err
		}
		k.Disabled = disabled != 0
		k.CreatedAt = time.Unix(createdAt, 0)
		k.CoolDownUntil = time.Unix(cooldown, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetKeyCooldown marks a key as unavailable until the given time.
func (s *DB) SetKeyCooldown(ctx context.Context, id string, until time.Time, lastErr string) error {
	_, err := s.ExecContext(ctx, `
        UPDATE api_keys SET cooldown_until = ?, last_error = ? WHERE id = ?`,
		until.Unix(), lastErr, id)
	return err
}

// SetKeyDisabled toggles the disabled flag.
func (s *DB) SetKeyDisabled(ctx context.Context, id string, disabled bool) error {
	_, err := s.ExecContext(ctx, `UPDATE api_keys SET disabled = ? WHERE id = ?`, boolToInt(disabled), id)
	return err
}

// DeleteKey removes a key from the pool.
func (s *DB) DeleteKey(ctx context.Context, id string) error {
	_, err := s.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// UpsertQuota writes the latest observed rate-limit snapshot for a key.
func (s *DB) UpsertQuota(ctx context.Context, q QuotaRow) error {
	_, err := s.ExecContext(ctx, `
        INSERT INTO quota_state (key_id, remaining, limit_amt, reset_at, updated_at)
        VALUES (?,?,?,?,?)
        ON CONFLICT(key_id) DO UPDATE SET
            remaining=excluded.remaining,
            limit_amt=excluded.limit_amt,
            reset_at=excluded.reset_at,
            updated_at=excluded.updated_at`,
		q.KeyID, q.Remaining, q.Limit, q.ResetAt.Unix(), time.Now().Unix())
	return err
}

// GetQuota reads the most recent quota snapshot for a key.
func (s *DB) GetQuota(ctx context.Context, keyID string) (QuotaRow, error) {
	var q QuotaRow
	var reset, updated int64
	err := s.QueryRowContext(ctx, `
        SELECT key_id, remaining, limit_amt, reset_at, updated_at
        FROM quota_state WHERE key_id = ?`, keyID).
		Scan(&q.KeyID, &q.Remaining, &q.Limit, &reset, &updated)
	if err != nil {
		return q, err
	}
	q.ResetAt = time.Unix(reset, 0)
	q.UpdatedAt = time.Unix(updated, 0)
	return q, nil
}

// CacheGet fetches a cache row. Returns sql.ErrNoRows if missing/expired.
func (s *DB) CacheGet(ctx context.Context, hash string) (CacheRow, error) {
	var c CacheRow
	var createdAt, expiresAt int64
	err := s.QueryRowContext(ctx, `
        SELECT hash, model, request, response, content_type, status, hits, created_at, expires_at
        FROM cache WHERE hash = ? AND expires_at > ?`, hash, time.Now().Unix()).
		Scan(&c.Hash, &c.Model, &c.Request, &c.Response, &c.ContentType, &c.Status, &c.Hits, &createdAt, &expiresAt)
	if err != nil {
		return c, err
	}
	c.CreatedAt = time.Unix(createdAt, 0)
	c.ExpiresAt = time.Unix(expiresAt, 0)
	return c, nil
}

// CachePut writes a cache row, replacing any existing entry with the same hash.
func (s *DB) CachePut(ctx context.Context, c CacheRow) error {
	_, err := s.ExecContext(ctx, `
        INSERT INTO cache (hash, model, request, response, content_type, status, hits, created_at, expires_at)
        VALUES (?,?,?,?,?,?,?,?,?)
        ON CONFLICT(hash) DO UPDATE SET
            response=excluded.response,
            content_type=excluded.content_type,
            status=excluded.status,
            expires_at=excluded.expires_at`,
		c.Hash, c.Model, c.Request, c.Response, c.ContentType, c.Status, c.Hits,
		c.CreatedAt.Unix(), c.ExpiresAt.Unix())
	return err
}

// CacheTouch increments the hit counter.
func (s *DB) CacheTouch(ctx context.Context, hash string) error {
	_, err := s.ExecContext(ctx, `UPDATE cache SET hits = hits + 1 WHERE hash = ?`, hash)
	return err
}

// CacheFlush removes every cached row.
func (s *DB) CacheFlush(ctx context.Context) error {
	_, err := s.ExecContext(ctx, `DELETE FROM cache`)
	return err
}

// CacheGC drops expired rows. Returns the count deleted.
func (s *DB) CacheGC(ctx context.Context) (int64, error) {
	res, err := s.ExecContext(ctx, `DELETE FROM cache WHERE expires_at <= ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- tokens (client-facing API keys) --------------------------------------

// CreateToken inserts a new token row. Caller is responsible for hashing the
// raw token before passing token_hash; only the hash is stored.
func (s *DB) CreateToken(ctx context.Context, t TokenRow) error {
	_, err := s.ExecContext(ctx, `
        INSERT INTO tokens (id, label, token_hash, prefix, quota_limit, quota_used, created_at, expires_at, last_used_at, revoked)
        VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Label, t.TokenHash, t.Prefix, t.QuotaLimit, t.QuotaUsed,
		t.CreatedAt.Unix(), t.ExpiresAt.Unix(), 0, boolToInt(t.Revoked))
	return err
}

// ListTokens returns every token (revoked or not) for the admin view.
func (s *DB) ListTokens(ctx context.Context) ([]TokenRow, error) {
	rows, err := s.QueryContext(ctx, `
        SELECT id, label, token_hash, prefix, quota_limit, quota_used,
               created_at, expires_at, last_used_at, revoked
        FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var t TokenRow
		var quotaLimit, quotaUsed, createdAt, expiresAt, lastUsed int64
		var revoked int
		if err := rows.Scan(&t.ID, &t.Label, &t.TokenHash, &t.Prefix,
			&quotaLimit, &quotaUsed, &createdAt, &expiresAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		t.QuotaLimit = int(quotaLimit)
		t.QuotaUsed = int(quotaUsed)
		t.CreatedAt = time.Unix(createdAt, 0)
		t.ExpiresAt = time.Unix(expiresAt, 0)
		t.LastUsedAt = time.Unix(lastUsed, 0)
		t.Revoked = revoked != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindTokenByHash returns the row whose token_hash matches (or sql.ErrNoRows).
func (s *DB) FindTokenByHash(ctx context.Context, hash string) (TokenRow, error) {
	var t TokenRow
	var quotaLimit, quotaUsed, createdAt, expiresAt, lastUsed int64
	var revoked int
	err := s.QueryRowContext(ctx, `
        SELECT id, label, token_hash, prefix, quota_limit, quota_used,
               created_at, expires_at, last_used_at, revoked
        FROM tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.Label, &t.TokenHash, &t.Prefix,
			&quotaLimit, &quotaUsed, &createdAt, &expiresAt, &lastUsed, &revoked)
	if err != nil {
		return t, err
	}
	t.QuotaLimit = int(quotaLimit)
	t.QuotaUsed = int(quotaUsed)
	t.CreatedAt = time.Unix(createdAt, 0)
	t.ExpiresAt = time.Unix(expiresAt, 0)
	t.LastUsedAt = time.Unix(lastUsed, 0)
	t.Revoked = revoked != 0
	return t, nil
}

// UpdateTokenHash rotates the token value: writes a new hash and prefix but
// keeps the id, label, quota counters and timestamps. Returns the affected
// row count.
func (s *DB) UpdateTokenHash(ctx context.Context, id, newHash, newPrefix string) error {
	_, err := s.ExecContext(ctx, `
        UPDATE tokens SET token_hash = ?, prefix = ? WHERE id = ?`,
		newHash, newPrefix, id)
	return err
}

// RevokeToken marks a token as revoked (soft delete).
func (s *DB) RevokeToken(ctx context.Context, id string) error {
	_, err := s.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	return err
}

// IncrementTokenUsage bumps quota_used and last_used_at for the given token.
// If quota_limit is set and quota_used has reached it, returns ErrQuotaExceeded.
func (s *DB) IncrementTokenUsage(ctx context.Context, id string, limit int) error {
	if limit > 0 {
		// Atomic check-and-increment: only update if quota_used < limit.
		res, err := s.ExecContext(ctx, `
            UPDATE tokens
            SET quota_used = quota_used + 1,
                last_used_at = ?
            WHERE id = ? AND revoked = 0 AND quota_used < ?`,
			time.Now().Unix(), id, limit)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrTokenQuotaExceeded
		}
		return nil
	}
	// Unlimited quota: just bump the counter.
	_, err := s.ExecContext(ctx, `
        UPDATE tokens
        SET quota_used = quota_used + 1,
            last_used_at = ?
        WHERE id = ? AND revoked = 0`,
		time.Now().Unix(), id)
	return err
}

// ResetTokenUsage zeroes quota_used for a token. Returns the affected row count.
func (s *DB) ResetTokenUsage(ctx context.Context, id string) error {
	_, err := s.ExecContext(ctx, `UPDATE tokens SET quota_used = 0 WHERE id = ?`, id)
	return err
}

// LogRequest appends a request_log row. Errors are non-fatal in callers.
func (s *DB) LogRequest(ctx context.Context, l LogRow) error {
	_, err := s.ExecContext(ctx, `
        INSERT INTO request_logs (ts, key_id, model, status, cache_hit, latency_ms,
                                   prompt_tokens, completion_tokens, cached_tokens, cost, error)
        VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		l.Timestamp.Unix(), nullString(l.KeyID), nullString(l.Model), l.Status,
		boolToInt(l.CacheHit), l.LatencyMs,
		l.PromptTokens, l.CompletionTokens, l.CachedTokens, l.Cost,
		nullString(l.Error))
	return err
}

// StatsSummary aggregates basic counters.
func (s *DB) StatsSummary(ctx context.Context) (StatsRow, error) {
	var st StatsRow
	row := s.QueryRowContext(ctx, `
        SELECT
            COUNT(*),
            COALESCE(SUM(cache_hit), 0),
            COALESCE(AVG(latency_ms), 0)
        FROM request_logs WHERE ts > ?`, time.Now().Add(-24*time.Hour).Unix())
	if err := row.Scan(&st.Last24hTotal, &st.Last24hCacheHits, &st.Last24hAvgLatencyMs); err != nil {
		return st, err
	}
	row = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache`)
	_ = row.Scan(&st.CacheEntries)
	row = s.QueryRowContext(ctx, `SELECT COALESCE(SUM(hits), 0) FROM cache`)
	_ = row.Scan(&st.CacheTotalHits)
	return st, nil
}

// KeyUsage is the per-key cost summary for the three rolling budget windows.
type KeyUsage struct {
	KeyID         string
	FiveHourCost  float64
	WeeklyCost    float64
	MonthlyCost   float64
	TotalRequests int64
	LastUsedAt    time.Time
	ModelsUsed    string // comma-separated distinct model IDs
}

// KeyUsageSummary returns the aggregated cost for each key across the three
// rolling budget windows (5h, 7d, 30d). Only non-cache-hit, successful
// requests are counted — cached responses don't consume upstream budget.
func (s *DB) KeyUsageSummary(ctx context.Context) ([]KeyUsage, error) {
	now := time.Now()
	rows, err := s.QueryContext(ctx, `
        SELECT
            key_id,
            COALESCE(SUM(CASE WHEN ts > ? THEN cost ELSE 0 END), 0) AS five_hour,
            COALESCE(SUM(CASE WHEN ts > ? THEN cost ELSE 0 END), 0) AS weekly,
            COALESCE(SUM(CASE WHEN ts > ? THEN cost ELSE 0 END), 0) AS monthly,
            COUNT(*) AS total,
            COALESCE(MAX(ts), 0) AS last_used,
            COALESCE(GROUP_CONCAT(DISTINCT model), '') AS models_used
        FROM request_logs
        WHERE key_id IS NOT NULL AND cache_hit = 0 AND status >= 200 AND status < 300
        GROUP BY key_id`,
		now.Add(-5*time.Hour).Unix(),
		now.Add(-7*24*time.Hour).Unix(),
		now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyUsage
	for rows.Next() {
		var u KeyUsage
		var lastUsed int64
		if err := rows.Scan(&u.KeyID, &u.FiveHourCost, &u.WeeklyCost, &u.MonthlyCost, &u.TotalRequests, &lastUsed, &u.ModelsUsed); err != nil {
			return nil, err
		}
		u.LastUsedAt = time.Unix(lastUsed, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- row types ---

// ErrTokenQuotaExceeded is returned by IncrementTokenUsage when the token
// has hit its configured quota limit.
var ErrTokenQuotaExceeded = errors.New("store: token quota exceeded")

type TokenRow struct {
	ID         string
	Label      string
	TokenHash  string
	Prefix     string
	QuotaLimit int
	QuotaUsed  int
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt time.Time
	Revoked    bool
}

type KeyRow struct {
	ID            string
	Label         string
	APIKey        string
	Weight        int
	Disabled      bool
	CreatedAt     time.Time
	CoolDownUntil time.Time
	LastError     string
}

type QuotaRow struct {
	KeyID     string
	Remaining float64
	Limit     float64
	ResetAt   time.Time
	UpdatedAt time.Time
}

type CacheRow struct {
	Hash        string
	Model       string
	Request     []byte
	Response    []byte
	ContentType string
	Status      int
	Hits        int
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type LogRow struct {
	Timestamp        time.Time
	KeyID            string
	Model            string
	Status           int
	CacheHit         bool
	LatencyMs        int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Cost             float64
	Error            string
}

type StatsRow struct {
	Last24hTotal        int64
	Last24hCacheHits    int64
	Last24hAvgLatencyMs float64
	CacheEntries        int
	CacheTotalHits      int64
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
