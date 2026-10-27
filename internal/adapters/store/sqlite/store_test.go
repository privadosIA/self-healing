package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

// fakeClock advances only when told to.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func sampleEvent(delivery string) model.WebhookEvent {
	return model.WebhookEvent{
		RepoID:         42,
		RepoFullName:   "acme/widget",
		CloneURL:       "https://gitea.example.com/acme/widget.git",
		DefaultBranch:  "main",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		Branch:         "main",
		PipelineStatus: "failure",
		JobName:        "quality",
		RunID:          2001,
		DeliveryID:     delivery,
	}
}

func openTemp(t *testing.T, clock ports.Clock) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestEnqueueAndGet(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)

	created, err := s.Enqueue(ctx, sampleEvent("d1"))
	if err != nil || !created {
		t.Fatalf("Enqueue = %v, %v", created, err)
	}
	got, err := s.Get(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Event != sampleEvent("d1") {
		t.Errorf("event not round-tripped:\n%+v", got.Event)
	}
	if got.Status != model.JobQueued || got.RuleID != "" || got.PRURL != "" || got.Error != "" || got.TokensIn != 0 || got.TokensOut != 0 {
		t.Errorf("unexpected defaults: %+v", got)
	}
	if got.ID == 0 || !got.CreatedAt.Equal(clock.t) || !got.UpdatedAt.Equal(clock.t) {
		t.Errorf("id/timestamps wrong: %+v", got)
	}
}

func TestGetUnknown(t *testing.T) {
	s, _ := openTemp(t, newClock())
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		result model.JobResult
	}{
		{"pr opened", model.JobResult{Status: model.JobPROpened, RuleID: "go:S1234", PRURL: "https://gitea.example.com/acme/widget/pulls/7", TokensIn: 812, TokensOut: 96}},
		{"skipped", model.JobResult{Status: model.JobSkipped, RuleID: "go:S1234", Error: "low confidence"}},
		{"failed", model.JobResult{Status: model.JobFailed, Error: "llm unreachable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			s, _ := openTemp(t, clock)
			if _, err := s.Enqueue(ctx, sampleEvent("d1")); err != nil {
				t.Fatal(err)
			}

			clock.advance(time.Second)
			if err := s.Start(ctx, "d1"); err != nil {
				t.Fatal(err)
			}
			running, _ := s.Get(ctx, "d1")
			if running.Status != model.JobRunning || !running.UpdatedAt.Equal(clock.t) {
				t.Errorf("after Start: %+v", running)
			}

			clock.advance(time.Second)
			if err := s.Finish(ctx, "d1", tt.result); err != nil {
				t.Fatal(err)
			}
			done, _ := s.Get(ctx, "d1")
			if done.Status != tt.result.Status || done.RuleID != tt.result.RuleID || done.PRURL != tt.result.PRURL ||
				done.TokensIn != tt.result.TokensIn || done.TokensOut != tt.result.TokensOut || done.Error != tt.result.Error {
				t.Errorf("result not stored: %+v", done)
			}
			if !done.UpdatedAt.Equal(clock.t) || !done.CreatedAt.Equal(newClock().t) {
				t.Errorf("timestamps wrong: created %v updated %v", done.CreatedAt, done.UpdatedAt)
			}
		})
	}
}

func TestInvalidTransitions(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, newClock())
	if _, err := s.Enqueue(ctx, sampleEvent("d1")); err != nil {
		t.Fatal(err)
	}
	finished := model.JobResult{Status: model.JobSkipped}

	// queued: cannot finish before starting.
	if err := s.Finish(ctx, "d1", finished); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Errorf("Finish from queued: %v", err)
	}
	if err := s.Start(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	// running: cannot start again, nor finish with a non-terminal state.
	if err := s.Start(ctx, "d1"); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Errorf("Start from running: %v", err)
	}
	for _, st := range []model.JobStatus{model.JobQueued, model.JobRunning, "bogus"} {
		if err := s.Finish(ctx, "d1", model.JobResult{Status: st}); !errors.Is(err, ports.ErrInvalidTransition) {
			t.Errorf("Finish with %q: %v", st, err)
		}
	}
	if err := s.Finish(ctx, "d1", finished); err != nil {
		t.Fatal(err)
	}
	// terminal: nothing moves any more.
	if err := s.Start(ctx, "d1"); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Errorf("Start from terminal: %v", err)
	}
	if err := s.Finish(ctx, "d1", model.JobResult{Status: model.JobFailed}); !errors.Is(err, ports.ErrInvalidTransition) {
		t.Errorf("Finish from terminal: %v", err)
	}
	// the rejected calls left the job untouched.
	if got, _ := s.Get(ctx, "d1"); got.Status != model.JobSkipped {
		t.Errorf("status = %s, want skipped", got.Status)
	}
	// unknown delivery.
	if err := s.Start(ctx, "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Start unknown: %v", err)
	}
	if err := s.Finish(ctx, "nope", finished); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Finish unknown: %v", err)
	}
}

func TestErrorTextIsTruncatedOnACharacterBoundary(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, newClock())
	_, _ = s.Enqueue(ctx, sampleEvent("d1"))
	_ = s.Start(ctx, "d1")
	long := strings.Repeat("é", maxErrorLen) // 2 bytes each, so the cut falls mid-character
	if err := s.Finish(ctx, "d1", model.JobResult{Status: model.JobFailed, Error: long}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "d1")
	if len(got.Error) > maxErrorLen || strings.ContainsRune(got.Error, '�') || got.Error == "" {
		t.Errorf("stored error has %d bytes", len(got.Error))
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, path := openTemp(t, clock)
	_, _ = s.Enqueue(ctx, sampleEvent("d1"))
	_ = s.Start(ctx, "d1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.Get(ctx, "d1")
	if err != nil || got.Status != model.JobRunning {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
}

func TestMigrationIsIdempotentAndVersioned(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, path := openTemp(t, clock)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // reopening must not re-run the migration
		s, err := Open(ctx, path, clock)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		var v int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
			t.Errorf("user_version = %d, %v", v, err)
		}
		_ = s.Close()
	}
}

func TestRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if _, err := Open(ctx, path, newClock()); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("err = %v, want a newer-schema error", err)
	}
}

func TestSchemaRejectsUnknownStatus(t *testing.T) {
	s, _ := openTemp(t, newClock())
	_, err := s.db.Exec(`INSERT INTO jobs (delivery_id, repo_id, commit_sha, status, event, created_at, updated_at)
		VALUES ('x', 1, 'sha', 'bogus', '{}', 't', 't')`)
	if err == nil {
		t.Error("the CHECK constraint should reject an unknown status")
	}
}

func TestOpenFailsOnUnwritablePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "x.db")
	if _, err := Open(context.Background(), missing, newClock()); err == nil {
		t.Error("expected an error for a path in a missing directory")
	}
}
