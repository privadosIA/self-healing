package logging

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/config"
)

func newLogger(t *testing.T, level string) (*bytes.Buffer, func(string, ...any), func(string, ...any)) {
	t.Helper()
	var buf bytes.Buffer
	l, err := New(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	return &buf,
		func(msg string, args ...any) { l.Info(msg, args...) },
		func(msg string, args ...any) { Sensitive(ctx, l, msg, args...) }
}

func TestRedactsCredentialsByKey(t *testing.T) {
	buf, info, _ := newLogger(t, "info")
	info("call",
		"Authorization", "Bearer changeme-token",
		"gitea_token", "changeme-gitea",
		"webhook_secret", "changeme-hmac",
		"X-Gitea-Signature", "sha256=0123",
		"api_key", "changeme-key",
		"repo", "acme/widget",
	)
	out := buf.String()
	for _, leak := range []string{"changeme-token", "changeme-gitea", "changeme-hmac", "sha256=0123", "changeme-key"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "acme/widget") {
		t.Errorf("non-sensitive attribute missing: %s", out)
	}
}

func TestRedactsInsideGroupsAndHeaders(t *testing.T) {
	buf, info, _ := newLogger(t, "info")
	h := http.Header{}
	h.Set("Authorization", "Bearer changeme-token")
	h.Set("Content-Type", "application/json")
	info("request", "headers", Headers(h))
	out := buf.String()
	if strings.Contains(out, "changeme-token") {
		t.Errorf("header value leaks: %s", out)
	}
	if !strings.Contains(out, "application/json") {
		t.Errorf("benign header dropped: %s", out)
	}
}

func TestConfigSecretNeverLogged(t *testing.T) {
	buf, info, _ := newLogger(t, "info")
	info("cfg", "value", config.Secret("changeme-secret"))
	if strings.Contains(buf.String(), "changeme-secret") {
		t.Errorf("config.Secret leaks: %s", buf.String())
	}
}

func TestSensitiveContentOnlyAtDebug(t *testing.T) {
	tests := []struct {
		level   string
		wantOut bool
	}{
		{"info", false},
		{"warn", false},
		{"error", false},
		{"debug", true},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			buf, _, sensitive := newLogger(t, tt.level)
			sensitive("llm request", "prompt", "fix this snippet", "Authorization", "Bearer changeme-token")
			out := buf.String()
			if got := strings.Contains(out, "fix this snippet"); got != tt.wantOut {
				t.Errorf("prompt logged = %v, want %v (%s)", got, tt.wantOut, out)
			}
			if strings.Contains(out, "changeme-token") {
				t.Errorf("Authorization leaks even at %s: %s", tt.level, out)
			}
		})
	}
}

func TestOutputIsJSON(t *testing.T) {
	buf, info, _ := newLogger(t, "info")
	info("hello", "k", "v")
	if got := strings.TrimSpace(buf.String()); !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("not JSON: %s", got)
	}
}

func TestInvalidLevel(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "verbose"); err == nil {
		t.Error("expected error for unknown level")
	}
}
