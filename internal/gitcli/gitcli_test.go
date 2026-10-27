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
