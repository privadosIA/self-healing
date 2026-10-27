package gitea

import (
	"context"
	"strings"
	"testing"
)

func TestPushPublishesTheFixBranch(t *testing.T) {
	repo, ws := checkout(t)
	s := spec()
	sha, err := ws.CommitPatch(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.BranchExists(context.Background(), s.Branch); exists {
		t.Fatal("the branch exists before the push")
	}

	if err := ws.Push(context.Background(), s.Branch); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// The server now has the branch, at the fix commit, whose parent is the failing commit.
	if got := git(t, repo.bare, "rev-parse", "refs/heads/"+s.Branch); got != sha {
		t.Errorf("remote branch is at %s, want %s", got, sha)
	}
	if parent := git(t, repo.bare, "rev-parse", s.Branch+"^"); parent != repo.sha {
		t.Errorf("remote commit descends from %s, want %s", parent, repo.sha)
	}
	if exists, err := ws.BranchExists(context.Background(), s.Branch); err != nil || !exists {
		t.Errorf("BranchExists after the push = %v, %v", exists, err)
	}
	// Pushing the same commit again is harmless.
	if err := ws.Push(context.Background(), s.Branch); err != nil {
		t.Errorf("second push: %v", err)
	}
}

func TestPushNeverForces(t *testing.T) {
	repo, ws := checkout(t)
	s := spec()
	// Someone else already owns that branch name with a different commit.
	git(t, repo.bare, "branch", s.Branch, repo.sha)
	other := git(t, repo.bare, "commit-tree", "-p", repo.sha, "-m", "someone else", repo.sha+"^{tree}")
	git(t, repo.bare, "update-ref", "refs/heads/"+s.Branch, other)

	if _, err := ws.CommitPatch(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := ws.Push(context.Background(), s.Branch); err == nil {
		t.Fatal("pushing over a diverged remote branch must fail")
	}
	if got := git(t, repo.bare, "rev-parse", "refs/heads/"+s.Branch); got != other {
		t.Errorf("the remote branch was overwritten: %s", got)
	}
}

func TestPushRejectsBadBranchNamesAndWrongTokens(t *testing.T) {
	_, ws := checkout(t)
	for _, bad := range []string{"", "-x", "a..b", "a b", "--delete", ":refs/heads/main", "main:main"} {
		if err := ws.Push(context.Background(), bad); err == nil {
			t.Errorf("Push(%q) accepted a bad branch name", bad)
		}
		if err := ws.DeleteRemoteBranch(context.Background(), bad); err == nil {
			t.Errorf("DeleteRemoteBranch(%q) accepted a bad branch name", bad)
		}
	}

	repo := serveRepo(t, map[string]string{"main.go": "package main\n"}, nil)
	good, err := newAdapter(t, repo.srv.URL, testToken, t.TempDir()).Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = good.Close() }()
	// Same checkout, but the stored credentials no longer work.
	w := good.(*workspace)
	w.opts.Token = "revoked-token"
	if _, err := w.CommitPatch(context.Background(), spec()); err == nil {
		t.Log("(the diff does not match this repository, which is fine)")
	}
	err = w.Push(context.Background(), "auto-fix/x/0123456")
	if err == nil {
		t.Fatal("a push with a revoked token must fail")
	}
	if strings.Contains(err.Error(), "revoked-token") || strings.Contains(err.Error(), testToken) {
		t.Errorf("the error leaks a token: %v", err)
	}
}

func TestDeleteRemoteBranchUndoesAPush(t *testing.T) {
	repo, ws := checkout(t)
	s := spec()
	if _, err := ws.CommitPatch(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := ws.Push(context.Background(), s.Branch); err != nil {
		t.Fatal(err)
	}
	if err := ws.DeleteRemoteBranch(context.Background(), s.Branch); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if exists, err := ws.BranchExists(context.Background(), s.Branch); err != nil || exists {
		t.Errorf("the branch is still on the remote: %v, %v", exists, err)
	}
	if out := git(t, repo.bare, "for-each-ref", "refs/heads/auto-fix"); out != "" {
		t.Errorf("the server still lists %q", out)
	}
	// The default branch is untouched.
	if head := git(t, repo.bare, "rev-parse", "HEAD"); head != repo.sha {
		t.Errorf("the server's HEAD changed to %s", head)
	}
	// Deleting what is already gone may or may not be an error, depending on the
	// git version; either way the branch stays absent and the caller can ignore it.
	_ = ws.DeleteRemoteBranch(context.Background(), s.Branch)
	if exists, _ := ws.BranchExists(context.Background(), s.Branch); exists {
		t.Error("the branch came back")
	}
}
