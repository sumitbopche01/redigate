// Package config loads RediGate settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRedisURL   = "redis://127.0.0.1:6379/0"
	defaultListenAddr = ":8080"
	defaultCacheTTL   = 3600
)

// Config is the process configuration. Secrets stay in UpstreamAPIKey and are
// not logged by the server.
type Config struct {
	RedisURL        string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	ListenAddr      string
	CacheTTL        time.Duration
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	redisURL := strings.TrimSpace(getenv("REDIS_URL"))
	if redisURL == "" {
		redisURL = defaultRedisURL
	}

	upstream := strings.TrimSpace(getenv("UPSTREAM_BASE_URL"))
	if upstream == "" {
		return Config{}, fmt.Errorf("UPSTREAM_BASE_URL is required")
	}

	listen := strings.TrimSpace(getenv("LISTEN_ADDR"))
	if listen == "" {
		listen = defaultListenAddr
	}

	ttl, err := cacheTTL(getenv("CACHE_TTL_SECONDS"))
	if err != nil {
		return Config{}, err
	}

	return Config{
		RedisURL:        redisURL,
		UpstreamBaseURL: upstream,
		UpstreamAPIKey:  strings.TrimSpace(getenv("UPSTREAM_API_KEY")),
		ListenAddr:      listen,
		CacheTTL:        ttl,
	}, nil
}

func cacheTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCacheTTL * time.Second, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("CACHE_TTL_SECONDS must be a positive integer")
	}
	return time.Duration(n) * time.Second, nil
}
