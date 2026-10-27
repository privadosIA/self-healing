package listener

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/core/model"
)

// fakeStore is an in-memory JobStore that deduplicates on DeliveryID.
type fakeStore struct {
	seen   map[string]bool
	events []model.WebhookEvent
	err    error
}

func newFakeStore() *fakeStore { return &fakeStore{seen: map[string]bool{}} }

func (s *fakeStore) Enqueue(_ context.Context, ev model.WebhookEvent) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.seen[ev.DeliveryID] {
		return false, nil
	}
	s.seen[ev.DeliveryID] = true
	s.events = append(s.events, ev)
	return true, nil
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "gitea", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type request struct {
	method   string
	event    string
	delivery string
	body     string
}

func post(h http.Handler, r request) *httptest.ResponseRecorder {
	if r.method == "" {
		r.method = http.MethodPost
	}
	req := httptest.NewRequest(r.method, "/webhook", strings.NewReader(r.body))
	if r.event != "" {
		req.Header.Set(eventHeader, r.event)
	}
	if r.delivery != "" {
		req.Header.Set(deliveryHeader, r.delivery)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookHandler(t *testing.T) {
	job := fixture(t, "workflow_job_failure.json")
	status := fixture(t, "status_failure.json")
	sub := func(body, old, new string) string { return strings.Replace(body, old, new, 1) }

	tests := []struct {
		name       string
		req        request
		storeErr   error
		wantStatus int
		wantEvents int
	}{
		{"failed workflow_job is queued", request{event: "workflow_job", delivery: "d1", body: job}, nil, http.StatusAccepted, 1},
		{"failed status is queued", request{event: "status", delivery: "d1", body: status}, nil, http.StatusAccepted, 1},
		{"status error state is queued", request{event: "status", delivery: "d1", body: sub(status, `"failure"`, `"error"`)}, nil, http.StatusAccepted, 1},
		{"successful workflow_job is ignored", request{event: "workflow_job", delivery: "d1", body: sub(job, `"conclusion": "failure"`, `"conclusion": "success"`)}, nil, http.StatusNoContent, 0},
		{"cancelled workflow_job is ignored", request{event: "workflow_job", delivery: "d1", body: sub(job, `"conclusion": "failure"`, `"conclusion": "cancelled"`)}, nil, http.StatusNoContent, 0},
		{"pending status is ignored", request{event: "status", delivery: "d1", body: sub(status, `"failure"`, `"pending"`)}, nil, http.StatusNoContent, 0},
		{"unsupported event is ignored", request{event: "push", delivery: "d1", body: job}, nil, http.StatusNoContent, 0},
		{"auto-fix branch is ignored", request{event: "workflow_job", delivery: "d1", body: sub(job, `"head_branch": "main"`, `"head_branch": "auto-fix/go-s1234/0123456"`)}, nil, http.StatusNoContent, 0},
		{"auto-fix branch given as a ref is ignored", request{event: "workflow_job", delivery: "d1", body: sub(job, `"head_branch": "main"`, `"head_branch": "refs/heads/auto-fix/x/0123456"`)}, nil, http.StatusNoContent, 0},
		{"branch merely containing auto-fix is queued", request{event: "workflow_job", delivery: "d1", body: sub(job, `"head_branch": "main"`, `"head_branch": "feature/auto-fix/x"`)}, nil, http.StatusAccepted, 1},
		{"missing commit sha", request{event: "workflow_job", delivery: "d1", body: sub(job, `"head_sha": "0123456789abcdef0123456789abcdef01234567"`, `"head_sha": ""`)}, nil, http.StatusBadRequest, 0},
		{"missing repository", request{event: "workflow_job", delivery: "d1", body: sub(job, `"full_name": "acme/widget"`, `"full_name": ""`)}, nil, http.StatusBadRequest, 0},
		{"status without branches", request{event: "status", delivery: "d1", body: sub(status, `[{ "name": "main" }]`, `[]`)}, nil, http.StatusBadRequest, 0},
		{"invalid JSON", request{event: "workflow_job", delivery: "d1", body: "{not json"}, nil, http.StatusBadRequest, 0},
		{"missing delivery header", request{event: "workflow_job", body: job}, nil, http.StatusBadRequest, 0},
		{"wrong method", request{method: http.MethodGet, event: "workflow_job", delivery: "d1"}, nil, http.StatusMethodNotAllowed, 0},
		{"store failure", request{event: "workflow_job", delivery: "d1", body: job}, errors.New("disk full"), http.StatusInternalServerError, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.err = tt.storeErr
			rec := post(NewWebhookHandler(store, discardLogger(), nil), tt.req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if len(store.events) != tt.wantEvents {
				t.Errorf("enqueued %d events, want %d", len(store.events), tt.wantEvents)
			}
		})
	}
}

func TestWebhookNormalization(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	base := model.WebhookEvent{
		RepoID:        42,
		RepoFullName:  "acme/widget",
		CloneURL:      "https://gitea.example.com/acme/widget.git",
		DefaultBranch: "main",
		CommitSHA:     sha,
		Branch:        "main",
		DeliveryID:    "delivery-1",
	}
	tests := []struct {
		name  string
		event string
		body  string
		want  model.WebhookEvent
	}{
		{
			"workflow_job", "workflow_job", fixture(t, "workflow_job_failure.json"),
			func() model.WebhookEvent {
				e := base
				e.PipelineStatus, e.JobName, e.RunID = "failure", "quality", 2001
				return e
			}(),
		},
		{
			"status", "status", fixture(t, "status_failure.json"),
			func() model.WebhookEvent {
				e := base
				e.PipelineStatus, e.JobName = "failure", "ci/quality"
				return e
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			rec := post(NewWebhookHandler(store, discardLogger(), nil), request{event: tt.event, delivery: "delivery-1", body: tt.body})
			if rec.Code != http.StatusAccepted || len(store.events) != 1 {
				t.Fatalf("status %d, events %d", rec.Code, len(store.events))
			}
			if got := store.events[0]; got != tt.want {
				t.Errorf("event =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestWebhookDuplicateDelivery(t *testing.T) {
	store := newFakeStore()
	h := NewWebhookHandler(store, discardLogger(), nil)
	req := request{event: "workflow_job", delivery: "same", body: fixture(t, "workflow_job_failure.json")}

	if rec := post(h, req); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d", rec.Code)
	}
	if rec := post(h, req); rec.Code != http.StatusOK {
		t.Errorf("repeated delivery status = %d, want 200", rec.Code)
	}
	if len(store.events) != 1 {
		t.Errorf("enqueued %d jobs, want 1", len(store.events))
	}

	req.delivery = "other"
	if rec := post(h, req); rec.Code != http.StatusAccepted || len(store.events) != 2 {
		t.Errorf("a new delivery must be queued: status %d, jobs %d", rec.Code, len(store.events))
	}
}

// TestWebhookBehindSignature checks the two pieces together: an unsigned or
// forged request never reaches the store.
func TestWebhookBehindSignature(t *testing.T) {
	body := fixture(t, "workflow_job_failure.json")
	store := newFakeStore()
	mw, err := RequireSignature([]byte(testSecret), 1<<20, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := mw(NewWebhookHandler(store, discardLogger(), nil))

	send := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set(eventHeader, "workflow_job")
		req.Header.Set(deliveryHeader, "d1")
		if sig != "" {
			req.Header.Set(SignatureHeader, sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send(""); code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", code)
	}
	if code := send(sign("other-secret", body)); code != http.StatusUnauthorized || len(store.events) != 0 {
		t.Errorf("forged = %d, events %d", code, len(store.events))
	}
	if code := send(sign(testSecret, body)); code != http.StatusAccepted || len(store.events) != 1 {
		t.Errorf("signed = %d, events %d", code, len(store.events))
	}
}
