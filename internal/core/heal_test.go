package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

// --- fakes: no network and no disk anywhere in this file -------------------

type harness struct {
	calls []string // every port call, in order

	// store
	startErr, finishErr error
	finished            *model.JobResult
	finishCtxErr        error // ctx.Err() seen by Finish

	// scanners
	sonar, trivy       []model.Finding
	sonarErr, trivyErr error
	scannerRepo        model.Repo
	scannerSHA         string

	// vcs / workspace
	openPRs      int
	countErr     error
	checkoutErr  error
	branchExists bool
	branchErr    error
	files        map[string]string
	readErr      error
	commitErr    error
	pushErr      error
	prErr        error
	deleteErr    error
	closed       int
	deleteCtxErr error
	checkoutURL  string
	checkoutSHA  string
	commit       ports.CommitSpec
	pushed       string
	deleted      string
	pr           ports.PullRequest

	// llm
	proposal ports.Proposal
	llmErr   error
	req      ports.FixRequest
}

func newHarness() *harness {
	return &harness{
		sonar: []model.Finding{{Source: model.SourceSonar, RuleID: "go:S1234", Severity: model.SeverityMedium, File: "internal/app.go", LineStart: 4, LineEnd: 4, Message: "Dead code"}},
		files: map[string]string{"internal/app.go": "package internal\n"},
		proposal: ports.Proposal{
			Diff:    "--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1 +1 @@\n-package internal\n+package internal // fixed\n",
			Summary: "Fix it", Confidence: 0.9, TokensIn: 812, TokensOut: 96,
		},
	}
}

func (h *harness) note(s string) { h.calls = append(h.calls, s) }

func (h *harness) called(s string) int {
	n := 0
	for _, c := range h.calls {
		if c == s {
			n++
		}
	}
	return n
}

func (h *harness) healer(maxPRs int) *Healer {
	sel, _ := NewSelector([]string{"**/*.lock", "**/migrations/**", "**/*secret*"})
	return &Healer{
		Store:    (*fakeStore)(h),
		Scanners: []ports.Scanner{&fakeScanner{h: h, kind: "sonar"}, &fakeScanner{h: h, kind: "trivy"}},
		VCS:      (*fakeVCS)(h),
		LLM:      (*fakeLLM)(h),
		Selector: sel,
		Config:   HealerConfig{MaxOpenPRs: maxPRs, ContextLines: 30, Model: "local-model", GiteaURL: "https://gitea.example.com"},
	}
}

type fakeStore harness

func (s *fakeStore) Enqueue(context.Context, model.WebhookEvent) (bool, error) { return true, nil }
func (s *fakeStore) Get(context.Context, string) (model.Job, error)            { return model.Job{}, nil }
func (s *fakeStore) FailInterrupted(context.Context) (int, error)              { return 0, nil }
func (s *fakeStore) ListQueued(context.Context, int) ([]model.Job, error)      { return nil, nil }
func (s *fakeStore) Start(context.Context, string) error {
	(*harness)(s).note("store.start")
	return s.startErr
}
func (s *fakeStore) Finish(ctx context.Context, _ string, r model.JobResult) error {
	h := (*harness)(s)
	h.note("store.finish")
	h.finishCtxErr = ctx.Err()
	h.finished = &r
	return s.finishErr
}

type fakeScanner struct {
	h    *harness
	kind string
}

func (s *fakeScanner) Findings(_ context.Context, repo model.Repo, sha string) ([]model.Finding, error) {
	s.h.note("scan." + s.kind)
	s.h.scannerRepo, s.h.scannerSHA = repo, sha
	if s.kind == "sonar" {
		return s.h.sonar, s.h.sonarErr
	}
	return s.h.trivy, s.h.trivyErr
}

type fakeVCS harness

func (v *fakeVCS) CountOpenAutoFixPRs(context.Context, string) (int, error) {
	(*harness)(v).note("vcs.count")
	return v.openPRs, v.countErr
}
func (v *fakeVCS) Checkout(_ context.Context, url, sha string) (ports.Workspace, error) {
	h := (*harness)(v)
	h.note("vcs.checkout")
	h.checkoutURL, h.checkoutSHA = url, sha
	if v.checkoutErr != nil {
		return nil, v.checkoutErr
	}
	return &fakeWorkspace{h: h}, nil
}
func (v *fakeVCS) OpenPullRequest(_ context.Context, pr ports.PullRequest) (string, error) {
	h := (*harness)(v)
	h.note("vcs.open-pr")
	h.pr = pr
	if v.prErr != nil {
		return "", v.prErr
	}
	return "https://gitea.example.com/acme/widget/pulls/7", nil
}

