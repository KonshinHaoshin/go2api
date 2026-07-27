// Command server is the entry point for go2api.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/user/go2api/internal/cache"
	"github.com/user/go2api/internal/config"
	"github.com/user/go2api/internal/keypool"
	"github.com/user/go2api/internal/proxy"
	"github.com/user/go2api/internal/server"
	"github.com/user/go2api/internal/store"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to YAML config")
	dbPath := flag.String("db", "data/go2api.db", "path to SQLite database file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"listen", cfg.Server.Listen,
		"strategy", cfg.KeyPool.Strategy,
		"keys", len(cfg.KeyPool.Keys),
		"cache_ttl", cfg.Cache.TTL,
	)
	if len(cfg.KeyPool.Keys) == 0 {
		logger.Warn("keypool.keys is empty — all proxy requests will fail with 'no key available' until a key is added via config or the admin API")
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		logger.Error("ensure db dir", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := keypool.New(rootCtx, cfg.KeyPool, db, logger)
	if err != nil {
		logger.Error("init keypool", "err", err)
		os.Exit(1)
	}

	c := cache.New(db, cfg.Cache.TTL, cfg.Cache.SkipStreaming, cfg.Cache.MaxBytes, logger)
	if cfg.Cache.Enabled {
		stop := c.StartGC(rootCtx, 5*time.Minute)
		defer stop()
	}
	// Phase 5: response state + conversation chain GC, independent of
	// cache.Enable. State retention is correctness data, not optional.
	startResponseStateGC(rootCtx, db, 5*time.Minute, logger)

	upstream := proxy.New(cfg.Upstream.BaseURL, cfg.Upstream.Timeout)
	prx := proxy.NewProxy(upstream, pool, logger)

	engine := server.New(server.Deps{
		Config: cfg,
		DB:     db,
		Pool:   pool,
		Cache:  c,
		Proxy:  prx,
		Logger: logger,
	})

	if err := server.ListenAndServe(engine, cfg.Server.Listen, time.Duration(cfg.Server.ReadTimeout)*time.Second, logger); err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
}

// startResponseStateGC deletes expired response_state + conversation rows on
// a fixed cadence. Mirrors cache.StartGC but is unconditional — chain state
// is correctness, not cached HTTP entities.
func startResponseStateGC(ctx context.Context, db *store.DB, every time.Duration, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := db.ResponseStateGC(ctx); err != nil {
					logger.Warn("responses: state GC failed", "err", err)
				} else if n > 0 {
					logger.Info("responses: state GC", "deleted", n)
				}
			}
		}
	}()
}
