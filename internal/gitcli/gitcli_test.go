package gitcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// workTree returns a directory with a tracked-looking file main.go.
func workTree(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	src := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goodDiff = `--- a/main.go
+++ b/main.go
@@ -1,5 +1,5 @@
 package main

 func main() {
-	println("hi")
+	println("hello")
 }
`

func TestApplyCheck(t *testing.T) {
	dir := workTree(t)
	tests := []struct {
		name    string
		diff    string
		wantErr string // empty means success
	}{
		{"valid diff", goodDiff, ""},
		{"git-style headers", "diff --git a/main.go b/main.go\n" + goodDiff, ""},
		{"context does not match", strings.Replace(goodDiff, `-	println("hi")`, `-	println("bye")`, 1), "apply"},
		{"file does not exist", strings.ReplaceAll(goodDiff, "main.go", "other.go"), "apply"},
		{"not a diff", "just some prose\n", "apply"},
		{"empty diff", "", "empty diff"},
		{"whitespace only", "  \n\n", "empty diff"},
		{"path escapes the tree", strings.ReplaceAll(goodDiff, "main.go", "../main.go"), "apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyCheck(context.Background(), dir, tt.diff)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestApplyCheckDoesNotModifyTheTree(t *testing.T) {
	dir := workTree(t)
	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if err := ApplyCheck(context.Background(), dir, goodDiff); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(before) != string(after) {
		t.Error("--check changed the file")
	}
}

func TestApplyCheckIgnoresUserConfiguration(t *testing.T) {
	dir := workTree(t)
	// A hostile global config must have no effect; with it ignored this passes.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[apply]\n\twhitespace = error\n[core]\n\thooksPath = /nonexistent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := ApplyCheck(context.Background(), dir, goodDiff); err != nil {
		t.Errorf("user configuration influenced git: %v", err)
	}
}

func TestApplyCheckHonorsContext(t *testing.T) {
	dir := workTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyCheck(ctx, dir, goodDiff); err == nil {
		t.Error("expected an error for a cancelled context")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := ApplyCheck(ctx, dir, goodDiff); err == nil {
		t.Error("expected an error for an expired context")
	}
}

func TestStderrIsTruncated(t *testing.T) {
	dir := workTree(t)
	long := strings.Repeat("x", 5000)
	err := ApplyCheck(context.Background(), dir, strings.ReplaceAll(goodDiff, "main.go", long))
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > maxStderr+100 {
		t.Errorf("error has %d bytes", len(err.Error()))
	}
}

// origin builds a repository with one commit and returns its path and commit id.
func origin(t *testing.T) (dir, sha string) {
	t.Helper()
	requireGit(t)
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "first")
	return dir, git("rev-parse", "HEAD")
}

var fileOnly = Options{Protocols: "file"}

func TestCloneFetchesExactlyOneCommit(t *testing.T) {
	src, sha := origin(t)
	// A second commit so there is history that must NOT come along.
	if err := os.WriteFile(filepath.Join(src, "later.txt"), []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "second"}} {
		cmd := exec.Command("git", append([]string{"-C", src}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	dst := t.TempDir()
	if err := Clone(context.Background(), dst, "file://"+src, sha, fileOnly); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "main.go")); err != nil || string(b) != "package main\n" {
		t.Errorf("main.go = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "later.txt")); err == nil {
		t.Error("the working tree holds a file from a later commit")
	}
	out, err := run(context.Background(), dst, nil, fileOnly, "rev-list", "--count", "HEAD")
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Errorf("history has %q commits (%v), want a shallow 1", out, err)
	}
}

func TestCloneRejectsBadInput(t *testing.T) {
	src, sha := origin(t)
	tests := []struct {
		name string
		url  string
		sha  string
		opts Options
	}{
		{"abbreviated sha", "file://" + src, sha[:10], fileOnly},
		{"uppercase sha", "file://" + src, strings.ToUpper(sha), fileOnly},
		{"empty sha", "file://" + src, "", fileOnly},
		{"sha option injection", "file://" + src, "--upload-pack=touch /tmp/x", fileOnly},
		{"unknown commit", "file://" + src, strings.Repeat("0", 40), fileOnly},
		{"missing repository", "file://" + src + "-missing", sha, fileOnly},
		{"file protocol is refused by default", "file://" + src, sha, Options{}},
		{"ssh is refused", "ssh://git@example.invalid/x.git", sha, Options{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Clone(context.Background(), t.TempDir(), tt.url, tt.sha, tt.opts); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestTokenTravelsOnlyInTheEnvironment(t *testing.T) {
	const token = "changeme-bot-token"
	o := Options{Token: token}
	cmd := command(context.Background(), t.TempDir(), nil, o, "fetch", "origin", strings.Repeat("a", 40))

	for _, arg := range cmd.Args {
		if strings.Contains(arg, token) {
			t.Errorf("the token is in the command line: %q", arg)
		}
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "Authorization: token "+token) {
		t.Error("the token is not passed through the environment")
	}
	if strings.Contains(strings.Join(Options{}.environment(), "\n"), "Authorization") {
		t.Error("an anonymous run carries an authorization header")
	}
}

func TestEnvironmentIsLockedDown(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_WORK_TREE", "/elsewhere")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("https_proxy", "http://proxy.invalid:3128")
	t.Setenv("HOME", "/home/healer")
	env := strings.Join(Options{Token: "x"}.environment(), "\n")

	for _, banned := range []string{"/elsewhere", "proxy.invalid", "/home/healer"} {
		if strings.Contains(env, banned) {
			t.Errorf("ambient value %q leaked into git's environment", banned)
		}
	}
	for _, want := range []string{
		"GIT_ALLOW_PROTOCOL=http:https", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"=core.hooksPath", "=/dev/null", "=http.followRedirects", "=false", "=http.proxy", "=credential.helper",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %q", want)
		}
	}
}

func TestAmbientGitDirDoesNotRedirectGit(t *testing.T) {
	dir := workTree(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nope"))
	if err := ApplyCheck(context.Background(), dir, goodDiff); err != nil {
		t.Errorf("an ambient GIT_DIR broke or redirected git: %v", err)
	}
}

func TestErrorScrubsTheToken(t *testing.T) {
	const token = "changeme-bot-token"
	src, sha := origin(t)
	// The remote name is echoed by git; put the token there to prove scrubbing.
	err := Clone(context.Background(), t.TempDir(), "file://"+src+"/"+token, sha, Options{Protocols: "file", Token: token})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the error leaks the token: %v", err)
	}
	if got := scrub("fatal: "+token+" rejected", token); strings.Contains(got, token) || !strings.Contains(got, "[redacted]") {
		t.Errorf("scrub = %q", got)
	}
}
