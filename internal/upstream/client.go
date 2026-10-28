// Package upstream forwards chat-completion requests to an OpenAI-compatible
// HTTP server. The caller supplies the request body unchanged. Client
// credentials come from configuration, not from the inbound request.
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	userAgent             = "RediGate/0.1"
	responseHeaderTimeout = 5 * time.Minute
)

// Result is an upstream HTTP response. The caller must close Body.
type Result struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// Client is a minimal OpenAI-compatible chat completions client.
type Client struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// New builds a client. baseURL is the upstream origin, with or without a
// trailing /v1 (https://api.openai.com and http://127.0.0.1:11434/v1 both work).
// An empty apiKey omits Authorization, which suits local servers.
func New(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	endpoint, err := completionsURL(baseURL)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = defaultHTTPClient()
	}
	return &Client{
		endpoint: endpoint,
		apiKey:   strings.TrimSpace(apiKey),
		http:     hc,
	}, nil
}

// Complete posts the raw chat-completion body to the upstream.
func (c *Client) Complete(ctx context.Context, body []byte) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	return &Result{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
	}, nil
}

func completionsURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("UPSTREAM_BASE_URL must be an absolute http(s) URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/chat/completions"
	} else {
		u.Path += "/v1/chat/completions"
	}
	return u.String(), nil
}

func defaultHTTPClient() *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	// Leave the body bytes untouched so streaming responses are not buffered
	// by transparent gzip.
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Transport: tr,
		// Do not follow redirects. A redirect would move the API key to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