type fakeWorkspace struct{ h *harness }

func (w *fakeWorkspace) ReadFile(p string) ([]byte, error) {
	w.h.note("ws.read")
	if w.h.readErr != nil {
		return nil, w.h.readErr
	}
	c, ok := w.h.files[p]
	if !ok {
		return nil, errors.New("file does not exist")
	}
	return []byte(c), nil
}
func (w *fakeWorkspace) BranchExists(_ context.Context, b string) (bool, error) {
	w.h.note("ws.branch-exists")
	return w.h.branchExists, w.h.branchErr
}
func (w *fakeWorkspace) CommitPatch(_ context.Context, s ports.CommitSpec) (string, error) {
	w.h.note("ws.commit")
	w.h.commit = s
	if w.h.commitErr != nil {
		return "", w.h.commitErr
	}
	return strings.Repeat("a", 40), nil
}
func (w *fakeWorkspace) Push(_ context.Context, b string) error {
	w.h.note("ws.push")
	w.h.pushed = b
	return w.h.pushErr
}
func (w *fakeWorkspace) DeleteRemoteBranch(ctx context.Context, b string) error {
	w.h.note("ws.delete-branch")
	w.h.deleted, w.h.deleteCtxErr = b, ctx.Err()
	return w.h.deleteErr
}
func (w *fakeWorkspace) Close() error { w.h.note("ws.close"); w.h.closed++; return nil }

type fakeLLM harness

func (l *fakeLLM) Propose(_ context.Context, r ports.FixRequest) (ports.Proposal, error) {
	h := (*harness)(l)
	h.note("llm.propose")
	h.req = r
	return l.proposal, l.llmErr
}

func event() model.WebhookEvent {
	return model.WebhookEvent{
		RepoID: 42, RepoFullName: "acme/widget", CloneURL: "https://gitea.example.com/acme/widget.git",
		DefaultBranch: "main", CommitSHA: "0123456789abcdef0123456789abcdef01234567", Branch: "main",
		PipelineStatus: "failure", JobName: "quality", RunID: 2001, DeliveryID: "d-1",
	}
}

// --- tests ------------------------------------------------------------------

func TestHealOpensAPullRequest(t *testing.T) {
	h := newHarness()
	res, err := h.healer(3).Heal(context.Background(), event())
	if err != nil {
		t.Fatal(err)
	}

	want := model.JobResult{Status: model.JobPROpened, RuleID: "go:S1234", PRURL: "https://gitea.example.com/acme/widget/pulls/7", TokensIn: 812, TokensOut: 96}
	if res != want {
		t.Errorf("result = %+v\nwant     %+v", res, want)
	}
	if h.finished == nil || *h.finished != want {
		t.Errorf("the store recorded %+v", h.finished)
	}

	// Cheap checks first, then the work, then cleanup, in this order.
	wantCalls := []string{
		"store.start", "vcs.count", "scan.sonar", "scan.trivy", "vcs.checkout", "ws.branch-exists", "ws.read",
		"llm.propose", "ws.commit", "ws.push", "vcs.open-pr", "ws.close", "store.finish",
	}
	if strings.Join(h.calls, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("calls:\n got  %v\n want %v", h.calls, wantCalls)
	}

	// What was passed along.
	if h.scannerRepo != (model.Repo{ID: 42, FullName: "acme/widget", DefaultBranch: "main", Branch: "main"}) || h.scannerSHA != event().CommitSHA {
		t.Errorf("scanner got %+v %s", h.scannerRepo, h.scannerSHA)
	}
	if h.checkoutURL != event().CloneURL || h.checkoutSHA != event().CommitSHA {
		t.Errorf("checkout got %s %s", h.checkoutURL, h.checkoutSHA)
	}
	if h.req.Finding.RuleID != "go:S1234" || h.req.FileContent != "package internal\n" || h.req.ContextLines != 30 || h.req.BuildCmd != "go build ./..." {
		t.Errorf("model request = %+v", h.req)
	}
	c := h.commit
	if c.Branch != "auto-fix/go-s1234/0123456" || c.File != "internal/app.go" || c.Diff != h.proposal.Diff || c.Subject != "fix(sonar): go:S1234 in internal/app.go" {
		t.Errorf("commit spec = %+v", c)
	}
	trailers := map[string]string{}
	for _, tr := range c.Trailers {
		trailers[tr.Key] = tr.Value
	}
	if trailers["Finding-Source"] != "sonar" || trailers["Finding-Rule"] != "go:S1234" || trailers["Triggered-By"] != "healerd delivery d-1" || len(trailers) != 3 {
		t.Errorf("trailers = %v", trailers)
	}
	if h.pushed != c.Branch {
		t.Errorf("pushed %q, committed %q", h.pushed, c.Branch)
	}
	if h.pr.Repo != "acme/widget" || h.pr.Head != c.Branch || h.pr.Base != "main" || strings.Join(h.pr.Labels, ",") != "auto-fix,sonar" ||
		!strings.Contains(h.pr.Body, "- [ ] Reviewed by a human") {
		t.Errorf("pull request = %+v", h.pr)
	}
	if h.closed != 1 {
		t.Errorf("workspace closed %d times", h.closed)
	}
}

