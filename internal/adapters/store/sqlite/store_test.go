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
	defer func() { _ = s2.Close() }()
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

func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)

	first := sampleEvent("d1")
	if created, err := s.Enqueue(ctx, first); err != nil || !created {
		t.Fatalf("first Enqueue = %v, %v", created, err)
	}
	before, _ := s.Get(ctx, "d1")

	// Same delivery later and with different content: nothing may change.
	clock.advance(time.Hour)
	second := sampleEvent("d1")
	second.CommitSHA, second.Branch = "fedcba9876543210fedcba9876543210fedcba98", "other"
	created, err := s.Enqueue(ctx, second)
	if err != nil || created {
		t.Fatalf("duplicate Enqueue = %v, %v; want false, nil", created, err)
	}
	after, _ := s.Get(ctx, "d1")
	if after != before {
		t.Errorf("duplicate modified the original job:\nbefore %+v\nafter  %+v", before, after)
	}

	// A duplicate of a job that already progressed must not reset it.
	_ = s.Start(ctx, "d1")
	if created, _ := s.Enqueue(ctx, first); created {
		t.Error("duplicate of a running job was created")
	}
	if got, _ := s.Get(ctx, "d1"); got.Status != model.JobRunning {
		t.Errorf("status = %s, want running", got.Status)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; want 1", n, err)
	}
}

func TestConcurrentDuplicatesCreateOneJob(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, newClock())

	const workers = 20
	results := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		go func() {
			created, err := s.Enqueue(ctx, sampleEvent("same"))
			if err != nil {
				t.Error(err)
			}
			results <- created
		}()
	}
	created := 0
	for i := 0; i < workers; i++ {
		if <-results {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d goroutines created the job, want exactly 1", created)
	}
}

func TestFailInterrupted(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)

	// queued, running and finished jobs of different deliveries.
	for _, d := range []string{"queued", "running-1", "running-2", "done"} {
		if _, err := s.Enqueue(ctx, sampleEvent(d)); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"running-1", "running-2", "done"} {
		if err := s.Start(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	prURL := "https://gitea.example.com/acme/widget/pulls/7"
	if err := s.Finish(ctx, "done", model.JobResult{Status: model.JobPROpened, PRURL: prURL}); err != nil {
		t.Fatal(err)
	}

	clock.advance(time.Minute)
	n, err := s.FailInterrupted(ctx)
	if err != nil || n != 2 {
		t.Fatalf("FailInterrupted = %d, %v; want 2, nil", n, err)
	}

	for _, d := range []string{"running-1", "running-2"} {
		got, _ := s.Get(ctx, d)
		if got.Status != model.JobFailed || got.Error != interruptedMessage || !got.UpdatedAt.Equal(clock.t) {
			t.Errorf("%s not recovered: %+v", d, got)
		}
	}
	if got, _ := s.Get(ctx, "queued"); got.Status != model.JobQueued || got.Error != "" {
		t.Errorf("queued job was touched: %+v", got)
	}
	if got, _ := s.Get(ctx, "done"); got.Status != model.JobPROpened || got.PRURL != prURL {
		t.Errorf("finished job was touched: %+v", got)
	}

	// Idempotent: nothing is left to recover.
	if n, err := s.FailInterrupted(ctx); err != nil || n != 0 {
		t.Errorf("second FailInterrupted = %d, %v; want 0, nil", n, err)
	}
}

func TestFailInterruptedOnEmptyStore(t *testing.T) {
	s, _ := openTemp(t, newClock())
	if n, err := s.FailInterrupted(context.Background()); err != nil || n != 0 {
		t.Errorf("FailInterrupted = %d, %v; want 0, nil", n, err)
	}
}

// TestRecoveryAfterCrash simulates a crash: the process dies with a job running
// and another queued, and the next start recovers them.
func TestRecoveryAfterCrash(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, path := openTemp(t, clock)
	_, _ = s.Enqueue(ctx, sampleEvent("was-running"))
	_, _ = s.Enqueue(ctx, sampleEvent("was-queued"))
	_ = s.Start(ctx, "was-running")
	if err := s.Close(); err != nil { // no Finish: the "crash"
		t.Fatal(err)
	}

	restarted, err := Open(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if n, err := restarted.FailInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("FailInterrupted = %d, %v; want 1, nil", n, err)
	}
	if got, _ := restarted.Get(ctx, "was-running"); got.Status != model.JobFailed {
		t.Errorf("was-running = %s, want failed", got.Status)
	}
	if got, _ := restarted.Get(ctx, "was-queued"); got.Status != model.JobQueued {
		t.Errorf("was-queued = %s, want queued", got.Status)
	}
	// The recovered job can be redelivered by Gitea but must not be re-created.
	if created, _ := restarted.Enqueue(ctx, sampleEvent("was-running")); created {
		t.Error("a recovered delivery was enqueued again")
	}
}

func TestReportRoundTripAndReplace(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)

	if _, err := s.GetReport(ctx, 42, "abc1234"); !errors.Is(err, ports.ErrReportNotFound) {
		t.Fatalf("Get on empty store: %v", err)
	}
	if err := s.PutReport(ctx, 42, "abc1234", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReport(ctx, 42, "abc1234")
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// A newer upload for the same key replaces the report, keeping one row.
	clock.advance(time.Minute)
	if err := s.PutReport(ctx, 42, "abc1234", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetReport(ctx, 42, "abc1234"); string(got) != `{"v":2}` {
		t.Errorf("report not replaced: %q", got)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reports").Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; want 1", n, err)
	}

	// Keys are independent: another repo or commit does not see it.
	for _, k := range []struct {
		repo int64
		sha  string
	}{{43, "abc1234"}, {42, "def5678"}} {
		if _, err := s.GetReport(ctx, k.repo, k.sha); !errors.Is(err, ports.ErrReportNotFound) {
			t.Errorf("Get(%d, %s): %v", k.repo, k.sha, err)
		}
	}
}

func TestReportsAreBounded(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)
	s.maxReports = 3

	for i := 1; i <= 5; i++ {
		clock.advance(time.Second)
		if err := s.PutReport(ctx, int64(i), "abc1234", []byte("r")); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reports").Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows = %d, %v; want 3", n, err)
	}
	// The two oldest went; the three newest stayed.
	for i, wantFound := range map[int64]bool{1: false, 2: false, 3: true, 4: true, 5: true} {
		_, err := s.GetReport(ctx, i, "abc1234")
		if found := err == nil; found != wantFound {
			t.Errorf("repo %d found = %v, want %v", i, found, wantFound)
		}
	}
}

