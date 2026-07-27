// Package server wires up the Gin HTTP server, middleware, and routes.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/user/go2api/internal/cache"
	"github.com/user/go2api/internal/config"
	"github.com/user/go2api/internal/handler"
	"github.com/user/go2api/internal/keypool"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/store"
)

// Deps bundles everything the server needs.
type Deps struct {
	Config *config.Config
	DB     *store.DB
	Pool   *keypool.Pool
	Cache  *cache.Cache
	Proxy  *proxy.Proxy
	Logger *slog.Logger
}

// New builds a configured *gin.Engine.
func New(d Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(loggingMiddleware(d.Logger))

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Friendly landing page at / so users hitting the API port directly see
	// an overview instead of Gin's bare "404 page not found".
	r.GET("/", gin.WrapF(handler.Landing))

	// Replace Gin's default 404 (plain text) with a friendly page that lists
	// available routes and links back to the landing page.
	r.NoRoute(gin.WrapF(handler.NotFound))

	oai := &handler.OpenAI{Proxy: d.Proxy, Cache: d.Cache, Store: d.DB, Logger: d.Logger}
	ant := &handler.Anthropic{Proxy: d.Proxy, Cache: d.Cache, Store: d.DB, Logger: d.Logger}
	rsp := &handler.Responses{Proxy: d.Proxy, Store: d.DB, Logger: d.Logger}
	adm := &handler.Admin{Pool: d.Pool, Store: d.DB}
	tok := &handler.Tokens{Store: d.DB}
	models := http.HandlerFunc(handler.Models)

	// Data-plane auth: accepts both static config tokens and DB-managed tokens
	// (with per-token quota enforcement).
	authMW := authMiddleware(authDeps{
		StaticTokens: d.Config.Server.AuthTokens,
		DB:           d.DB,
		Logger:       d.Logger,
	})

	api := r.Group("/")
	api.Use(authMW)
	{
		api.POST("/v1/chat/completions", gin.WrapH(oai))
		api.POST("/v1/messages", gin.WrapH(ant))
		api.POST("/v1/responses", gin.WrapH(rsp))
		api.GET("/v1/models", gin.WrapH(models))
	}

	// Admin endpoints use the same auth layer.
	admin := r.Group("/admin")
	admin.Use(authMW)
	{
		admin.GET("/keys", gin.WrapF(adm.Keys))
		admin.POST("/keys", gin.WrapF(adm.AddKey))
		admin.PATCH("/keys/:id", gin.WrapF(adm.ToggleKey))
		admin.DELETE("/keys/:id", gin.WrapF(adm.DeleteKey))
		admin.GET("/stats", gin.WrapF(adm.Stats))
		admin.POST("/cache/flush", gin.WrapF(adm.FlushCache))

		admin.GET("/tokens", gin.WrapF(tok.ListTokens))
		admin.POST("/tokens", gin.WrapF(tok.CreateToken))
		admin.DELETE("/tokens/:id", gin.WrapF(tok.RevokeToken))
		admin.POST("/tokens/:id/rotate", gin.WrapF(tok.RotateToken))
		admin.POST("/tokens/:id/reset-quota", gin.WrapF(tok.ResetTokenQuota))
	}

	return r
}

// ListenAndServe is a thin wrapper that honors the configured read timeout.
func ListenAndServe(r http.Handler, addr string, readTimeout time.Duration, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  readTimeout,
		WriteTimeout: 0, // streaming responses can take much longer; let the proxy timeout govern.
		IdleTimeout:  120 * time.Second,
	}
	logger.Info("listening", "addr", addr)
	return srv.ListenAndServe()
}
