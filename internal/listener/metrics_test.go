package listener

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	tests := []struct {
		method     string
		wantStatus int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodHead, http.StatusOK},
		{http.MethodPost, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			HealthHandler().ServeHTTP(rec, httptest.NewRequest(tt.method, "/healthz", nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
	rec := httptest.NewRecorder()
	HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestMetricsExposeCounters(t *testing.T) {
	m := NewMetrics()

	// Every outcome and the runtime collectors are present before any event.
	out := scrape(t, m)
	for _, want := range []string{
		`healerd_webhooks_total{result="queued"} 0`,
		`healerd_webhooks_total{result="duplicate"} 0`,
		`healerd_webhooks_total{result="ignored"} 0`,
		`healerd_webhooks_total{result="malformed"} 0`,
		`healerd_webhooks_total{result="error"} 0`,
		"go_goroutines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}

	// Drive the real handler and check each outcome is counted.
	body := fixture(t, "workflow_job_failure.json")
	h := NewWebhookHandler(newFakeStore(), discardLogger(), m)
	post(h, request{event: "workflow_job", delivery: "d1", body: body})        // queued
	post(h, request{event: "workflow_job", delivery: "d1", body: body})        // duplicate
	post(h, request{event: "push", delivery: "d2", body: body})                // ignored
	post(h, request{event: "workflow_job", delivery: "d3", body: "{not json"}) // malformed
	post(h, request{event: "workflow_job", body: body})                        // malformed (no delivery)
	failing := newFakeStore()
	failing.err = io.ErrUnexpectedEOF
	post(NewWebhookHandler(failing, discardLogger(), m), request{event: "workflow_job", delivery: "d4", body: body}) // error

	out = scrape(t, m)
	for _, want := range []string{
		`healerd_webhooks_total{result="queued"} 1`,
		`healerd_webhooks_total{result="duplicate"} 1`,
		`healerd_webhooks_total{result="ignored"} 1`,
		`healerd_webhooks_total{result="malformed"} 2`,
		`healerd_webhooks_total{result="error"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics is missing %q after traffic", want)
		}
	}
}

func TestMetricsRegistriesAreIndependent(t *testing.T) {
	a, b := NewMetrics(), NewMetrics() // would panic on a shared global registry
	a.webhook(resultQueued)
	if strings.Contains(scrape(t, b), `result="queued"} 1`) {
		t.Error("registries share state")
	}
}
