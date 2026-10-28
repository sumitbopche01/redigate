// Package gateway is the OpenAI-compatible HTTP surface.
//
// POST /v1/chat/completions proxies to the configured upstream. Cacheable
// requests (non-streaming, temperature omitted or 0) are served from Redis
// when the exact key hits. Streaming and other sampling requests bypass the
// cache.
//
// TODO: coalesce in-flight cache misses on the same key (singleflight) so
// concurrent identical requests share one upstream call.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/sumitbopche01/redigate/internal/cache"
	"github.com/sumitbopche01/redigate/internal/upstream"
)

const (
	cacheHeader      = "X-RediGate-Cache"
	cacheHIT         = "HIT"
	cacheMISS        = "MISS"
	cacheBYPASS      = "BYPASS"
	maxRequestBytes  = 2 << 20
	maxResponseBytes = 8 << 20
	cacheOpTimeout   = 2 * time.Second
)

// responseCache is the exact-match cache. Redis errors are returned so the
// handler can fail open.
type responseCache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte) error
}

// pinger is the readiness dependency (Redis).
type pinger interface {
	Ping(ctx context.Context) error
}

// completer forwards a raw chat-completion body upstream.
type completer interface {
	Complete(ctx context.Context, body []byte) (*upstream.Result, error)
}

// Handler serves health, readiness, and chat completions.
type Handler struct {
	log      *slog.Logger
	cache    responseCache
	ping     pinger
	upstream completer
	mux      *http.ServeMux
}

// New wires the HTTP handler. cache and ping are usually the same Redis store.
func New(log *slog.Logger, cache responseCache, ping pinger, up completer) *Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{log: log, cache: cache, ping: ping, upstream: up}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	h.mux = mux
	return h
}

// ServeHTTP recovers handler panics and dispatches the route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			h.log.Error("panic", "err", rec)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		}
	}()
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, "ok")
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.ping == nil {
		writeStatus(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.ping.Ping(ctx); err != nil {
		h.log.Warn("redis ping failed", "err", err)
		writeStatus(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeStatus(w, http.StatusOK, "ok")
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "Content-Type must be application/json")
		return
	}
	body, err := readBody(r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	id, err := cache.Inspect(body)
	if err != nil {
		if cache.IsValidation(err) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		h.log.Error("cache identity failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to process request")
		return
	}

	started := time.Now()
	if !id.Cacheable {
		h.forward(w, r, body, id, cacheBYPASS, started)
		return
	}
	if h.writeHit(w, r, id, started) {
		return
	}
	h.forward(w, r, body, id, cacheMISS, started)
}

func (h *Handler) writeHit(w http.ResponseWriter, r *http.Request, id cache.Identity, started time.Time) bool {
	ctx, cancel := context.WithTimeout(r.Context(), cacheOpTimeout)
	defer cancel()
	got, ok, err := h.cache.Get(ctx, id.Key)
	if err != nil {
		h.log.Warn("redis get failed; forwarding", "err", err, "model", id.Model)
		return false
	}
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	setCacheHeader(w.Header(), cacheHIT)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(got)
	h.logCompletion(id.Model, cacheHIT, http.StatusOK, started)
	return true
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, id cache.Identity, state string, started time.Time) {
	res, err := h.upstream.Complete(r.Context(), body)
	if err != nil {
		h.log.Error("upstream request failed", "err", err, "model", id.Model)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		return
	}
	defer res.Body.Close()

	store := state == cacheMISS && id.Cacheable && res.StatusCode == http.StatusOK && jsonBody(res.Header)
	if !store {
		writeProxied(w, res, state)
		h.logCompletion(id.Model, state, res.StatusCode, started)
		return
	}

	buf, tooLarge, err := readAtMost(res.Body, maxResponseBytes)
	if err != nil {
		h.log.Error("read upstream response", "err", err, "model", id.Model)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream response could not be read")
		return
	}
	if tooLarge {
		h.log.Warn("upstream response exceeds cache limit", "model", id.Model, "limit", maxResponseBytes)
		writeReader(w, res, io.MultiReader(bytes.NewReader(buf), res.Body), state)
		h.logCompletion(id.Model, state, http.StatusOK, started)
		return
	}
	if len(buf) > 0 {
		setCtx, cancel := context.WithTimeout(context.Background(), cacheOpTimeout)
		err := h.cache.Set(setCtx, id.Key, buf)
		cancel()
		if err != nil {
			h.log.Warn("redis set failed", "err", err, "model", id.Model)
		}
	}
	writeBuffered(w, res, buf, state)
	h.logCompletion(id.Model, state, http.StatusOK, started)
}

func (h *Handler) logCompletion(model, state string, status int, started time.Time) {
	h.log.Info("chat completion",
		"model", model,
		"cache", state,
		"status", status,
		"duration", time.Since(started),
	)
}

func writeProxied(w http.ResponseWriter, res *upstream.Result, state string) {
	writeReader(w, res, res.Body, state)
}

func writeReader(w http.ResponseWriter, res *upstream.Result, body io.Reader, state string) {
	copyHeaders(w.Header(), res.Header)
	setCacheHeader(w.Header(), state)
	w.WriteHeader(res.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeBuffered(w http.ResponseWriter, res *upstream.Result, buf []byte, state string) {
	copyHeaders(w.Header(), res.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	setCacheHeader(w.Header(), state)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// setCacheHeader writes the product header with its exact spelling.
// Header.Set would canonicalize X-RediGate-Cache to X-Redigate-Cache.
func setCacheHeader(h http.Header, state string) {
	delete(h, "X-Redigate-Cache")
	h[cacheHeader] = []string{state}
}

func copyHeaders(dst, src http.Header) {
	hop := map[string]struct{}{
		"connection":        {},
		"proxy-connection":  {},
		"keep-alive":        {},
		"transfer-encoding": {},
		"te":                {},
		"trailer":           {},
		"upgrade":           {},
		"content-length":    {},
		"x-redigate-cache":  {},
	}
	for _, c := range src.Values("Connection") {
		for _, f := range strings.Split(c, ",") {
			if name := strings.ToLower(strings.TrimSpace(f)); name != "" {
				hop[name] = struct{}{}
			}
		}
	}
	for k, vals := range src {
		if _, skip := hop[strings.ToLower(k)]; skip {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

var errBodyTooLarge = errors.New("body too large")

func readBody(r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBytes)
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return b, nil
}

func readAtMost(r io.Reader, n int64) (buf []byte, tooLarge bool, err error) {
	buf, err = io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > n {
		return buf, true, nil
	}
	return buf, false, nil
}

func jsonBody(h http.Header) bool {
	ct := h.Get("Content-Type")
	return ct == "" || isJSON(ct)
}

func isJSON(ct string) bool {
	media, _, err := mime.ParseMediaType(ct)
	return err == nil && media == "application/json"
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    typ,
		},
	})
}

func writeStatus(w http.ResponseWriter, status int, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": state})
}