func TestHealSkipsWithoutSpendingWork(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*harness)
		maxPRs    int
		reason    string
		notCalled []string // calls that must not have happened
	}{
		{"open PR limit reached", func(h *harness) { h.openPRs = 3 }, 3, "limit 3", []string{"scan.sonar", "vcs.checkout", "llm.propose"}},
		{"limit exceeded", func(h *harness) { h.openPRs = 9 }, 3, "9 open", []string{"scan.sonar", "vcs.checkout", "llm.propose"}},
		{"no findings", func(h *harness) { h.sonar = nil }, 3, "no fixable finding (0 reported", []string{"vcs.checkout", "llm.propose"}},
		{"only excluded findings", func(h *harness) {
			h.sonar = []model.Finding{{RuleID: "R", Severity: model.SeverityHigh, File: "yarn.lock", LineStart: 1, LineEnd: 1}}
		}, 3, "1 excluded", []string{"vcs.checkout", "llm.propose"}},
		{"branch already exists", func(h *harness) { h.branchExists = true }, 3, "auto-fix/go-s1234/0123456 already exists", []string{"llm.propose", "ws.commit", "vcs.open-pr"}},
		{"file cannot be read", func(h *harness) { h.readErr = errors.New("file is too large") }, 3, "cannot read internal/app.go", []string{"llm.propose", "ws.commit"}},
		{"model declines", func(h *harness) { h.llmErr = errors.Join(ports.ErrNoProposal, errors.New("low confidence")) }, 3, "no usable proposal", []string{"ws.commit", "ws.push", "vcs.open-pr"}},
		{"patch rejected", func(h *harness) { h.commitErr = errors.Join(ports.ErrPatchRejected, errors.New("does not apply")) }, 3, "patch rejected", []string{"ws.push", "vcs.open-pr"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			tt.setup(h)
			res, err := h.healer(tt.maxPRs).Heal(context.Background(), event())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != model.JobSkipped || !strings.Contains(res.Error, tt.reason) {
				t.Errorf("result = %+v, want skipped with %q", res, tt.reason)
			}
			for _, c := range tt.notCalled {
				if h.called(c) != 0 {
					t.Errorf("%s was called", c)
				}
			}
			if h.finished == nil || h.finished.Status != model.JobSkipped {
				t.Errorf("the store recorded %+v", h.finished)
			}
			if h.called("ws.close") != h.called("vcs.checkout") {
				t.Error("a workspace was left open")
			}
		})
	}
}

func TestHealBelowTheLimitProceeds(t *testing.T) {
	h := newHarness()
	h.openPRs = 2
	if res, _ := h.healer(3).Heal(context.Background(), event()); res.Status != model.JobPROpened {
		t.Errorf("status = %s", res.Status)
	}
}

func TestHealFailsOnInfrastructureErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		setup     func(*harness)
		reason    string
		notCalled []string
		closed    int
	}{
		{"counting pull requests", func(h *harness) { h.countErr = boom }, "counting open pull requests", []string{"scan.sonar"}, 0},
		{"a scanner fails", func(h *harness) { h.sonarErr = boom }, "scanning", []string{"vcs.checkout"}, 0},
		{"checkout fails", func(h *harness) { h.checkoutErr = boom }, "checking out the commit", []string{"llm.propose"}, 0},
		{"branch check fails", func(h *harness) { h.branchErr = boom }, "checking the fix branch", []string{"llm.propose"}, 1},
		{"model call fails", func(h *harness) { h.llmErr = boom }, "asking the model", []string{"ws.commit"}, 1},
		{"commit fails for a reason other than the patch", func(h *harness) { h.commitErr = boom }, "committing the patch", []string{"ws.push"}, 1},
		{"push fails", func(h *harness) { h.pushErr = boom }, "pushing the branch", []string{"vcs.open-pr"}, 1},
		{"opening the pull request fails", func(h *harness) { h.prErr = boom }, "opening the pull request", nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			tt.setup(h)
			res, err := h.healer(3).Heal(context.Background(), event())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != model.JobFailed || !strings.Contains(res.Error, tt.reason) || !strings.Contains(res.Error, "boom") {
				t.Errorf("result = %+v, want failed with %q", res, tt.reason)
			}
			for _, c := range tt.notCalled {
				if h.called(c) != 0 {
					t.Errorf("%s was called", c)
				}
			}
			if h.closed != tt.closed {
				t.Errorf("workspace closed %d times, want %d", h.closed, tt.closed)
			}
			if h.finished == nil || h.finished.Status != model.JobFailed {
				t.Errorf("the store recorded %+v", h.finished)
			}
		})
	}
}

func TestHealRecordsTokensWhenTheModelWasUsed(t *testing.T) {
	h := newHarness()
	h.commitErr = errors.Join(ports.ErrPatchRejected, errors.New("x"))
	res, _ := h.healer(3).Heal(context.Background(), event())
	if res.Status != model.JobSkipped || res.TokensIn != 812 || res.TokensOut != 96 {
		t.Errorf("a skipped job after a model call lost its token usage: %+v", res)
	}
	h = newHarness()
	h.pushErr = errors.New("boom")
	res, _ = h.healer(3).Heal(context.Background(), event())
	if res.TokensIn != 812 || res.TokensOut != 96 {
		t.Errorf("a failed job after a model call lost its token usage: %+v", res)
	}
	h = newHarness()
	h.openPRs = 3
	if res, _ = h.healer(3).Heal(context.Background(), event()); res.TokensIn != 0 || res.TokensOut != 0 {
		t.Errorf("tokens recorded for a job that never called the model: %+v", res)
	}
}

func TestHealUndoesThePushWhenThePullRequestFails(t *testing.T) {
	h := newHarness()
	h.prErr = errors.New("boom")
	res, _ := h.healer(3).Heal(context.Background(), event())
	if res.Status != model.JobFailed {
		t.Fatalf("status = %s", res.Status)
	}
	if h.deleted != "auto-fix/go-s1234/0123456" {
		t.Errorf("deleted branch %q", h.deleted)
	}
	order := strings.Join(h.calls, ",")
	if !strings.Contains(order, "ws.push,vcs.open-pr,ws.delete-branch") {
		t.Errorf("order = %s", order)
	}
	// A failed cleanup is logged, never allowed to change the outcome.
	h = newHarness()
	h.prErr, h.deleteErr = errors.New("boom"), errors.New("cannot delete")
	if res, _ := h.healer(3).Heal(context.Background(), event()); res.Status != model.JobFailed || strings.Contains(res.Error, "cannot delete") {
		t.Errorf("result = %+v", res)
	}
	// No cleanup when nothing was pushed.
	h = newHarness()
	h.pushErr = errors.New("boom")
	h.healer(3).Heal(context.Background(), event())
	if h.called("ws.delete-branch") != 0 {
		t.Error("deleted a branch that was never pushed")
	}
}

func TestHealToleratesAMissingTrivyReport(t *testing.T) {
	h := newHarness()
	h.trivyErr = ports.ErrReportNotFound
	if res, _ := h.healer(3).Heal(context.Background(), event()); res.Status != model.JobPROpened {
		t.Errorf("a repository without Trivy should still be healed: %+v", res)
	}

	// With only a missing report there is simply nothing to fix.
	h = newHarness()
	h.sonar, h.trivyErr = nil, ports.ErrReportNotFound
	if res, _ := h.healer(3).Heal(context.Background(), event()); res.Status != model.JobSkipped {
		t.Errorf("result = %+v", res)
	}

	// Any other Trivy failure is a failure.
	h = newHarness()
	h.trivyErr = errors.New("report is corrupt")
	if res, _ := h.healer(3).Heal(context.Background(), event()); res.Status != model.JobFailed {
		t.Errorf("result = %+v", res)
	}
}

