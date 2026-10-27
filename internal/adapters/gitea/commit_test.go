package gitea

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/ports"
)

const appSource = "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"

const appDiff = `--- a/internal/app.go
+++ b/internal/app.go
@@ -1,5 +1,5 @@
 package internal

 func run() {
-	println("hi")
+	println("hello")
 }
`

// checkout serves a repository and returns an open workspace on it.
func checkout(t *testing.T) (*fixtureRepo, *workspace) {
	t.Helper()
	repo := serveRepo(t, map[string]string{"internal/app.go": appSource, "other.go": "package main\n"}, nil)
	a := newAdapter(t, repo.srv.URL, testToken, t.TempDir())
	ws, err := a.Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return repo, ws.(*workspace)
}

func (w *workspace) git(t *testing.T, args ...string) string { return git(t, w.dir, args...) }

func spec() ports.CommitSpec {
	return ports.CommitSpec{
		Branch:  "auto-fix/go-s1234/0123456",
		File:    "internal/app.go",
		Diff:    appDiff,
		Subject: "fix(sonar): go:S1234 in internal/app.go",
		Trailers: []ports.Trailer{
			{Key: "Finding-Source", Value: "sonar"},
			{Key: "Finding-Rule", Value: "go:S1234"},
			{Key: "Triggered-By", Value: "healerd delivery d-1"},
		},
	}
}

func TestCommitPatchCreatesTheFixCommit(t *testing.T) {
	repo, ws := checkout(t)
	sha, err := ws.CommitPatch(context.Background(), spec())
	if err != nil {
		t.Fatalf("CommitPatch: %v", err)
	}

	if head := ws.git(t, "rev-parse", "HEAD"); head != sha {
		t.Errorf("returned %s but HEAD is %s", sha, head)
	}
	if branch := ws.git(t, "rev-parse", "--abbrev-ref", "HEAD"); branch != "auto-fix/go-s1234/0123456" {
		t.Errorf("branch = %q", branch)
	}
	if parent := ws.git(t, "rev-parse", "HEAD^"); parent != repo.sha {
		t.Errorf("the commit descends from %s, want the failing commit %s", parent, repo.sha)
	}
	if files := ws.git(t, "show", "--name-only", "--format=", "HEAD"); files != "internal/app.go" {
		t.Errorf("the commit changes %q", files)
	}
	wantMsg := "fix(sonar): go:S1234 in internal/app.go\n\nFinding-Source: sonar\nFinding-Rule: go:S1234\nTriggered-By: healerd delivery d-1"
	if msg := ws.git(t, "log", "-1", "--format=%B"); msg != wantMsg {
		t.Errorf("message =\n%s\nwant\n%s", msg, wantMsg)
	}
	if who := ws.git(t, "log", "-1", "--format=%an <%ae> | %cn <%ce>"); who != "ci-healer-bot <bot@example.com> | ci-healer-bot <bot@example.com>" {
		t.Errorf("identity = %q", who)
	}
	if got, err := ws.ReadFile("internal/app.go"); err != nil || !strings.Contains(string(got), `println("hello")`) {
		t.Errorf("file after the commit: %q, %v", got, err)
	}
	if status := ws.git(t, "status", "--porcelain"); status != "" {
		t.Errorf("the tree is dirty after the commit: %q", status)
	}
}

func TestCommitPatchIgnoresHooksAndSigning(t *testing.T) {
	_, ws := checkout(t)
	hook := filepath.Join(ws.dir, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { // #nosec G306 -- test hook
		t.Fatal(err)
	}
	if _, err := ws.CommitPatch(context.Background(), spec()); err != nil {
		t.Errorf("a repository hook influenced the commit: %v", err)
	}
}

func TestCommitPatchFailuresCommitNothing(t *testing.T) {
	stale := strings.Replace(appDiff, `-	println("hi")`, `-	println("not there")`, 1)
	otherFile := "--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-package main\n+package other\n"
	twoFiles := appDiff + otherFile

	tests := []struct {
		name    string
		mutate  func(*ports.CommitSpec)
		wantErr string
	}{
		{"diff does not apply", func(s *ports.CommitSpec) { s.Diff = stale }, "does not apply"},
		{"diff changes another file", func(s *ports.CommitSpec) { s.Diff = otherFile }, "other than"},
		{"diff changes two files", func(s *ports.CommitSpec) { s.Diff = twoFiles }, "other than"},
		{"diff for a file that does not exist", func(s *ports.CommitSpec) {
			s.Diff = strings.ReplaceAll(appDiff, "internal/app.go", "internal/missing.go")
		}, "does not apply"},
		{"empty diff", func(s *ports.CommitSpec) { s.Diff = "" }, "does not apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, ws := checkout(t)
			s := spec()
			tt.mutate(&s)
			if _, err := ws.CommitPatch(context.Background(), s); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if head := ws.git(t, "rev-parse", "HEAD"); head != repo.sha {
				t.Errorf("HEAD moved to %s", head)
			}
			if status := ws.git(t, "status", "--porcelain"); status != "" {
				t.Errorf("the failed patch left the tree dirty: %q", status)
			}
			if got, _ := ws.ReadFile("other.go"); string(got) != "package main\n" {
				t.Errorf("other.go was modified: %q", got)
			}
		})
	}
}

