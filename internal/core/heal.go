package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

// finishTimeout bounds recording a result and cleaning up after a job whose
// context was cancelled, for example by a shutdown.
const finishTimeout = 10 * time.Second

// HealerConfig holds the settings of the use case.
type HealerConfig struct {
	// MaxOpenPRs is the most open auto-fix pull requests a repository may have.
	MaxOpenPRs int
	// ContextLines is the context kept around a finding; zero means the default.
	ContextLines int
	// Model is the model's name, shown in the pull request.
	Model string
	// GiteaURL and SonarURL build links in the pull request; SonarURL may be empty.
	GiteaURL, SonarURL string
}

// Healer turns a failed-pipeline event into, at best, a pull request.
type Healer struct {
	Store    ports.JobStore
	Scanners []ports.Scanner
	VCS      ports.VCS
	LLM      ports.LLM
	Selector *Selector
	Config   HealerConfig
	Log      *slog.Logger
}

// Heal runs the job queued for the event and records how it ended: a pull
// request, skipped (nothing safe to do) or failed (something broke). It never
// merges anything.
//
// The returned result is what was recorded. The error is non-nil only when the
// job could not be run or recorded at all, for example because the store is
// down; a job that fails is a result, not an error.
func (h *Healer) Heal(ctx context.Context, ev model.WebhookEvent) (model.JobResult, error) {
	if err := h.Store.Start(ctx, ev.DeliveryID); err != nil {
		if errors.Is(err, ports.ErrInvalidTransition) {
			// Already started or finished by another worker: nothing to do.
			return model.JobResult{}, fmt.Errorf("core: job %s is not queued: %w", ev.DeliveryID, err)
		}
		return model.JobResult{}, fmt.Errorf("core: starting job: %w", err)
	}

	res := h.run(ctx, ev)

	// Record the outcome even if ctx was cancelled mid-job, or the job would be
	// left running until the next restart marks it failed.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := h.Store.Finish(fctx, ev.DeliveryID, res); err != nil {
		return res, fmt.Errorf("core: recording the result: %w", err)
	}
	h.log().Info("job finished", "delivery_id", ev.DeliveryID, "repo", ev.RepoFullName, "status", string(res.Status), "rule", res.RuleID)
	return res, nil
}

func (h *Healer) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.DiscardHandler)
}

func skipped(rule, format string, args ...any) model.JobResult {
	return model.JobResult{Status: model.JobSkipped, RuleID: rule, Error: fmt.Sprintf(format, args...)}
}

func failed(rule string, err error) model.JobResult {
	return model.JobResult{Status: model.JobFailed, RuleID: rule, Error: err.Error()}
}