func TestHealMergesFindingsFromEveryScanner(t *testing.T) {
	h := newHarness()
	h.trivy = []model.Finding{{Source: model.SourceTrivy, RuleID: "CVE-2099-1", Severity: model.SeverityCritical, File: "go.mod", Package: "x"}}
	h.files["go.mod"] = "module example.com/app\n"
	res, _ := h.healer(3).Heal(context.Background(), event())
	if res.RuleID != "CVE-2099-1" {
		t.Errorf("chose %q, want the most severe finding across scanners", res.RuleID)
	}
	if h.req.BuildCmd != "go build ./..." || h.commit.File != "go.mod" || h.pr.Labels[1] != "trivy" {
		t.Errorf("request %+v commit %+v labels %v", h.req, h.commit, h.pr.Labels)
	}
}

func TestHealIgnoresAJobThatIsNotQueued(t *testing.T) {
	h := newHarness()
	h.startErr = ports.ErrInvalidTransition
	_, err := h.healer(3).Heal(context.Background(), event())
	if !errors.Is(err, ports.ErrInvalidTransition) {
		t.Errorf("err = %v", err)
	}
	if len(h.calls) != 1 || h.calls[0] != "store.start" {
		t.Errorf("work was done for a job someone else owns: %v", h.calls)
	}

	h = newHarness()
	h.startErr = ports.ErrNotFound
	if _, err := h.healer(3).Heal(context.Background(), event()); err == nil || len(h.calls) != 1 {
		t.Errorf("err = %v, calls = %v", err, h.calls)
	}
}

func TestHealReportsAFailureToRecordTheResult(t *testing.T) {
	h := newHarness()
	h.finishErr = errors.New("database is locked")
	res, err := h.healer(3).Heal(context.Background(), event())
	if err == nil || !strings.Contains(err.Error(), "recording the result") {
		t.Errorf("err = %v", err)
	}
	if res.Status != model.JobPROpened {
		t.Errorf("the result should still be returned: %+v", res)
	}
}

func TestHealRecordsTheOutcomeEvenWhenCancelled(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the context as the model call starts, as a shutdown would.
	hh := h.healer(3)
	hh.LLM = cancelLLM{h: h, cancel: cancel}
	res, err := hh.Heal(ctx, event())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", res.Status)
	}
	if h.finished == nil || h.finishCtxErr != nil {
		t.Errorf("Finish ran with a cancelled context (%v) or not at all", h.finishCtxErr)
	}
}

type cancelLLM struct {
	h      *harness
	cancel context.CancelFunc
}

func (c cancelLLM) Propose(ctx context.Context, _ ports.FixRequest) (ports.Proposal, error) {
	c.h.note("llm.propose")
	c.cancel()
	return ports.Proposal{}, ctx.Err()
}

func TestHealCleansUpEvenWhenCancelled(t *testing.T) {
	h := newHarness()
	h.prErr = errors.New("boom")
	ctx, cancel := context.WithCancel(context.Background())
	hh := h.healer(3)
	hh.VCS = cancelVCS{(*fakeVCS)(h), cancel}
	res, _ := hh.Heal(ctx, event())
	if res.Status != model.JobFailed {
		t.Fatalf("status = %s", res.Status)
	}
	if h.deleted == "" || h.deleteCtxErr != nil {
		t.Errorf("the branch was not removed with a usable context (deleted %q, ctx err %v)", h.deleted, h.deleteCtxErr)
	}
}

type cancelVCS struct {
	*fakeVCS
	cancel context.CancelFunc
}

func (c cancelVCS) OpenPullRequest(ctx context.Context, pr ports.PullRequest) (string, error) {
	c.cancel()
	return c.fakeVCS.OpenPullRequest(ctx, pr)
}

func TestHealUsesTheEventsOwnBranchAsTheBase(t *testing.T) {
	h := newHarness()
	ev := event()
	ev.Branch = "release/1.2"
	h.healer(3).Heal(context.Background(), ev)
	if h.pr.Base != "release/1.2" || h.scannerRepo.Branch != "release/1.2" {
		t.Errorf("base %q, scanner branch %q", h.pr.Base, h.scannerRepo.Branch)
	}
}

func TestBuildCmd(t *testing.T) {
	for in, want := range map[string]string{
		"main.go": "go build ./...", "internal/app.go": "go build ./...", "go.mod": "go build ./...", "svc/go.mod": "go build ./...",
		"app.py": "", "package.json": "", "Makefile": "", "go.modx": "",
	} {
		if got := buildCmd(in); got != want {
			t.Errorf("buildCmd(%q) = %q, want %q", in, got, want)
		}
	}
}
