package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/user/go2api/internal/store"
)

// Tokens handles /admin/tokens CRUD. The admin creates and revokes tokens
// that clients use to call go2api's data plane.
type Tokens struct {
	Store *store.DB
}

// tokenPrefix is the public identifier prefix for go2api tokens.
const tokenPrefix = "g2a_"

// rawTokenLength is the number of random bytes (encoded as hex) inside the
// token after the prefix. 24 bytes = 48 hex chars, plenty for collision-free
// uniqueness within a single deployment.
const rawTokenLength = 24

// CreateToken handles POST /admin/tokens.
//
// Body:
//
//	{ "label": "team-A laptop", "quota_limit": 1000 }
//
// On success returns the freshly-generated token exactly once; the caller
// MUST surface it to the admin immediately because the server only stores
// the SHA256 of the token.
func (t *Tokens) CreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label      string `json:"label"`
		QuotaLimit int    `json:"quota_limit"`
		ExpiresAt  int64  `json:"expires_at"` // unix seconds, 0 = never
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON 格式错误")
		return
	}
	if strings.TrimSpace(body.Label) == "" {
		writeJSONError(w, http.StatusBadRequest, "缺少令牌名称")
		return
	}
	if body.QuotaLimit < 0 {
		writeJSONError(w, http.StatusBadRequest, "限额不能为负数")
		return
	}
	raw, hash, prefix, err := generateToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "生成令牌失败:"+err.Error())
		return
	}
	id := "tok-" + shortID()
	row := store.TokenRow{
		ID:         id,
		Label:      body.Label,
		TokenHash:  hash,
		Prefix:     prefix,
		QuotaLimit: body.QuotaLimit,
		QuotaUsed:  0,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Unix(body.ExpiresAt, 0),
		Revoked:    false,
	}
	if err := t.Store.CreateToken(r.Context(), row); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "保存令牌失败:"+err.Error())
		return
	}
	// Map the zero-time back to the zero unix seconds so the response is clean.
	if row.ExpiresAt.Unix() <= 0 {
		row.ExpiresAt = time.Time{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          id,
		"label":       row.Label,
		"token":       raw, // shown ONCE
		"prefix":      prefix,
		"quota_limit": row.QuotaLimit,
		"quota_used":  0,
		"created_at":  row.CreatedAt,
		"expires_at":  row.ExpiresAt,
	})
}

// ListTokens handles GET /admin/tokens. Returns metadata only (no raw token).
func (t *Tokens) ListTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := t.Store.ListTokens(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "获取令牌列表失败:"+err.Error())
		return
	}
	type item struct {
		ID         string    `json:"id"`
		Label      string    `json:"label"`
		Prefix     string    `json:"prefix"`
		QuotaLimit int       `json:"quota_limit"`
		QuotaUsed  int       `json:"quota_used"`
		CreatedAt  time.Time `json:"created_at"`
		ExpiresAt  time.Time `json:"expires_at"`
		LastUsedAt time.Time `json:"last_used_at"`
		Revoked    bool      `json:"revoked"`
	}
	out := make([]item, 0, len(rows))
	for _, r := range rows {
		out = append(out, item{
			ID:         r.ID,
			Label:      r.Label,
			Prefix:     r.Prefix,
			QuotaLimit: r.QuotaLimit,
			QuotaUsed:  r.QuotaUsed,
			CreatedAt:  r.CreatedAt,
			ExpiresAt:  r.ExpiresAt,
			LastUsedAt: r.LastUsedAt,
			Revoked:    r.Revoked,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// RevokeToken handles DELETE /admin/tokens/:id.
func (t *Tokens) RevokeToken(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/tokens/")
	if id == "" || strings.Contains(id, "/") {
		writeJSONError(w, http.StatusBadRequest, "缺少令牌 ID")
		return
	}
	if err := t.Store.RevokeToken(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "撤销失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// RotateToken handles POST /admin/tokens/:id/rotate. Generates a new raw
// token value for the same logical id, returning it ONCE.
func (t *Tokens) RotateToken(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/tokens/")
	id = strings.TrimSuffix(id, "/rotate")
	if id == "" || id == "/rotate" {
		writeJSONError(w, http.StatusBadRequest, "缺少令牌 ID")
		return
	}
	raw, hash, prefix, err := generateToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "生成令牌失败:"+err.Error())
		return
	}
	if err := t.Store.UpdateTokenHash(r.Context(), id, hash, prefix); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "更新令牌失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"token":  raw,
		"prefix": prefix,
	})
}

// ResetTokenQuota handles POST /admin/tokens/:id/reset-quota.
func (t *Tokens) ResetTokenQuota(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/tokens/")
	id = strings.TrimSuffix(id, "/reset-quota")
	if id == "" || id == "/reset-quota" {
		writeJSONError(w, http.StatusBadRequest, "缺少令牌 ID")
		return
	}
	if err := t.Store.ResetTokenUsage(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "重置失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// generateToken returns the raw token (to be shown once), its SHA256 hash
// (for storage and lookup), and a short prefix (for human identification).
func generateToken() (raw, hash, prefix string, err error) {
	buf := make([]byte, rawTokenLength)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", err
	}
	raw = tokenPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	// Prefix = "g2a_" + first 4 bytes hex (8 chars total)
	prefix = raw[:8]
	return raw, hash, prefix, nil
}

// shortID returns 8 hex chars from crypto/rand for token row ids.
func shortID() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// ValidateBearer looks up a token by SHA256 hash. Returns the token row,
// whether it is currently usable, and any reason it isn't. err is non-nil
// only for unexpected DB failures (not for invalid tokens).
func ValidateBearer(ctx context.Context, db *store.DB, raw string) (store.TokenRow, bool, string) {
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	row, err := db.FindTokenByHash(ctx, hash)
	if err != nil {
		return store.TokenRow{}, false, ""
	}
	if row.Revoked {
		return row, false, "令牌已撤销"
	}
	// ExpiresAt stored as unix seconds; 0 means "never expires". We can't use
	// time.Time.IsZero() because time.Unix(0, 0) is 1970-01-01, not the zero
	// value, so check the unix seconds directly.
	if row.ExpiresAt.Unix() > 0 && time.Now().After(row.ExpiresAt) {
		return row, false, "令牌已过期"
	}
	if row.QuotaLimit > 0 && row.QuotaUsed >= row.QuotaLimit {
		return row, false, "令牌已用尽额度"
	}
	return row, true, ""
}