func TestCommitPatchRejectsBadSpecsBeforeTouchingAnything(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ports.CommitSpec)
	}{
		{"empty branch", func(s *ports.CommitSpec) { s.Branch = "" }},
		{"branch starting with a dash", func(s *ports.CommitSpec) { s.Branch = "-x" }},
		{"branch with dots", func(s *ports.CommitSpec) { s.Branch = "a..b" }},
		{"branch with a space", func(s *ports.CommitSpec) { s.Branch = "a b" }},
		{"branch ending in .lock", func(s *ports.CommitSpec) { s.Branch = "auto-fix/x.lock" }},
		{"empty file", func(s *ports.CommitSpec) { s.File = "" }},
		{"file escaping the tree", func(s *ports.CommitSpec) { s.File = "../x.go" }},
		{"absolute file", func(s *ports.CommitSpec) { s.File = "/etc/passwd" }},
		{"git metadata", func(s *ports.CommitSpec) { s.File = ".git/config" }},
		{"empty subject", func(s *ports.CommitSpec) { s.Subject = " \n " }},
		{"invalid trailer key", func(s *ports.CommitSpec) { s.Trailers = []ports.Trailer{{Key: "Bad Key", Value: "v"}} }},
		{"trailer key with a line break", func(s *ports.CommitSpec) { s.Trailers = []ports.Trailer{{Key: "A\nB", Value: "v"}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, ws := checkout(t)
			s := spec()
			tt.mutate(&s)
			if _, err := ws.CommitPatch(context.Background(), s); err == nil {
				t.Fatal("expected an error")
			}
			if head := ws.git(t, "rev-parse", "HEAD"); head != repo.sha {
				t.Error("HEAD moved")
			}
			if branches := ws.git(t, "for-each-ref", "refs/heads"); branches != "" {
				t.Errorf("a branch was created: %q", branches)
			}
		})
	}
}

func TestBranchExists(t *testing.T) {
	repo, ws := checkout(t)
	git(t, repo.bare, "branch", "auto-fix/go-s1234/0123456", repo.sha)
	git(t, repo.bare, "branch", "auto-fix/go-s1234/0123456-longer", repo.sha)

	tests := []struct {
		branch string
		want   bool
	}{
		{"auto-fix/go-s1234/0123456", true},
		{"auto-fix/go-s1234/abcdef0", false},
		{"auto-fix/go-s1234", false},      // a prefix of an existing branch
		{"auto-fix/go-s1234/0123", false}, // a prefix of an existing branch
		{"auto-fix/go-s1234/0123456-longer", true},
	}
	for _, tt := range tests {
		got, err := ws.BranchExists(context.Background(), tt.branch)
		if err != nil {
			t.Errorf("BranchExists(%q): %v", tt.branch, err)
			continue
		}
		if got != tt.want {
			t.Errorf("BranchExists(%q) = %v, want %v", tt.branch, got, tt.want)
		}
	}

	for _, bad := range []string{"", "-x", "a..b", "a b", "--upload-pack=x"} {
		if _, err := ws.BranchExists(context.Background(), bad); err == nil {
			t.Errorf("BranchExists(%q) accepted a bad name", bad)
		}
	}
}

func TestBranchExistsReportsRemoteFailuresWithoutTheToken(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "x\n"}, nil)
	ws, err := newAdapter(t, repo.srv.URL, testToken, t.TempDir()).Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	repo.srv.Close() // the remote disappears

	_, err = ws.BranchExists(context.Background(), "auto-fix/x/0123456")
	if err == nil {
		t.Fatal("expected an error: an unreachable remote must not read as 'branch does not exist'")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the error leaks the token: %v", err)
	}
}

func TestFormatCommitMessage(t *testing.T) {
	long := strings.Repeat("é", 500)
	tests := []struct {
		name     string
		subject  string
		trailers []ports.Trailer
		want     string
		wantErr  bool
	}{
		{"subject only", "fix: x", nil, "fix: x\n", false},
		{"subject and trailers", "fix: x", []ports.Trailer{{Key: "A", Value: "1"}, {Key: "B-C", Value: "2"}}, "fix: x\n\nA: 1\nB-C: 2\n", false},
		{"empty trailer values are dropped", "fix: x", []ports.Trailer{{Key: "A", Value: ""}, {Key: "B", Value: "  "}}, "fix: x\n", false},
		{"line breaks in the subject are flattened", "fix: x\n\nCo-Authored-By: evil", nil, "fix: x Co-Authored-By: evil\n", false},
		{"a trailer value cannot forge another trailer", "fix: x", []ports.Trailer{{Key: "Triggered-By", Value: "d1\nCo-Authored-By: evil <e@example.com>"}}, "fix: x\n\nTriggered-By: d1 Co-Authored-By: evil <e@example.com>\n", false},
		{"control characters are removed", "fix:\x00 x\x07", []ports.Trailer{{Key: "A", Value: "v\x1b[31m"}}, "fix: x\n\nA: v[31m\n", false},
		{"carriage returns are flattened", "fix: x\r\nCo-Authored-By: evil", nil, "fix: x Co-Authored-By: evil\n", false},
		{"empty subject", "  ", nil, "", true},
		{"key with a space", "fix: x", []ports.Trailer{{Key: "A B", Value: "1"}}, "", true},
		{"key with a colon", "fix: x", []ports.Trailer{{Key: "A:", Value: "1"}}, "", true},
		{"key starting with a digit", "fix: x", []ports.Trailer{{Key: "1A", Value: "1"}}, "", true},
		{"empty key", "fix: x", []ports.Trailer{{Key: "", Value: "1"}}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FormatCommitMessage(tt.subject, tt.trailers)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}
		})
	}

	got, _ := FormatCommitMessage(long, []ports.Trailer{{Key: "A", Value: long}})
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if n := len([]rune(line)); n > maxLineRunes+len("A: ") {
			t.Errorf("a line has %d runes", n)
		}
	}
}
