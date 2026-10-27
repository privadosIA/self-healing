package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

var (
	// ErrQueueFull is returned by Submit when too many jobs are waiting. The job
	// is not lost: it stays queued in the store and the sweep picks it up.
	ErrQueueFull = errors.New("core: the job queue is full")
	// ErrPoolClosed is returned by Submit once shutdown has begun.
	ErrPoolClosed = errors.New("core: the worker pool is shutting down")
)

// JobRunner runs one job. *Healer is the real implementation.
type JobRunner interface {
	Heal(ctx context.Context, ev model.WebhookEvent) (model.JobResult, error)
}

// QueueSource lists jobs that are queued in the store.
type QueueSource interface {
	ListQueued(ctx context.Context, limit int) ([]model.Job, error)
}

// PoolConfig configures a Pool.
type PoolConfig struct {
	Runner JobRunner
	// Source feeds the sweep with queued jobs from the store; nil disables it.
	Source QueueSource
	// Workers is how many jobs run at once; at least 1.
	Workers int
	// MaxPending bounds the jobs waiting or running in memory; 1024 by default.
	MaxPending int
	// Grace is how long running jobs may finish after shutdown begins before
	// they are cancelled; 30s by default.
	Grace time.Duration
	// SweepInterval is how often queued jobs are re-read from the store; 30s by
	// default.
	SweepInterval time.Duration
	// OnResult is called after every job with what was recorded; may be nil.
	OnResult func(model.JobResult)
	Log      *slog.Logger
}

// Pool runs jobs on a fixed number of workers. Jobs of the same repository run
// one at a time, in arrival order, while jobs of different repositories run in
// parallel.
//
// Serialization is done with a queue per repository rather than a lock per
// repository: a worker is never parked waiting for another job to finish, so a
// busy repository cannot starve the others.
type Pool struct {
	cfg PoolConfig

	mu      sync.Mutex
	known   map[string]struct{}            // delivery ids waiting or running
	busy    map[int64]bool                 // repositories with a job handed to a worker
	waiting map[int64][]model.WebhookEvent // jobs behind a busy repository, FIFO
	closed  bool

	ready chan model.WebhookEvent // jobs a worker may take now
	stop  chan struct{}
}

// NewPool validates cfg and returns a Pool. Call Run to start it.
func NewPool(cfg PoolConfig) (*Pool, error) {
	if cfg.Runner == nil {
		return nil, errors.New("core: a pool needs a runner")
	}
	if cfg.Workers < 1 {
		return nil, errors.New("core: a pool needs at least one worker")
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 1024
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 30 * time.Second
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Pool{
		cfg:     cfg,
		known:   map[string]struct{}{},
		busy:    map[int64]bool{},
		waiting: map[int64][]model.WebhookEvent{},
		ready:   make(chan model.WebhookEvent, cfg.MaxPending),
		stop:    make(chan struct{}),
	}, nil
}

// Submit hands a job to the pool without blocking. A delivery that is already
// waiting or running is ignored. The job must already be queued in the store.
func (p *Pool) Submit(ev model.WebhookEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrPoolClosed
	}
	if _, dup := p.known[ev.DeliveryID]; dup {
		return nil
	}
	if len(p.known) >= p.cfg.MaxPending {
		return ErrQueueFull
	}
	p.known[ev.DeliveryID] = struct{}{}
	if p.busy[ev.RepoID] {
		p.waiting[ev.RepoID] = append(p.waiting[ev.RepoID], ev)
		return nil
	}
	p.busy[ev.RepoID] = true
	p.ready <- ev // never blocks: ready has room for every known job
	return nil
}

// finished releases a job and, if its repository has more waiting, hands the
// next one over.
func (p *Pool) finished(ev model.WebhookEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.known, ev.DeliveryID)
	queue := p.waiting[ev.RepoID]
	if len(queue) == 0 || p.closed {
		// During shutdown the rest stay queued in the store for the next start.
		delete(p.waiting, ev.RepoID)
		delete(p.busy, ev.RepoID)
		for _, left := range queue {
			delete(p.known, left.DeliveryID)
		}
		return
	}
	next := queue[0]
	if len(queue) == 1 {
		delete(p.waiting, ev.RepoID)
	} else {
		p.waiting[ev.RepoID] = queue[1:]
	}
	p.ready <- next
}

