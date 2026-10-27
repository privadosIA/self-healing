package gitcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir, isolated from the machine's configuration, and
// returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestValidBranch(t *testing.T) {
	good := []string{"main", "auto-fix/go-s1234/0123456", "release/1.2", "feature_x", "a", "A1.b-c/d_e"}
	bad := []string{
		"", "-x", "--delete", "/x", ".x", "a..b", "a//b", "a/", "a.", "x.lock", "auto-fix/x.lock", "a/.hidden",
		"a b", "a\tb", "a\nb", "a:b", "a?b", "a*b", "a[b", "a\\b", "a~b", "a^b", "a@{b", "refs/heads/x:y", "é", "a" + strings.Repeat("b", 250),
	}
	for _, b := range good {
		if !ValidBranch(b) {
			t.Errorf("ValidBranch(%q) = false, want true", b)
		}
	}
	for _, b := range bad {
		if ValidBranch(b) {
			t.Errorf("ValidBranch(%q) = true, want false", b)
		}
	}
}

var author = Options{Protocols: "file", AuthorName: "ci-healer-bot", AuthorEmail: "bot@example.com"}

const commentDiff = "--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n package main\n+// fixed\n"

// clone returns a working copy of a fresh origin at its only commit.
func clone(t *testing.T) (src, work, sha string) {
	t.Helper()
	src, sha = origin(t)
	work = t.TempDir()
	if err := Clone(context.Background(), work, "file://"+src, sha, author); err != nil {
		t.Fatal(err)
	}
	return src, work, sha
}

func TestBranchCommitPushDeleteLifecycle(t *testing.T) {
	ctx := context.Background()
	src, work, sha := clone(t)
	const branch = "auto-fix/go-s1234/0123456"

	if exists, err := RemoteBranchExists(ctx, work, branch, author); err != nil || exists {
		t.Fatalf("before: exists %v, err %v", exists, err)
	}
	if err := CreateBranch(ctx, work, branch, author); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToIndex(ctx, work, commentDiff, author); err != nil {
		t.Fatal(err)
	}
	files, err := StagedFiles(ctx, work, author)
	if err != nil || len(files) != 1 || files[0] != "main.go" {
		t.Fatalf("staged = %v, %v", files, err)
	}
	commit, err := Commit(ctx, work, "fix: add a comment\n\nFinding-Rule: x\n", author)
	if err != nil {
		t.Fatal(err)
	}
	if !fullSHA.MatchString(commit) || commit == sha {
		t.Errorf("commit id = %q", commit)
	}
	if staged, _ := StagedFiles(ctx, work, author); len(staged) != 0 {
		t.Errorf("the index is not clean after the commit: %v", staged)
	}

	if err := Push(ctx, work, branch, author); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if exists, err := RemoteBranchExists(ctx, work, branch, author); err != nil || !exists {
		t.Errorf("after the push: exists %v, err %v", exists, err)
	}
	if got := git(t, src, "rev-parse", branch); got != commit {
		t.Errorf("the remote branch is at %s, want %s", got, commit)
	}
	// A longer name that merely starts with the branch is a different branch.
	if exists, _ := RemoteBranchExists(ctx, work, branch+"-more", author); exists {
		t.Error("a prefix was taken for the branch")
	}

	if err := DeleteRemoteBranch(ctx, work, branch, author); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if exists, _ := RemoteBranchExists(ctx, work, branch, author); exists {
		t.Error("the branch is still on the remote")
	}
}

func TestResetHardDiscardsAPatch(t *testing.T) {
	ctx := context.Background()
	_, work, _ := clone(t)
	if err := ApplyToIndex(ctx, work, commentDiff, author); err != nil {
		t.Fatal(err)
	}
	if err := ResetHard(ctx, work, author); err != nil {
		t.Fatal(err)
	}
	if staged, _ := StagedFiles(ctx, work, author); len(staged) != 0 {
		t.Errorf("still staged: %v", staged)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "main.go")); string(b) != "package main\n" {
		t.Errorf("the working tree was not restored: %q", b)
	}
}

func TestBranchOperationsRejectBadInput(t *testing.T) {
	ctx := context.Background()
	_, work, _ := clone(t)
	for _, b := range []string{"", "-x", "a..b", "a b", "--delete", ":refs/heads/main"} {
		if _, err := RemoteBranchExists(ctx, work, b, author); err == nil {
			t.Errorf("RemoteBranchExists(%q) accepted the name", b)
		}
		if err := CreateBranch(ctx, work, b, author); err == nil {
			t.Errorf("CreateBranch(%q) accepted the name", b)
		}
		if err := Push(ctx, work, b, author); err == nil {
			t.Errorf("Push(%q) accepted the name", b)
		}
		if err := DeleteRemoteBranch(ctx, work, b, author); err == nil {
			t.Errorf("DeleteRemoteBranch(%q) accepted the name", b)
		}
	}
	if err := ApplyToIndex(ctx, work, " \n", author); err == nil {
		t.Error("an empty diff was applied")
	}
	if err := ApplyToIndex(ctx, work, strings.ReplaceAll(commentDiff, "main.go", "missing.go"), author); err == nil {
		t.Error("a diff for a missing file was applied")
	}
}

func TestCommitNeedsAnAuthorAndAMessage(t *testing.T) {
	ctx := context.Background()
	_, work, _ := clone(t)
	if err := ApplyToIndex(ctx, work, commentDiff, author); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"no author": {Protocols: "file"},
		"no name":   {Protocols: "file", AuthorEmail: "bot@example.com"},
		"no email":  {Protocols: "file", AuthorName: "bot"},
	} {
		if _, err := Commit(ctx, work, "fix: x\n", o); err == nil {
			t.Errorf("%s: a commit was made without an identity", name)
		}
	}
	for _, msg := range []string{"", "  \n "} {
		if _, err := Commit(ctx, work, msg, author); err == nil {
			t.Errorf("a commit was made with the message %q", msg)
		}
	}
	// None of the refused attempts committed anything.
	if staged, _ := StagedFiles(ctx, work, author); len(staged) != 1 {
		t.Errorf("staged = %v: the refused commits must leave the index alone", staged)
	}
}

func TestPushRefusesToOverwriteADivergedBranch(t *testing.T) {
	ctx := context.Background()
	src, work, sha := clone(t)
	const branch = "auto-fix/x/0123456"
	// The remote already has this branch, at a different commit.
	other := git(t, src, "commit-tree", "-p", sha, "-m", "somebody else", sha+"^{tree}")
	git(t, src, "update-ref", "refs/heads/"+branch, other)

	if err := CreateBranch(ctx, work, branch, author); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToIndex(ctx, work, commentDiff, author); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(ctx, work, "fix: x\n", author); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, work, branch, author); err == nil {
		t.Error("a push over a diverged branch must fail")
	}
	if got := git(t, src, "rev-parse", "refs/heads/"+branch); got != other {
		t.Errorf("the remote branch was overwritten: %s", got)
	}
}
