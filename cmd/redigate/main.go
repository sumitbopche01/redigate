// Command redigate is a Redis-backed OpenAI-compatible chat gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sumitbopche01/redigate/internal/cache"
	"github.com/sumitbopche01/redigate/internal/config"
	"github.com/sumitbopche01/redigate/internal/gateway"
	"github.com/sumitbopche01/redigate/internal/upstream"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("redigate stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = rdb.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	client, err := upstream.New(cfg.UpstreamBaseURL, cfg.UpstreamAPIKey, nil)
	if err != nil {
		return err
	}
	store := cache.NewStore(rdb, cfg.CacheTTL)
	handler := gateway.New(slog.Default(), store, store, client)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening",
			"addr", cfg.ListenAddr,
			"upstream", cfg.UpstreamBaseURL,
			"redis", opt.Addr,
			"cache_ttl", cfg.CacheTTL.String(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
