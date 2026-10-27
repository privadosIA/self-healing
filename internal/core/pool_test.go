package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const wait = 5 * time.Second // generous: only a failing test ever waits this long

func job(repo int64, n int) model.WebhookEvent {
	return model.WebhookEvent{RepoID: repo, DeliveryID: fmt.Sprintf("r%d-j%d", repo, n), RepoFullName: fmt.Sprintf("acme/r%d", repo)}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// runner is a JobRunner that records what happens to it.
type runner struct {
	mu      sync.Mutex
	order   []string        // delivery ids in the order they started
	perRepo map[int64][]int // sequence of job numbers per repository, in start order
	active  map[int64]int
	maxRepo map[int64]int // highest concurrency seen per repository
	cur     int
	maxAll  int
	done    map[string]bool

	// behavior
	onStart func(ctx context.Context, ev model.WebhookEvent) // may block
	result  func(ev model.WebhookEvent) (model.JobResult, error)
}

func newRunner() *runner {
	return &runner{perRepo: map[int64][]int{}, active: map[int64]int{}, maxRepo: map[int64]int{}, done: map[string]bool{}}
}

func (r *runner) Heal(ctx context.Context, ev model.WebhookEvent) (model.JobResult, error) {
	r.mu.Lock()
	r.order = append(r.order, ev.DeliveryID)
	var n int
	fmt.Sscanf(ev.DeliveryID, "r%d-j%d", new(int64), &n)
	r.perRepo[ev.RepoID] = append(r.perRepo[ev.RepoID], n)
	r.active[ev.RepoID]++
	r.maxRepo[ev.RepoID] = max(r.maxRepo[ev.RepoID], r.active[ev.RepoID])
	r.cur++
	r.maxAll = max(r.maxAll, r.cur)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.active[ev.RepoID]--
		r.cur--
		r.done[ev.DeliveryID] = true
		r.mu.Unlock()
	}()
	if r.onStart != nil {
		r.onStart(ctx, ev)
	}
	if r.result != nil {
		return r.result(ev)
	}
	return model.JobResult{Status: model.JobPROpened, RuleID: ev.DeliveryID}, nil
}

func (r *runner) finishedCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.done) }
func (r *runner) startedCount() int  { r.mu.Lock(); defer r.mu.Unlock(); return len(r.order) }
func (r *runner) hasStarted(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.order {
		if o == id {
			return true
		}
	}
	return false
}
func (r *runner) hasFinished(id string) bool { r.mu.Lock(); defer r.mu.Unlock(); return r.done[id] }

// start runs the pool and returns a function that stops it and waits for Run to return.
func start(t *testing.T, cfg PoolConfig) (*Pool, func()) {
	t.Helper()
	p, err := NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { p.Run(ctx); close(returned) }()
	stop := func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(wait):
			t.Fatal("Run did not return")
		}
	}
	t.Cleanup(func() { cancel() })
	return p, stop
}

