// Package keypool manages a pool of upstream API keys with multiple selection strategies.
package keypool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/user/go2api/internal/config"
	"github.com/user/go2api/internal/store"
)

// ErrNoKeyAvailable is returned when every key is unavailable.
var ErrNoKeyAvailable = errors.New("keypool: no key available")

// Key is the runtime representation of a configured upstream API key.
type Key struct {
	ID            string
	Label         string
	APIKey        string
	Weight        int
	Disabled      bool
	CooldownUntil time.Time
	LastError     string
}

// Available reports whether the key can be used right now.
func (k *Key) Available() bool {
	if k == nil || k.Disabled {
		return false
	}
	if !k.CooldownUntil.IsZero() && time.Now().Before(k.CooldownUntil) {
		return false
	}
	return true
}

// Pool is the thread-safe key manager.
type Pool struct {
	mu       sync.RWMutex
	keys     []*Key
	strategy string
	failover config.FailoverConfig
	db       *store.DB
	logger   *slog.Logger

	rrCounter atomic.Uint64 // for round-robin and weighted

	retryableStatuses map[int]bool
	cooldown          time.Duration
}

// New constructs a Pool from the configured key list and persists them to the store.
func New(ctx context.Context, cfg config.KeyPoolConfig, db *store.DB, logger *slog.Logger) (*Pool, error) {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Pool{
		strategy:          cfg.Strategy,
		failover:          cfg.Failover,
		db:                db,
		logger:            logger,
		retryableStatuses: sliceToSet(cfg.Failover.RetryOn),
		cooldown:          time.Duration(cfg.Failover.CoolDown) * time.Second,
	}
	// Persist initial keys, then load authoritative state from DB.
	for _, kc := range cfg.Keys {
		id := kc.ID
		if id == "" {
			id = fmt.Sprintf("key-%s", sanitizeLabel(kc.Label))
		}
		if err := db.UpsertKey(ctx, store.KeyRow{
			ID:        id,
			Label:     kc.Label,
			APIKey:    kc.APIKey,
			Weight:    kc.Weight,
			Disabled:  kc.Disabled,
			CreatedAt: time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("persist key %s: %w", kc.Label, err)
		}
	}
	if err := p.reload(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// reload refreshes the in-memory key list from the database.
func (p *Pool) reload(ctx context.Context) error {
	rows, err := p.db.ListKeys(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = p.keys[:0]
	for _, r := range rows {
		k := &Key{
			ID:            r.ID,
			Label:         r.Label,
			APIKey:        r.APIKey,
			Weight:        r.Weight,
			Disabled:      r.Disabled,
			CooldownUntil: r.CoolDownUntil,
			LastError:     r.LastError,
		}
		p.keys = append(p.keys, k)
	}
	return nil
}

// Pick selects a key for an outgoing request. Strategy determines the heuristic:
//   - round_robin: atomic counter modulo available
//   - weighted:    expansion-weighted round robin
//   - quota_aware: prefer keys with most remaining quota; fall back to round robin
func (p *Pool) Pick(ctx context.Context) (*Key, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	available := make([]*Key, 0, len(p.keys))
	for _, k := range p.keys {
		if k.Available() {
			available = append(available, k)
		}
	}
	if len(available) == 0 {
		return nil, ErrNoKeyAvailable
	}
	switch p.strategy {
	case "weighted":
		return p.pickWeighted(available), nil
	case "quota_aware":
		return p.pickQuotaAware(ctx, available)
	default:
		return p.pickRoundRobin(available), nil
	}
}

func (p *Pool) pickRoundRobin(avail []*Key) *Key {
	idx := p.rrCounter.Add(1) - 1
	return avail[int(idx%uint64(len(avail)))]
}

func (p *Pool) pickWeighted(avail []*Key) *Key {
	// Build a flat slice where each key appears Weight times.
	total := 0
	for _, k := range avail {
		total += k.Weight
	}
	if total <= 0 {
		return p.pickRoundRobin(avail)
	}
	idx := int(p.rrCounter.Add(1)-1) % total
	for _, k := range avail {
		if idx < k.Weight {
			return k
		}
		idx -= k.Weight
	}
	return avail[len(avail)-1]
}

func (p *Pool) pickQuotaAware(ctx context.Context, avail []*Key) (*Key, error) {
	type scored struct {
		key  *Key
		rem  float64
		seen time.Time
	}
	scores := make([]scored, 0, len(avail))
	for _, k := range avail {
		q, err := p.db.GetQuota(ctx, k.ID)
		rem := 0.0
		var seen time.Time
		if err == nil {
			rem = q.Remaining
			seen = q.UpdatedAt
		}
		scores = append(scores, scored{key: k, rem: rem, seen: seen})
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].rem != scores[j].rem {
			return scores[i].rem > scores[j].rem
		}
		// Tie-break: prefer key with freshest quota info, then round robin.
		if !scores[i].seen.Equal(scores[j].seen) {
			return scores[i].seen.After(scores[j].seen)
		}
		return scores[i].key.Label < scores[j].key.Label
	})
	// Sometimes randomize among the top tier to spread load.
	top := scores[0]
	if len(scores) > 1 && scores[1].rem >= top.rem*0.9 {
		// 30% chance to use the runner-up to spread load.
		if rand.Intn(10) < 3 {
			return scores[1].key, nil
		}
	}
	return top.key, nil
}

// MarkSuccess records a successful call for the given key. It clears any cooldown.
func (p *Pool) MarkSuccess(ctx context.Context, k *Key) {
	if k == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k.LastError = ""
	k.CooldownUntil = time.Time{}
	if err := p.db.SetKeyCooldown(ctx, k.ID, time.Time{}, ""); err != nil {
		p.logger.Warn("clear cooldown failed", "key", k.ID, "err", err)
	}
}

// MarkFailure records a failed call. If the status code is retryable, the key is
// placed in cooldown for the configured duration.
func (p *Pool) MarkFailure(ctx context.Context, k *Key, status int, errStr string) {
	if k == nil {
		return
	}
	retry := p.retryableStatuses[status] || status >= 500
	if !retry {
		// Non-retryable failure (e.g. 400). Don't cooldown, just record.
		p.mu.Lock()
		k.LastError = errStr
		p.mu.Unlock()
		_ = p.db.SetKeyCooldown(ctx, k.ID, time.Time{}, errStr)
		return
	}
	until := time.Now().Add(p.cooldown)
	p.mu.Lock()
	k.CooldownUntil = until
	k.LastError = errStr
	p.mu.Unlock()
	if err := p.db.SetKeyCooldown(ctx, k.ID, until, errStr); err != nil {
		p.logger.Warn("set cooldown failed", "key", k.ID, "err", err)
	}
}

// RecordQuota updates the cached quota snapshot from upstream rate-limit headers.
func (p *Pool) RecordQuota(ctx context.Context, k *Key, headers map[string]string) {
	if k == nil {
		return
	}
	remaining := parseFloat(headers["x-ratelimit-remaining-requests"], headers["x-quota-remaining"])
	limit := parseFloat(headers["x-ratelimit-limit-requests"], headers["x-quota-limit"])
	reset := parseReset(headers["x-ratelimit-reset-requests"], headers["x-ratelimit-reset"], headers["x-quota-reset"])
	if remaining == 0 && limit == 0 && reset.IsZero() {
		return
	}
	if err := p.db.UpsertQuota(ctx, store.QuotaRow{
		KeyID:     k.ID,
		Remaining: remaining,
		Limit:     limit,
		ResetAt:   reset,
	}); err != nil {
		p.logger.Warn("upsert quota failed", "key", k.ID, "err", err)
	}
}

// Snapshot returns a copy of the current key state for the admin endpoint.
func (p *Pool) Snapshot(ctx context.Context) ([]KeyView, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	views := make([]KeyView, 0, len(p.keys))
	for _, k := range p.keys {
		q, _ := p.db.GetQuota(ctx, k.ID)
		views = append(views, KeyView{
			ID:            k.ID,
			Label:         k.Label,
			APIKeyMasked:  maskAPIKey(k.APIKey),
			Weight:        k.Weight,
			Disabled:      k.Disabled,
			CooldownUntil: k.CooldownUntil,
			LastError:     k.LastError,
			Quota: QuotaView{
				Remaining: q.Remaining,
				Limit:     q.Limit,
				ResetAt:   q.ResetAt,
			},
		})
	}
	return views, nil
}

// SetDisabled toggles the disabled flag for a key.
func (p *Pool) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if err := p.db.SetKeyDisabled(ctx, id, disabled); err != nil {
		return err
	}
	return p.reload(ctx)
}

// AddKey inserts a new key at runtime.
func (p *Pool) AddKey(ctx context.Context, kc config.KeyConfig) error {
	id := kc.ID
	if id == "" {
		id = fmt.Sprintf("key-%s", sanitizeLabel(kc.Label))
	}
	weight := kc.Weight
	if weight <= 0 {
		weight = 1
	}
	if err := p.db.UpsertKey(ctx, store.KeyRow{
		ID:        id,
		Label:     kc.Label,
		APIKey:    kc.APIKey,
		Weight:    weight,
		Disabled:  kc.Disabled,
		CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	return p.reload(ctx)
}

// RemoveKey deletes a key.
func (p *Pool) RemoveKey(ctx context.Context, id string) error {
	if err := p.db.DeleteKey(ctx, id); err != nil {
		return err
	}
	return p.reload(ctx)
}

// IsRetryableStatus returns true if the upstream status is configured for failover.
func (p *Pool) IsRetryableStatus(status int) bool {
	return p.retryableStatuses[status] || status >= 500
}

// MaxRetries returns the configured retry budget.
func (p *Pool) MaxRetries() int {
	return p.failover.MaxRetries
}

// Enabled reports whether failover is on.
func (p *Pool) Enabled() bool {
	return p.failover.Enabled
}

// KeyView is the admin-friendly snapshot of a key.
type KeyView struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	APIKeyMasked  string    `json:"api_key_masked"`
	Weight        int       `json:"weight"`
	Disabled      bool      `json:"disabled"`
	CooldownUntil time.Time `json:"cooldown_until"`
	LastError     string    `json:"last_error,omitempty"`
	Quota         QuotaView `json:"quota"`
}

type QuotaView struct {
	Remaining float64   `json:"remaining"`
	Limit     float64   `json:"limit"`
	ResetAt   time.Time `json:"reset_at"`
}

func sliceToSet(s []int) map[int]bool {
	m := make(map[int]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func parseFloat(values ...string) float64 {
	for _, v := range values {
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return 0
}

func parseReset(values ...string) time.Time {
	for _, v := range values {
		if v == "" {
			continue
		}
		// Try unix timestamp seconds first.
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Unix(int64(f), 0)
		}
		// Try duration (e.g. "5m", "30s").
		if d, err := time.ParseDuration(v); err == nil {
			return time.Now().Add(d)
		}
	}
	return time.Time{}
}

func sanitizeLabel(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-' || r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("k%d", time.Now().UnixNano()%1_000_000)
	}
	return string(out)
}

func maskAPIKey(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}