// run does the work. Cheap checks come first so that no checkout or model call
// is spent on a job that will be skipped anyway.
func (h *Healer) run(ctx context.Context, ev model.WebhookEvent) model.JobResult {
	repo := model.Repo{ID: ev.RepoID, FullName: ev.RepoFullName, DefaultBranch: ev.DefaultBranch, Branch: ev.Branch}

	open, err := h.VCS.CountOpenAutoFixPRs(ctx, ev.RepoFullName)
	if err != nil {
		return failed("", fmt.Errorf("counting open pull requests: %w", err))
	}
	if open >= h.Config.MaxOpenPRs {
		return skipped("", "the repository already has %d open auto-fix pull requests (limit %d)", open, h.Config.MaxOpenPRs)
	}

	findings, err := h.scan(ctx, repo, ev.CommitSHA)
	if err != nil {
		return failed("", err)
	}
	sel := h.Selector.Select(findings)
	if !sel.Found {
		return skipped("", "no fixable finding (%d reported, %d excluded)", sel.Considered, sel.Excluded)
	}
	f := sel.Finding
	branch := model.AutoFixBranch(f.RuleID, ev.CommitSHA)

	ws, err := h.VCS.Checkout(ctx, ev.CloneURL, ev.CommitSHA)
	if err != nil {
		return failed(f.RuleID, fmt.Errorf("checking out the commit: %w", err))
	}
	defer func() {
		if err := ws.Close(); err != nil {
			h.log().Warn("cannot remove the workspace", "delivery_id", ev.DeliveryID, "error", err)
		}
	}()

	exists, err := ws.BranchExists(ctx, branch)
	if err != nil {
		return failed(f.RuleID, fmt.Errorf("checking the fix branch: %w", err))
	}
	if exists {
		return skipped(f.RuleID, "the branch %s already exists", branch)
	}

	content, err := ws.ReadFile(f.File)
	if err != nil {
		return skipped(f.RuleID, "cannot read %s at this commit: %v", f.File, err)
	}

	prop, err := h.LLM.Propose(ctx, ports.FixRequest{
		Finding: f, FileContent: string(content), ContextLines: h.Config.ContextLines, BuildCmd: buildCmd(f.File),
	})
	if err != nil {
		if errors.Is(err, ports.ErrNoProposal) {
			return skipped(f.RuleID, "%v", err)
		}
		return failed(f.RuleID, fmt.Errorf("asking the model: %w", err))
	}
	tokens := func(r model.JobResult) model.JobResult {
		r.TokensIn, r.TokensOut = prop.TokensIn, prop.TokensOut
		return r
	}

	_, err = ws.CommitPatch(ctx, ports.CommitSpec{
		Branch:  branch,
		File:    f.File,
		Diff:    prop.Diff,
		Subject: model.CommitSubject(f),
		Trailers: []ports.Trailer{
			{Key: "Finding-Source", Value: f.Source},
			{Key: "Finding-Rule", Value: f.RuleID},
			{Key: "Triggered-By", Value: "healerd delivery " + ev.DeliveryID},
		},
	})
	if err != nil {
		if errors.Is(err, ports.ErrPatchRejected) {
			return tokens(skipped(f.RuleID, "%v", err))
		}
		return tokens(failed(f.RuleID, fmt.Errorf("committing the patch: %w", err)))
	}

	if err := ws.Push(ctx, branch); err != nil {
		return tokens(failed(f.RuleID, fmt.Errorf("pushing the branch: %w", err)))
	}

	url, err := h.VCS.OpenPullRequest(ctx, BuildPullRequest(FixReport{
		Event: ev, Finding: f, Proposal: prop, Branch: branch,
		Model: h.Config.Model, GiteaURL: h.Config.GiteaURL, SonarURL: h.Config.SonarURL,
	}))
	if err != nil {
		// Do not leave a branch without a pull request: it would make every
		// later failure of this commit look already handled.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		if derr := ws.DeleteRemoteBranch(dctx, branch); derr != nil {
			h.log().Warn("cannot remove the branch of a failed pull request", "delivery_id", ev.DeliveryID, "error", derr)
		}
		return tokens(failed(f.RuleID, fmt.Errorf("opening the pull request: %w", err)))
	}

	return tokens(model.JobResult{Status: model.JobPROpened, RuleID: f.RuleID, PRURL: url})
}

// scan gathers the findings of every scanner. A Trivy report that was never
// uploaded is not an error, since a repository may not run Trivy, but any other
// failure is: choosing among partial results could pick a less severe finding
// while a more severe one went unseen.
func (h *Healer) scan(ctx context.Context, repo model.Repo, sha string) ([]model.Finding, error) {
	var all []model.Finding
	for _, s := range h.Scanners {
		got, err := s.Findings(ctx, repo, sha)
		if errors.Is(err, ports.ErrReportNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("scanning: %w", err)
		}
		all = append(all, got...)
	}
	return all, nil
}

// buildCmd is the command a model can be told verifies the project builds.
func buildCmd(file string) string {
	switch {
	case strings.HasSuffix(file, ".go"), file == "go.mod", strings.HasSuffix(file, "/go.mod"):
		return "go build ./..."
	default:
		return ""
	}
}
