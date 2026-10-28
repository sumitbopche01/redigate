package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sumitbopche01/redigate/internal/cache"
	"github.com/sumitbopche01/redigate/internal/upstream"
)

func TestChatCompletionsMissThenHit(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusOK, payload: []byte(`{"id":"c1","object":"chat.completion"}`)}
	h, _ := newStack(t, up)
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)

	rec := post(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.Bytes())
	}
	if cacheOf(rec.Header()) != cacheMISS {
		t.Fatalf("cache header %q", cacheOf(rec.Header()))
	}
	if _, dup := rec.Header()["X-Redigate-Cache"]; dup {
		t.Fatal("duplicate canonical cache header")
	}
	if rec.Body.String() != `{"id":"c1","object":"chat.completion"}` {
		t.Fatalf("body %s", rec.Body.String())
	}
	if up.calls() != 1 {
		t.Fatalf("upstream calls %d", up.calls())
	}
	if !bytes.Equal(up.lastBody(), body) {
		t.Fatalf("forwarded %s", up.lastBody())
	}

	rec = post(t, h, body)
	if cacheOf(rec.Header()) != cacheHIT {
		t.Fatalf("second cache header %q", cacheOf(rec.Header()))
	}
	if rec.Body.String() != `{"id":"c1","object":"chat.completion"}` {
		t.Fatalf("hit body %s", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
	}
	if up.calls() != 1 {
		t.Fatalf("upstream calls after hit %d", up.calls())
	}
}

