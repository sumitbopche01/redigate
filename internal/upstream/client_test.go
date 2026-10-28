package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompletionsURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://api.openai.com", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "https://api.openai.com/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1/", "http://127.0.0.1:11434/v1/chat/completions"},
		{"https://example.com/openai", "https://example.com/openai/v1/chat/completions"},
	}
	for _, tc := range cases {
		got, err := completionsURL(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%s -> %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "ftp://example.com", "api.openai.com", "http://user:pass@example.com"} {
		if _, err := completionsURL(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestClientForwardsBodyAndAuth(t *testing.T) {
	var gotBody, gotAuth, gotPath, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "sk-test", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Complete(context.Background(), []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gotUA != userAgent {
		t.Fatalf("ua %q", gotUA)
	}
	if gotBody != `{"model":"m"}` {
		t.Fatalf("body %s", gotBody)
	}
}

func TestClientOmitsAuthWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected auth %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/v1", "  ", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.endpoint, "/v1/chat/completions") {
		t.Fatalf("endpoint %s", c.endpoint)
	}
	res, err := c.Complete(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

func TestClientNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(srv.URL, "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	_, err = c.Complete(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
}
