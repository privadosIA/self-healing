package safehttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
)

func allow(t *testing.T, s string) config.Allowlist {
	t.Helper()
	al, err := config.ParseAllowlist(s)
	if err != nil {
		t.Fatal(err)
	}
	return al
}

func TestAllowedHostIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()

	resp, err := NewClient(allow(t, "127.0.0.1"), time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowed host failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDisallowedHostIsRefused(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()

	tests := []struct {
		name      string
		allowlist string
	}{
		{"ip outside the cidr", "10.0.0.0/8"},
		{"only a domain suffix", "example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(allow(t, tt.allowlist), time.Second).Get(srv.URL)
			if !errors.Is(err, ErrHostNotAllowed) {
				t.Fatalf("err = %v, want ErrHostNotAllowed", err)
			}
		})
	}
	if hits != 0 {
		t.Errorf("the server was contacted %d times", hits)
	}
}

// A redirect from an allowed host to a forbidden one must not be followed.
func TestRedirectToDisallowedHostIsRefused(t *testing.T) {
	var forbiddenHits int
	forbidden := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forbiddenHits++ }))
	defer forbidden.Close()
	// "localhost" resolves to 127.0.0.1 but is a different name than the
	// allowlisted IP literal, so the check must reject it.
	target := strings.Replace(forbidden.URL, "127.0.0.1", "localhost", 1)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := NewClient(allow(t, "127.0.0.1"), time.Second).Get(redirector.URL)
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
	if forbiddenHits != 0 {
		t.Errorf("the forbidden host was contacted %d times", forbiddenHits)
	}
}

func TestRedirectLoopIsStopped(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound)
	}))
	defer srv.Close()

	_, err := NewClient(allow(t, "127.0.0.1"), time.Second).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("err = %v, want a redirect-limit error", err)
	}
}

func TestEnvironmentProxyIsIgnored(t *testing.T) {
	// If the proxy variable were honored, the dial would target the proxy
	// (an allowed address) and the real destination would escape the check.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	t.Setenv("HTTP_PROXY", srv.URL)
	t.Setenv("HTTPS_PROXY", srv.URL)

	_, err := NewClient(allow(t, "127.0.0.1"), time.Second).Get("http://forbidden.test/")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("err = %v, want ErrHostNotAllowed", err)
	}
}