func TestReportBinaryContentIsPreserved(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, newClock())
	data := []byte{0x00, 0xff, 0x10, '\n', '"', 0x00}
	if err := s.PutReport(ctx, 1, "abc1234", data); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetReport(ctx, 1, "abc1234"); string(got) != string(data) {
		t.Errorf("content changed: %v", got)
	}
}

func TestListQueued(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, _ := openTemp(t, clock)

	if got, err := s.ListQueued(ctx, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	for _, d := range []string{"a", "b", "c", "d", "e"} {
		clock.advance(time.Second)
		if _, err := s.Enqueue(ctx, sampleEvent(d)); err != nil {
			t.Fatal(err)
		}
	}
	// b is running, c finished: neither is queued any more.
	_ = s.Start(ctx, "b")
	_ = s.Start(ctx, "c")
	_ = s.Finish(ctx, "c", model.JobResult{Status: model.JobSkipped})

	got, err := s.ListQueued(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range got {
		ids = append(ids, j.Event.DeliveryID)
		if j.Status != model.JobQueued || j.Event.RepoFullName != "acme/widget" || j.Event.CloneURL == "" {
			t.Errorf("job not fully decoded: %+v", j)
		}
	}
	if strings.Join(ids, ",") != "a,d,e" {
		t.Errorf("queued = %v, want oldest first and only queued ones: a,d,e", ids)
	}

	if got, _ := s.ListQueued(ctx, 2); len(got) != 2 || got[0].Event.DeliveryID != "a" || got[1].Event.DeliveryID != "d" {
		t.Errorf("limit not honored: %+v", got)
	}
	for _, n := range []int{0, -1} {
		if got, err := s.ListQueued(ctx, n); err != nil || got != nil {
			t.Errorf("limit %d: %v, %v", n, got, err)
		}
	}
}
