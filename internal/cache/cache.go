// Package cache implements request-level response caching backed by SQLite.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/user/go2api/internal/store"
)

// ErrMiss is returned by Get when no entry is present.
var ErrMiss = errors.New("cache: miss")

// Cache stores HTTP responses keyed by a normalized request fingerprint.
type Cache struct {
	db            *store.DB
	ttl           time.Duration
	skipStreaming bool
	maxBytes      int64
	logger        *slog.Logger
}

// New returns a Cache using the provided store and TTL.
func New(db *store.DB, ttl time.Duration, skipStreaming bool, maxBytes int64, logger *slog.Logger) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	return &Cache{db: db, ttl: ttl, skipStreaming: skipStreaming, maxBytes: maxBytes, logger: logger}
}

// Item is the value side of a cache entry.
type Item struct {
	Status      int
	ContentType string
	Body        []byte
}

// Fingerprint is the input to KeyFor(). It captures every dimension that should
// change the cache result.
type Fingerprint struct {
	Endpoint string          // e.g. "/v1/chat/completions"
	Model    string          // client-facing model id (without "opencode-go/" prefix)
	Stream   bool            // streaming requests are not cached
	RawBody  json.RawMessage // raw request JSON for deep normalization
}

// KeyFor computes the SHA256 cache key for the given fingerprint.
// Streaming requests are intentionally not hashed here: callers should not even call this.
func KeyFor(fp Fingerprint) (string, error) {
	if fp.Stream {
		return "", fmt.Errorf("cache: streaming requests cannot be cached")
	}
	normalized, err := normalize(fp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:]), nil
}

// Get returns the cached item for the given hash, or ErrMiss if absent / expired.
// On hit it also increments the counter.
func (c *Cache) Get(ctx context.Context, hash string) (Item, error) {
	if !c.skipStreaming && false { // hook left for future toggles
		// always skip streaming by default; guard against future regressions
	}
	row, err := c.db.CacheGet(ctx, hash)
	if err != nil {
		return Item{}, ErrMiss
	}
	if int64(len(row.Response)) > c.maxBytes {
		c.logger.Warn("cache entry exceeds max_bytes, dropping", "hash", hash, "size", len(row.Response))
		return Item{}, ErrMiss
	}
	if err := c.db.CacheTouch(ctx, hash); err != nil {
		c.logger.Warn("cache touch failed", "err", err)
	}
	return Item{Status: row.Status, ContentType: row.ContentType, Body: row.Response}, nil
}

// Put stores the response. It silently refuses payloads larger than maxBytes.
func (c *Cache) Put(ctx context.Context, hash, model string, request []byte, item Item) error {
	if int64(len(item.Body)) > c.maxBytes {
		return fmt.Errorf("cache: response %d bytes exceeds max_bytes %d", len(item.Body), c.maxBytes)
	}
	now := time.Now()
	return c.db.CachePut(ctx, store.CacheRow{
		Hash:        hash,
		Model:       model,
		Request:     request,
		Response:    item.Body,
		ContentType: item.ContentType,
		Status:      item.Status,
		Hits:        0,
		CreatedAt:   now,
		ExpiresAt:   now.Add(c.ttl),
	})
}

// Flush removes every cached row.
func (c *Cache) Flush(ctx context.Context) error {
	return c.db.CacheFlush(ctx)
}

// StartGC launches a background goroutine that periodically drops expired rows.
// It returns a stop function; callers should defer it.
func (c *Cache) StartGC(ctx context.Context, every time.Duration) (stop func()) {
	stopCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stopCtx.Done():
				return
			case <-t.C:
				if n, err := c.db.CacheGC(stopCtx); err != nil {
					c.logger.Warn("cache gc failed", "err", err)
				} else if n > 0 {
					c.logger.Info("cache gc removed rows", "count", n)
				}
			}
		}
	}()
	return cancel
}

// normalize builds the byte slice that gets hashed. It walks the request JSON
// to:
//
//   - drop keys that must not affect cacheability (stream, user, metadata)
//   - canonicalize slice ordering so two clients with semantically identical
//     requests hit the same bucket
//
// Unknown shapes fall back to hashing the raw bytes after stripping the stream flag.
func normalize(fp Fingerprint) ([]byte, error) {
	// Fast-path: empty body -> hash the endpoint+model only.
	if len(fp.RawBody) == 0 {
		return join(fp.Endpoint, fp.Model, "{}"), nil
	}
	var v any
	if err := json.Unmarshal(fp.RawBody, &v); err != nil {
		// Not JSON: fall back to raw.
		return join(fp.Endpoint, fp.Model, string(fp.RawBody)), nil
	}
	walk(v)
	stripKeys(v, "stream", "user", "metadata", "stream_options", "id")
	canon(v)
	out, err := json.Marshal(v)
	if err != nil {
		return join(fp.Endpoint, fp.Model, string(fp.RawBody)), nil
	}
	return join(fp.Endpoint, fp.Model, string(out)), nil
}

func join(parts ...string) []byte {
	total := 0
	for _, p := range parts {
		total += len(p) + 1
	}
	buf := make([]byte, 0, total)
	for _, p := range parts {
		buf = append(buf, p...)
		buf = append(buf, '\n')
	}
	return buf
}

// walk descends through the JSON value normalizing nested maps.
func walk(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			walk(child)
			t[k] = child
		}
	case []any:
		for i, child := range t {
			walk(child)
			t[i] = child
		}
	}
}

// stripKeys removes keys whose names match any of the supplied keys (recursively).
func stripKeys(v any, names ...string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if isIgnoredKey(k, names) {
				delete(t, k)
				continue
			}
			stripKeys(child, names...)
		}
	case []any:
		for _, child := range t {
			stripKeys(child, names...)
		}
	}
}

func isIgnoredKey(k string, names []string) bool {
	for _, n := range names {
		if k == n {
			return true
		}
	}
	return false
}

// canon canonicalizes maps by sorting their keys before serialization, so that
// {"a":1,"b":2} and {"b":2,"a":1} produce identical hashes.
func canon(v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		canonMap := make(map[string]any, len(t))
		for _, k := range keys {
			canonMap[k] = t[k]
		}
		// Mutate in place: replace the original map contents in deterministic order.
		for k := range t {
			delete(t, k)
		}
		for k, val := range canonMap {
			canon(val)
			t[k] = val
		}
	case []any:
		for _, child := range t {
			canon(child)
		}
	}
}
