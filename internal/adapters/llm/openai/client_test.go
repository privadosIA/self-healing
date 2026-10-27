package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/safehttp"
)

const testKey = "changeme-llm-key"

func httpClient(t *testing.T, allow string, timeout time.Duration) *http.Client {
	t.Helper()
	al, err := config.ParseAllowlist(allow)
	if err != nil {
		t.Fatal(err)
	}
	return safehttp.NewClient(al, timeout)
}

// captured is what the fake server saw.
type captured struct {
	path   string
	auth   string
	ctype  string
	method string
	body   chatRequest
	calls  int
}

func newServer(t *testing.T, cap *captured, handle func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.calls++
		cap.path, cap.method = r.URL.Path, r.Method
		cap.auth, cap.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&cap.body)
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okHandler(content string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 812, "completion_tokens": 96},
		})
	}
}

func newClient(t *testing.T, baseURL, key string, timeout time.Duration) *Client {
	t.Helper()
	c, err := NewClient(Config{BaseURL: baseURL, APIKey: key, Model: "local-model", Client: httpClient(t, "127.0.0.1", timeout)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatSendsTheExpectedRequest(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler(`{"ok":true}`))
	c := newClient(t, srv.URL+"/v1/", testKey, 2*time.Second)

	got, err := c.Chat(context.Background(), "system text", `{"user":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != `{"ok":true}` || got.TokensIn != 812 || got.TokensOut != 96 {
		t.Errorf("completion = %+v", got)
	}
	if cap.method != http.MethodPost || cap.path != "/v1/chat/completions" {
		t.Errorf("%s %s", cap.method, cap.path)
	}
	if cap.auth != "Bearer "+testKey || cap.ctype != "application/json" {
		t.Errorf("auth %q, content-type %q", cap.auth, cap.ctype)
	}
	b := cap.body
	if b.Model != "local-model" || b.Temperature != 0.1 || b.ResponseFormat["type"] != "json_object" {
		t.Errorf("body = %+v", b)
	}
	if len(b.Messages) != 2 || b.Messages[0] != (message{"system", "system text"}) || b.Messages[1] != (message{"user", `{"user":1}`}) {
		t.Errorf("messages = %+v", b.Messages)
	}
}

func TestNoAuthorizationHeaderWithoutAKey(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler("{}"))
	if _, err := newClient(t, srv.URL+"/v1", "", 2*time.Second).Chat(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if cap.auth != "" {
		t.Errorf("Authorization = %q, want none", cap.auth)
	}
}

func TestBaseURLWithoutTrailingSlash(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler("{}"))
	if _, err := newClient(t, srv.URL+"/v1", "", 2*time.Second).Chat(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/v1/chat/completions" {
		t.Errorf("path = %q", cap.path)
	}
}

func TestChatErrors(t *testing.T) {
	const secretBody = "echoed source code and changeme-leak"
	tests := []struct {
		name    string
		handle  func(http.ResponseWriter, *http.Request)
		wantErr string
		is      error
	}{
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, secretBody, http.StatusUnauthorized) }, "HTTP 401", nil},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, secretBody, http.StatusInternalServerError)
		}, "HTTP 500", nil},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, secretBody, http.StatusTooManyRequests) }, "HTTP 429", nil},
		{"invalid json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{nope")) }, "invalid response", nil},
		{"no choices", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"choices":[]}`)) }, "no choices", nil},
		{"truncated", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"a\":"},"finish_reason":"length"}]}`))
		}, "truncated", ErrTruncated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cap captured
			srv := newServer(t, &cap, tt.handle)
			_, err := newClient(t, srv.URL+"/v1", testKey, 2*time.Second).Chat(context.Background(), "SECRET-PROMPT", "u")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
			for _, leak := range []string{secretBody, "changeme-leak", "SECRET-PROMPT", testKey} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error leaks %q: %v", leak, err)
				}
			}
		})
	}
}

func TestChatTimesOut(t *testing.T) {
	var cap captured
	release := make(chan struct{})
	srv := newServer(t, &cap, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)

	start := time.Now()
	_, err := newClient(t, srv.URL+"/v1", "", 100*time.Millisecond).Chat(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v to give up", elapsed)
	}
}

func TestChatHonorsContextCancellation(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler("{}"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient(t, srv.URL+"/v1", "", 2*time.Second).Chat(ctx, "s", "u"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if cap.calls != 0 {
		t.Error("a cancelled request reached the server")
	}
}

func TestClientHonorsTheAllowlist(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler("{}"))
	c, err := NewClient(Config{BaseURL: srv.URL + "/v1", Model: "m", Client: httpClient(t, "10.0.0.0/8", time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), "s", "u"); !errors.Is(err, safehttp.ErrHostNotAllowed) {
		t.Errorf("err = %v, want ErrHostNotAllowed", err)
	}
	if cap.calls != 0 {
		t.Error("the server was contacted despite the allowlist")
	}
}

func TestNewClientRejectsBadConfig(t *testing.T) {
	hc := httpClient(t, "example.com", time.Second)
	for name, cfg := range map[string]Config{
		"not a url":  {BaseURL: "llm", Model: "m", Client: hc},
		"bad scheme": {BaseURL: "ftp://llm.example.com", Model: "m", Client: hc},
		"no model":   {BaseURL: "http://llm.example.com/v1", Client: hc},
		"no client":  {BaseURL: "http://llm.example.com/v1", Model: "m"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
