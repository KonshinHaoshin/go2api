package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/user/go2api/internal/handler"
	"github.com/user/go2api/internal/store"
)

// loggingMiddleware writes a structured access-log line for every request.
func loggingMiddleware(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"latency_ms", time.Since(start).Milliseconds(),
			"remote", c.ClientIP(),
		)
	}
}

// authDeps bundles everything the auth middleware needs to decide whether a
// request is allowed.
type authDeps struct {
	// StaticTokens are the values listed in configs/config.yaml under
	// server.auth_tokens. Compared in constant time.
	StaticTokens []string
	// DB may hold additional managed tokens (see handler.Tokens). When set,
	// the middleware also accepts tokens whose SHA256 hash matches a row in
	// the tokens table, enforces per-token quota, and increments usage.
	DB *store.DB
	// Logger receives a structured line for every DB-token acceptance.
	Logger *slog.Logger
}

// authMiddleware verifies the Authorization header. It tries (in order):
//
//  1. The static tokens from configs/config.yaml (constant-time compare).
//  2. A SHA256-hashed row in the DB tokens table. On success the row's
//     quota_used counter is incremented (subject to quota_limit).
//
// If no tokens are configured and DB is nil, the middleware is open.
//
// When the request is rejected it returns a JSON error body so the browser
// sees a helpful message instead of a bare 401.
func authMiddleware(d authDeps) gin.HandlerFunc {
	if len(d.StaticTokens) == 0 && d.DB == nil {
		return func(c *gin.Context) { c.Next() }
	}
	want := make([][]byte, len(d.StaticTokens))
	for i, t := range d.StaticTokens {
		want[i] = []byte(t)
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(c *gin.Context) {
		// Skip auth for health, the SPA fallback, and OPTIONS preflight.
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		p := c.Request.URL.Path
		if p == "/healthz" || p == "/" {
			c.Next()
			return
		}

		header := c.Request.Header.Get("Authorization")
		if header == "" {
			header = c.Request.Header.Get("x-api-key")
		}
		got := []byte(header)
		if len(got) > 7 {
			prefix := strings.ToLower(string(got[:7]))
			if prefix == "bearer " {
				got = got[7:]
			}
		}

		if len(got) == 0 {
			abort401(c, "缺少 Authorization 头")
			return
		}

		// 1. Static tokens.
		if matchStatic(want, got) {
			c.Next()
			return
		}

		// 2. DB-backed tokens (with quota enforcement).
		if d.DB != nil {
			row, ok, reason := handler.ValidateBearer(c.Request.Context(), d.DB, string(got))
			if ok {
				if err := d.DB.IncrementTokenUsage(c.Request.Context(), row.ID, row.QuotaLimit); err != nil {
					// Race: token hit its quota between ValidateBearer and IncrementTokenUsage.
					abort429(c, "令牌已用尽额度")
					return
				}
				c.Set("token_id", row.ID)
				c.Set("token_label", row.Label)
				c.Next()
				return
			}
			if reason != "" {
				if strings.Contains(reason, "额度") {
					abort429(c, reason)
					return
				}
				abort401(c, reason)
				return
			}
			// err != nil -> not in DB; fall through to 401.
		}

		abort401(c, "令牌无效")
	}
}

func matchStatic(want [][]byte, got []byte) bool {
	if len(got) == 0 {
		return false
	}
	for _, t := range want {
		if subtle.ConstantTimeCompare(t, got) == 1 {
			return true
		}
	}
	return false
}

func abort401(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{"message": msg, "type": "auth_error"},
	})
}

func abort429(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{"message": msg, "type": "quota_exceeded"},
	})
}