func TestJobsOfOneRepositoryNeverOverlapAndKeepTheirOrder(t *testing.T) {
	r := newRunner()
	r.onStart = func(context.Context, model.WebhookEvent) { time.Sleep(time.Millisecond) }
	p, stop := start(t, PoolConfig{Runner: r, Workers: 6})
	defer stop()

	const repos, perRepo = 4, 15
	for n := 1; n <= perRepo; n++ { // interleave repositories
		for repo := int64(1); repo <= repos; repo++ {
			if err := p.Submit(job(repo, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitFor(t, "all jobs", func() bool { return r.finishedCount() == repos*perRepo })

	r.mu.Lock()
	defer r.mu.Unlock()
	for repo := int64(1); repo <= repos; repo++ {
		if r.maxRepo[repo] != 1 {
			t.Errorf("repository %d ran %d jobs at once", repo, r.maxRepo[repo])
		}
		seq := r.perRepo[repo]
		for i, n := range seq {
			if n != i+1 {
				t.Errorf("repository %d ran jobs out of order: %v", repo, seq)
				break
			}
		}
		if len(seq) != perRepo {
			t.Errorf("repository %d ran %d jobs, want %d", repo, len(seq), perRepo)
		}
	}
}

func TestDifferentRepositoriesRunInParallel(t *testing.T) {
	const repos = 4
	var arrived sync.WaitGroup
	arrived.Add(repos)
	release := make(chan struct{})
	r := newRunner()
	r.onStart = func(context.Context, model.WebhookEvent) {
		arrived.Done()
		<-release
	}
	p, stop := start(t, PoolConfig{Runner: r, Workers: repos})
	defer stop()
	for repo := int64(1); repo <= repos; repo++ {
		_ = p.Submit(job(repo, 1))
	}

	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	select {
	case <-all: // every repository's job is running at the same time
	case <-time.After(wait):
		t.Fatal("jobs of different repositories did not run in parallel")
	}
	close(release)
	waitFor(t, "completion", func() bool { return r.finishedCount() == repos })
}

func TestNoMoreWorkersThanConfigured(t *testing.T) {
	const workers, repos = 3, 10
	var running atomic.Int32
	release := make(chan struct{})
	r := newRunner()
	r.onStart = func(context.Context, model.WebhookEvent) { running.Add(1); <-release; running.Add(-1) }
	p, stop := start(t, PoolConfig{Runner: r, Workers: workers})
	defer stop()
	for repo := int64(1); repo <= repos; repo++ {
		_ = p.Submit(job(repo, 1))
	}
	waitFor(t, "the workers to fill up", func() bool { return running.Load() == workers })
	time.Sleep(20 * time.Millisecond) // give a wrongly extra worker the chance to appear
	if got := running.Load(); got != workers {
		t.Errorf("%d jobs running with %d workers", got, workers)
	}
	close(release)
	waitFor(t, "completion", func() bool { return r.finishedCount() == repos })
	if r.maxAll > workers {
		t.Errorf("peak concurrency %d exceeds %d workers", r.maxAll, workers)
	}
}

// A repository with several jobs must not hold a worker while it waits its turn.
func TestABusyRepositoryDoesNotStarveOthers(t *testing.T) {
	gate := make(chan struct{})
	r := newRunner()
	r.onStart = func(_ context.Context, ev model.WebhookEvent) {
		if ev.DeliveryID == "r1-j1" {
			<-gate
		}
	}
	p, stop := start(t, PoolConfig{Runner: r, Workers: 2})
	defer stop()

	for n := 1; n <= 3; n++ {
		_ = p.Submit(job(1, n)) // r1-j1 blocks; j2 and j3 must wait behind it
	}
	_ = p.Submit(job(2, 1))

	waitFor(t, "the other repository's job to finish while r1-j1 is blocked", func() bool { return r.hasFinished("r2-j1") })
	if r.hasStarted("r1-j2") {
		t.Error("r1-j2 started while r1-j1 was still running")
	}
	close(gate)
	waitFor(t, "repository 1 to finish", func() bool { return r.hasFinished("r1-j3") })
}

func TestDuplicateDeliveriesAreIgnoredWhilePending(t *testing.T) {
	gate := make(chan struct{})
	r := newRunner()
	r.onStart = func(context.Context, model.WebhookEvent) { <-gate }
	p, stop := start(t, PoolConfig{Runner: r, Workers: 1})
	defer stop()

	_ = p.Submit(job(1, 1))
	waitFor(t, "the job to start", func() bool { return r.startedCount() == 1 })
	_ = p.Submit(job(1, 1)) // running
	_ = p.Submit(job(1, 2))
	_ = p.Submit(job(1, 2)) // waiting
	close(gate)
	waitFor(t, "completion", func() bool { return r.finishedCount() == 2 })
	if r.startedCount() != 2 {
		t.Errorf("%d jobs started for 2 distinct deliveries", r.startedCount())
	}
	// Once finished, the same delivery may be submitted again (the store decides).
	_ = p.Submit(job(1, 1))
	waitFor(t, "the resubmitted job", func() bool { return r.startedCount() == 3 })
}

func TestQueueFull(t *testing.T) {
	r := newRunner()
	p, err := NewPool(PoolConfig{Runner: r, Workers: 1, MaxPending: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Run has not started, so everything stays pending.
	for n := 1; n <= 3; n++ {
		if err := p.Submit(job(int64(n), 1)); err != nil {
			t.Fatalf("submit %d: %v", n, err)
		}
	}
	if err := p.Submit(job(9, 1)); !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}
	if err := p.Submit(job(1, 1)); err != nil {
		t.Errorf("a duplicate of a pending job should still be accepted silently: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitFor(t, "the queue to drain", func() bool { return r.finishedCount() == 3 })
	if err := p.Submit(job(9, 1)); err != nil {
		t.Errorf("after draining: %v", err)
	}
	waitFor(t, "the late job", func() bool { return r.finishedCount() == 4 })
	cancel()
	<-done
}

func TestGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	r := newRunner()
	r.onStart = func(_ context.Context, ev model.WebhookEvent) {
		if ev.DeliveryID == "r1-j1" {
			close(started)
			<-finish // ignores cancellation: a job that is allowed to complete
		}
	}
	p, err := NewPool(PoolConfig{Runner: r, Workers: 1, Grace: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { p.Run(ctx); close(returned) }()

	_ = p.Submit(job(1, 1))
	_ = p.Submit(job(1, 2)) // waiting behind j1
	_ = p.Submit(job(2, 1)) // ready, but the only worker is busy
	<-started

	cancel()
	select {
	case <-returned:
		t.Fatal("Run returned while a job was still running inside the grace period")
	case <-time.After(50 * time.Millisecond):
	}
	waitFor(t, "intake to close", func() bool { return errors.Is(p.Submit(job(3, 1)), ErrPoolClosed) })

	close(finish)
	select {
	case <-returned:
	case <-time.After(wait):
		t.Fatal("Run did not return after the running job finished")
	}
	if !r.hasFinished("r1-j1") {
		t.Error("the running job was not allowed to finish")
	}
	if r.hasStarted("r1-j2") || r.hasStarted("r2-j1") {
		t.Errorf("jobs started after shutdown began: %v", r.order)
	}
}

func TestShutdownCancelsJobsAfterTheGracePeriod(t *testing.T) {
	started := make(chan struct{})
	var sawCancel atomic.Bool
	r := newRunner()
	r.onStart = func(ctx context.Context, _ model.WebhookEvent) {
		close(started)
		<-ctx.Done()
		sawCancel.Store(true)
	}
	p, err := NewPool(PoolConfig{Runner: r, Workers: 1, Grace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { p.Run(ctx); close(returned) }()
	_ = p.Submit(job(1, 1))
	<-started

	begun := time.Now()
	cancel()
	select {
	case <-returned:
	case <-time.After(wait):
		t.Fatal("Run did not return after the grace period")
	}
	if elapsed := time.Since(begun); elapsed < 40*time.Millisecond {
		t.Errorf("jobs were cancelled after %v, before the grace period", elapsed)
	}
	if !sawCancel.Load() {
		t.Error("the job's context was never cancelled")
	}
}

func TestRunReturnsPromptlyWhenIdle(t *testing.T) {
	_, stop := start(t, PoolConfig{Runner: newRunner(), Workers: 4, Grace: time.Minute})
	stop()
}

func TestAPanickingJobDoesNotBreakThePool(t *testing.T) {
	var results []model.JobResult
	var mu sync.Mutex
	r := newRunner()
	r.onStart = func(_ context.Context, ev model.WebhookEvent) {
		if ev.DeliveryID == "r1-j1" {
			panic("boom")
		}
	}
	p, stop := start(t, PoolConfig{Runner: r, Workers: 1, OnResult: func(res model.JobResult) {
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	}})
	defer stop()

	_ = p.Submit(job(1, 1)) // panics
	_ = p.Submit(job(1, 2)) // same repository: must still run
	_ = p.Submit(job(2, 1))
	waitFor(t, "the jobs after the panic", func() bool { return r.hasFinished("r1-j2") && r.hasFinished("r2-j1") })
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 {
		t.Errorf("OnResult called %d times, want 2 (the panicking job reports nothing)", len(results))
	}
}

func TestResultsAndErrors(t *testing.T) {
	var got []model.JobResult
	var mu sync.Mutex
	r := newRunner()
	r.result = func(ev model.WebhookEvent) (model.JobResult, error) {
		switch ev.DeliveryID {
		case "r1-j1":
			return model.JobResult{Status: model.JobPROpened, PRURL: "u"}, nil
		case "r2-j1":
			return model.JobResult{Status: model.JobSkipped, Error: "why"}, nil
		case "r3-j1":
			return model.JobResult{}, fmt.Errorf("taken: %w", ports.ErrInvalidTransition) // someone else's job
		default:
			return model.JobResult{Status: model.JobFailed}, errors.New("store down") // recorded in memory only
		}
	}
	p, stop := start(t, PoolConfig{Runner: r, Workers: 2, OnResult: func(res model.JobResult) {
		mu.Lock()
		got = append(got, res)
		mu.Unlock()
	}})
	defer stop()
	for repo := int64(1); repo <= 4; repo++ {
		_ = p.Submit(job(repo, 1))
	}
	waitFor(t, "all jobs", func() bool { return r.finishedCount() == 4 })
	waitFor(t, "the callbacks", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	mu.Lock()
	defer mu.Unlock()
	counts := map[model.JobStatus]int{}
	for _, res := range got {
		counts[res.Status]++
	}
	if counts[model.JobPROpened] != 1 || counts[model.JobSkipped] != 1 || counts[model.JobFailed] != 1 || len(got) != 3 {
		t.Errorf("results = %+v (a job that someone else already took reports nothing)", got)
	}
}

// --- the sweep ---------------------------------------------------------------

type source struct {
	mu   sync.Mutex
	jobs []model.Job
	err  error
	n    int
}

func (s *source) ListQueued(_ context.Context, limit int) ([]model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.err != nil {
		return nil, s.err
	}
	out := s.jobs
	if len(out) > limit {
		out = out[:limit]
	}
	return append([]model.Job(nil), out...), nil
}
func (s *source) set(jobs ...model.WebhookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = nil
	for _, ev := range jobs {
		s.jobs = append(s.jobs, model.Job{Event: ev, Status: model.JobQueued})
	}
}
func (s *source) calls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

func TestSweepRecoversQueuedJobsAtStartAndPeriodically(t *testing.T) {
	src := &source{}
	src.set(job(1, 1), job(2, 1)) // survived a restart
	r := newRunner()
	_, stop := start(t, PoolConfig{Runner: r, Workers: 2, Source: src, SweepInterval: 10 * time.Millisecond})
	defer stop()

	waitFor(t, "the jobs left over from before the start", func() bool { return r.hasFinished("r1-j1") && r.hasFinished("r2-j1") })
	// The same rows still being listed (the fake never marks them done) must not rerun them.
	waitFor(t, "several sweeps", func() bool { return src.calls() >= 4 })

	src.set(job(1, 1), job(2, 1), job(3, 1)) // a job that did not fit earlier shows up
	waitFor(t, "the late job", func() bool { return r.hasFinished("r3-j1") })

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.done) != 3 {
		t.Errorf("%d jobs ran, want 3", len(r.done))
	}
}

func TestSweepFailuresAreNotFatal(t *testing.T) {
	src := &source{err: errors.New("database is locked")}
	r := newRunner()
	p, stop := start(t, PoolConfig{Runner: r, Workers: 1, Source: src, SweepInterval: 5 * time.Millisecond})
	defer stop()
	waitFor(t, "failing sweeps", func() bool { return src.calls() >= 3 })

	src.mu.Lock()
	src.err = nil
	src.mu.Unlock()
	src.set(job(1, 1))
	waitFor(t, "recovery after the error", func() bool { return r.hasFinished("r1-j1") })

	if err := p.Submit(job(2, 1)); err != nil {
		t.Errorf("the pool stopped accepting jobs after a sweep error: %v", err)
	}
}

func TestSweepStopsWhenTheQueueIsFull(t *testing.T) {
	src := &source{}
	var evs []model.WebhookEvent
	for n := 1; n <= 5; n++ {
		evs = append(evs, job(int64(n), 1))
	}
	src.set(evs...)
	p, err := NewPool(PoolConfig{Runner: newRunner(), Workers: 1, MaxPending: 2, Source: src})
	if err != nil {
		t.Fatal(err)
	}
	p.Sweep(context.Background()) // no workers running: the queue fills and the sweep gives up quietly
	p.mu.Lock()
	n := len(p.known)
	p.mu.Unlock()
	if n != 2 {
		t.Errorf("%d jobs accepted, want the queue limit of 2", n)
	}
}

func TestNoSourceMeansNoSweep(t *testing.T) {
	p, _ := NewPool(PoolConfig{Runner: newRunner(), Workers: 1})
	p.Sweep(context.Background()) // must not panic
}

// --- intake -------------------------------------------------------------------

type enqueuer struct {
	created bool
	err     error
	calls   int
}

func (e *enqueuer) Enqueue(context.Context, model.WebhookEvent) (bool, error) {
	e.calls++
	return e.created, e.err
}

func TestIntake(t *testing.T) {
	t.Run("a new job is stored and submitted", func(t *testing.T) {
		r := newRunner()
		p, stop := start(t, PoolConfig{Runner: r, Workers: 1})
		defer stop()
		created, err := (&Intake{Store: &enqueuer{created: true}, Pool: p}).Enqueue(context.Background(), job(1, 1))
		if err != nil || !created {
			t.Fatalf("Enqueue = %v, %v", created, err)
		}
		waitFor(t, "the job to run", func() bool { return r.hasFinished("r1-j1") })
	})
	t.Run("a duplicate is not submitted", func(t *testing.T) {
		r := newRunner()
		p, stop := start(t, PoolConfig{Runner: r, Workers: 1})
		defer stop()
		created, err := (&Intake{Store: &enqueuer{created: false}, Pool: p}).Enqueue(context.Background(), job(1, 1))
		if err != nil || created {
			t.Fatalf("Enqueue = %v, %v", created, err)
		}
		time.Sleep(20 * time.Millisecond)
		if r.startedCount() != 0 {
			t.Error("a duplicate delivery was run")
		}
	})
	t.Run("a store error is returned and nothing is submitted", func(t *testing.T) {
		r := newRunner()
		p, stop := start(t, PoolConfig{Runner: r, Workers: 1})
		defer stop()
		_, err := (&Intake{Store: &enqueuer{err: errors.New("disk full")}, Pool: p}).Enqueue(context.Background(), job(1, 1))
		if err == nil {
			t.Fatal("expected the store error")
		}
		time.Sleep(20 * time.Millisecond)
		if r.startedCount() != 0 {
			t.Error("a job that was not stored was run")
		}
	})
	t.Run("a full or closing pool still accepts the webhook", func(t *testing.T) {
		p, _ := NewPool(PoolConfig{Runner: newRunner(), Workers: 1, MaxPending: 1})
		in := &Intake{Store: &enqueuer{created: true}, Pool: p}
		for n := 1; n <= 3; n++ { // the second and third do not fit
			if created, err := in.Enqueue(context.Background(), job(int64(n), 1)); err != nil || !created {
				t.Errorf("job %d: %v, %v; it is stored, so the sweep will get it", n, created, err)
			}
		}
	})
}

func TestNewPoolValidation(t *testing.T) {
	if _, err := NewPool(PoolConfig{Workers: 1}); err == nil {
		t.Error("a pool without a runner must be rejected")
	}
	for _, w := range []int{0, -1} {
		if _, err := NewPool(PoolConfig{Runner: newRunner(), Workers: w}); err == nil {
			t.Errorf("%d workers must be rejected", w)
		}
	}
	p, err := NewPool(PoolConfig{Runner: newRunner(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.MaxPending != 1024 || p.cfg.Grace != 30*time.Second || p.cfg.SweepInterval != 30*time.Second {
		t.Errorf("defaults = %+v", p.cfg)
	}
}

func TestHealerSatisfiesJobRunner(t *testing.T) {
	var _ JobRunner = (*Healer)(nil)
}
