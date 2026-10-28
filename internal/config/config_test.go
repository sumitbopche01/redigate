package config

import "testing"
import "time"

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(func(k string) string {
		if k == "UPSTREAM_BASE_URL" {
			return "https://api.openai.com"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != defaultRedisURL {
		t.Fatalf("redis url %q", cfg.RedisURL)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Fatalf("listen %q", cfg.ListenAddr)
	}
	if cfg.CacheTTL != time.Hour {
		t.Fatalf("ttl %s", cfg.CacheTTL)
	}
	if cfg.UpstreamAPIKey != "" {
		t.Fatalf("api key %q", cfg.UpstreamAPIKey)
	}
	if cfg.UpstreamBaseURL != "https://api.openai.com" {
		t.Fatalf("upstream %q", cfg.UpstreamBaseURL)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := map[string]string{
		"REDIS_URL":         "redis://redis:6379/1",
		"UPSTREAM_BASE_URL": " http://127.0.0.1:11434/v1 ",
		"UPSTREAM_API_KEY":  " sk-test ",
		"LISTEN_ADDR":       ":9090",
		"CACHE_TTL_SECONDS": "30",
	}
	cfg, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "redis://redis:6379/1" || cfg.ListenAddr != ":9090" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.UpstreamBaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("upstream %q", cfg.UpstreamBaseURL)
	}
	if cfg.UpstreamAPIKey != "sk-test" {
		t.Fatalf("key %q", cfg.UpstreamAPIKey)
	}
	if cfg.CacheTTL != 30*time.Second {
		t.Fatalf("ttl %s", cfg.CacheTTL)
	}
}

func TestLoadRejectsMissingUpstreamAndBadTTL(t *testing.T) {
	if _, err := load(func(string) string { return "" }); err == nil {
		t.Fatal("expected missing upstream error")
	}
	_, err := load(func(k string) string {
		switch k {
		case "UPSTREAM_BASE_URL":
			return "http://example"
		case "CACHE_TTL_SECONDS":
			return "0"
		default:
			return ""
		}
	})
	if err == nil {
		t.Fatal("expected ttl error")
	}
	_, err = load(func(k string) string {
		switch k {
		case "UPSTREAM_BASE_URL":
			return "http://example"
		case "CACHE_TTL_SECONDS":
			return "nope"
		default:
			return ""
		}
	})
	if err == nil {
		t.Fatal("expected ttl parse error")
	}
}