func TestNormalizedMessagesShareOneUpstreamCall(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusOK, payload: []byte(`{"id":"norm"}`)}
	h, _ := newStack(t, up)
	first := []byte(`{"model":"m","messages":[{"role":"User","content":"  hi  ","name":" "}]}`)
	second := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"m","temperature":0}`)

	if cacheOf(post(t, h, first).Header()) != cacheMISS {
		t.Fatal("expected miss")
	}
	rec := post(t, h, second)
	if cacheOf(rec.Header()) != cacheHIT {
		t.Fatalf("header %q", cacheOf(rec.Header()))
	}
	if rec.Body.String() != `{"id":"norm"}` {
		t.Fatalf("body %s", rec.Body.String())
	}
	if up.calls() != 1 {
		t.Fatalf("calls %d", up.calls())
	}
	if !bytes.Equal(up.lastBody(), first) {
		t.Fatalf("stored response came from a different request than the first body: %s", up.lastBody())
	}
}

func TestTemperatureBypassDoesNotPoisonCache(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusOK, payload: []byte(`{"id":"hot"}`)}
	h, _ := newStack(t, up)
	hot := []byte(`{"model":"m","temperature":1,"messages":[{"role":"user","content":"hi"}]}`)
	cold := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	rec := post(t, h, hot)
	if cacheOf(rec.Header()) != cacheBYPASS {
		t.Fatalf("header %q", cacheOf(rec.Header()))
	}
	up.payload = []byte(`{"id":"cold"}`)
	rec = post(t, h, cold)
	if cacheOf(rec.Header()) != cacheMISS {
		t.Fatalf("cold header %q body %s", cacheOf(rec.Header()), rec.Body.String())
	}
	if rec.Body.String() != `{"id":"cold"}` {
		t.Fatalf("poisoned cache: %s", rec.Body.String())
	}
	if up.calls() != 2 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestStreamBypassDoesNotPoisonCache(t *testing.T) {
	up := &scriptedUpstream{
		status:  http.StatusOK,
		header:  http.Header{"Content-Type": []string{"text/event-stream"}},
		payload: []byte("data: hi\n\n"),
	}
	h, _ := newStack(t, up)
	stream := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	rec := post(t, h, stream)
	if cacheOf(rec.Header()) != cacheBYPASS {
		t.Fatalf("header %q", cacheOf(rec.Header()))
	}
	if rec.Body.String() != "data: hi\n\n" {
		t.Fatalf("stream body %q", rec.Body.String())
	}
	if !rec.Flushed {
		t.Fatal("expected the stream to be flushed")
	}
	up.header = nil
	up.payload = []byte(`{"id":"plain"}`)
	rec = post(t, h, plain)
	if cacheOf(rec.Header()) != cacheMISS || rec.Body.String() != `{"id":"plain"}` {
		t.Fatalf("plain header %q body %s", cacheOf(rec.Header()), rec.Body.String())
	}
	rec = post(t, h, stream)
	if cacheOf(rec.Header()) != cacheBYPASS {
		t.Fatal("second stream should still bypass")
	}
	if up.calls() != 3 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestUpstreamErrorIsNotCached(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusTooManyRequests, payload: []byte(`{"error":{"message":"slow down"}}`)}
	h, _ := newStack(t, up)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 2; i++ {
		rec := post(t, h, body)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status %d", rec.Code)
		}
		if cacheOf(rec.Header()) != cacheMISS {
			t.Fatalf("header %q", cacheOf(rec.Header()))
		}
		if rec.Body.String() != `{"error":{"message":"slow down"}}` {
			t.Fatalf("body %s", rec.Body.String())
		}
	}
	if up.calls() != 2 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestOversizedUpstreamIsProxiedUncached(t *testing.T) {
	payload := append(bytes.Repeat([]byte("a"), maxResponseBytes), 'b')
	up := &scriptedUpstream{status: http.StatusOK, payload: payload}
	h, _ := newStack(t, up)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	rec := post(t, h, body)
	if rec.Code != http.StatusOK || cacheOf(rec.Header()) != cacheMISS {
		t.Fatalf("status %d header %q len %d", rec.Code, cacheOf(rec.Header()), rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("proxied len %d want %d", rec.Body.Len(), len(payload))
	}
	if cacheOf(post(t, h, body).Header()) != cacheMISS {
		t.Fatal("oversized response was cached")
	}
	if up.calls() != 2 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestPlainTextSuccessIsNotCached(t *testing.T) {
	up := &scriptedUpstream{
		status:  http.StatusOK,
		header:  http.Header{"Content-Type": []string{"text/plain"}},
		payload: []byte("not-json"),
	}
	h, _ := newStack(t, up)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 2; i++ {
		rec := post(t, h, body)
		if rec.Code != http.StatusOK || cacheOf(rec.Header()) != cacheMISS {
			t.Fatalf("status %d header %q", rec.Code, cacheOf(rec.Header()))
		}
		if rec.Body.String() != "not-json" {
			t.Fatalf("body %q", rec.Body.String())
		}
	}
	if up.calls() != 2 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestUpstreamTransportError(t *testing.T) {
	up := &scriptedUpstream{err: errors.New("dial tcp: refused")}
	h, _ := newStack(t, up)
	rec := post(t, h, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if cacheOf(rec.Header()) != "" {
		t.Fatalf("unexpected cache header %q", cacheOf(rec.Header()))
	}
}

func TestCacheGetFailureFailsOpen(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusOK, payload: []byte(`{"id":"live"}`)}
	h := New(discardLog(), brokenCache{}, okPing{}, up)
	rec := post(t, h, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK || cacheOf(rec.Header()) != cacheMISS {
		t.Fatalf("status %d header %q body %s", rec.Code, cacheOf(rec.Header()), rec.Body.String())
	}
	if rec.Body.String() != `{"id":"live"}` {
		t.Fatalf("body %s", rec.Body.String())
	}
	if up.calls() != 1 {
		t.Fatalf("calls %d", up.calls())
	}
}

func TestChatValidationAndMediaType(t *testing.T) {
	h, _ := newStack(t, &scriptedUpstream{status: http.StatusOK, payload: []byte(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestBodyTooLarge(t *testing.T) {
	h, _ := newStack(t, &scriptedUpstream{status: http.StatusOK, payload: []byte(`{}`)})
	payload := bytes.Repeat([]byte("a"), maxRequestBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestHealthzIgnoresRedis(t *testing.T) {
	h := New(discardLog(), brokenCache{}, brokenPing{}, &scriptedUpstream{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestReadyz(t *testing.T) {
	up := &scriptedUpstream{status: http.StatusOK, payload: []byte(`{}`)}
	h, mr := newStack(t, up)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status %d body %s", rec.Code, rec.Body.String())
	}
	mr.Close()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestRealUpstreamMissThenHit(t *testing.T) {
	var calls int
	var mu sync.Mutex
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"model":"stack"`)) {
			t.Errorf("body %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Call", "yes")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"stack-%d"}`, n))
	}))
	t.Cleanup(origin.Close)

	client, err := upstream.New(origin.URL, "sk-test", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := cache.NewStore(rdb, time.Hour)
	h := New(discardLog(), store, store, client)

	body := []byte(`{"model":"stack","messages":[{"role":"user","content":"ping"}]}`)
	miss := post(t, h, body)
	if miss.Code != http.StatusOK || cacheOf(miss.Header()) != cacheMISS {
		t.Fatalf("miss status %d header %q body %s", miss.Code, cacheOf(miss.Header()), miss.Body.String())
	}
	if miss.Header().Get("X-Upstream-Call") != "yes" {
		t.Fatal("expected upstream header on miss")
	}
	hit := post(t, h, body)
	if cacheOf(hit.Header()) != cacheHIT {
		t.Fatalf("hit header %q body %s", cacheOf(hit.Header()), hit.Body.String())
	}
	if hit.Body.String() != miss.Body.String() {
		t.Fatalf("hit body %s miss body %s", hit.Body.String(), miss.Body.String())
	}
	if hit.Header().Get("X-Upstream-Call") != "" {
		t.Fatal("hit should not replay upstream headers")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls %d", calls)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newStack(t, &scriptedUpstream{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func cacheOf(h http.Header) string {
	v := h[cacheHeader]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func newStack(t *testing.T, up completer) (*Handler, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := cache.NewStore(rdb, time.Hour)
	return New(discardLog(), store, store, up), mr
}

func post(t *testing.T, h http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type scriptedUpstream struct {
	mu      sync.Mutex
	bodies  [][]byte
	status  int
	header  http.Header
	payload []byte
	err     error
}

func (s *scriptedUpstream) Complete(ctx context.Context, body []byte) (*upstream.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, append([]byte(nil), body...))
	if s.err != nil {
		return nil, s.err
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	hdr := s.header.Clone()
	if hdr == nil {
		hdr = make(http.Header)
	}
	if hdr.Get("Content-Type") == "" && status == http.StatusOK {
		hdr.Set("Content-Type", "application/json")
	}
	return &upstream.Result{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(bytes.NewReader(append([]byte(nil), s.payload...))),
	}, nil
}

func (s *scriptedUpstream) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *scriptedUpstream) lastBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

type brokenCache struct{}

func (brokenCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("redis down")
}

func (brokenCache) Set(context.Context, string, []byte) error {
	return errors.New("redis down")
}

type okPing struct{}

func (okPing) Ping(context.Context) error { return nil }

type brokenPing struct{}

func (brokenPing) Ping(context.Context) error { return errors.New("redis down") }