// Run starts the workers and the sweep, and blocks until ctx is cancelled and
// every worker has stopped. On cancellation it stops taking jobs, lets running
// ones finish for the grace period, then cancels them.
func (p *Pool) Run(ctx context.Context) {
	// Jobs get their own context so that cancelling ctx does not cut them off at
	// once: that is what the grace period is for.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	var workers sync.WaitGroup
	for i := 0; i < p.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			p.work(jobCtx)
		}()
	}
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		p.sweepLoop(ctx)
	}()

	<-ctx.Done()
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	close(p.stop)

	drained := make(chan struct{})
	go func() { workers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(p.cfg.Grace):
		p.cfg.Log.Warn("shutdown grace period expired, cancelling running jobs")
		cancelJobs()
		<-drained
	}
	<-sweepDone
}

func (p *Pool) work(ctx context.Context) {
	for {
		select { // stop wins, so no new job starts once shutdown has begun
		case <-p.stop:
			return
		default:
		}
		select {
		case <-p.stop:
			return
		case ev := <-p.ready:
			p.run(ctx, ev)
		}
	}
}

// run executes one job. A panic in the runner must not take the worker, or the
// repository, down with it.
func (p *Pool) run(ctx context.Context, ev model.WebhookEvent) {
	defer p.finished(ev)
	defer func() {
		if r := recover(); r != nil {
			p.cfg.Log.Error("job panicked", "delivery_id", ev.DeliveryID, "panic", fmt.Sprint(r))
		}
	}()
	res, err := p.cfg.Runner.Heal(ctx, ev)
	if err != nil {
		if errors.Is(err, ports.ErrInvalidTransition) {
			return // another worker or a restart already took it
		}
		p.cfg.Log.Error("job could not be completed", "delivery_id", ev.DeliveryID, "error", err)
	}
	if p.cfg.OnResult != nil && res.Status != "" {
		p.cfg.OnResult(res)
	}
}

// sweepLoop reads queued jobs from the store at start, which recovers the ones
// that survived a restart, and then periodically, which recovers the ones that
// did not fit in the queue.
func (p *Pool) sweepLoop(ctx context.Context) {
	if p.cfg.Source == nil {
		return
	}
	p.Sweep(ctx)
	t := time.NewTicker(p.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Sweep(ctx)
		}
	}
}

// Sweep submits the jobs that are queued in the store. Errors are logged: the
// next sweep tries again.
func (p *Pool) Sweep(ctx context.Context) {
	if p.cfg.Source == nil {
		return
	}
	jobs, err := p.cfg.Source.ListQueued(ctx, p.cfg.MaxPending)
	if err != nil {
		if ctx.Err() == nil {
			p.cfg.Log.Error("cannot list queued jobs", "error", err)
		}
		return
	}
	for _, j := range jobs {
		if err := p.Submit(j.Event); err != nil {
			return // full or closing: the rest are picked up by a later sweep
		}
	}
}

// Intake stores a webhook's job and wakes the pool. It is what the webhook
// listener enqueues through.
type Intake struct {
	Store ports.JobEnqueuer
	Pool  *Pool
	Log   *slog.Logger
}

var _ ports.JobEnqueuer = (*Intake)(nil)

// Enqueue records the job and submits it. A full or closing pool is not an
// error for the caller: the job is safely queued in the store and the sweep, or
// the next start, picks it up.
func (i *Intake) Enqueue(ctx context.Context, ev model.WebhookEvent) (bool, error) {
	created, err := i.Store.Enqueue(ctx, ev)
	if err != nil || !created {
		return created, err
	}
	if err := i.Pool.Submit(ev); err != nil && i.Log != nil {
		i.Log.Warn("job stays queued until the next sweep", "delivery_id", ev.DeliveryID, "reason", err.Error())
	}
	return true, nil
}
