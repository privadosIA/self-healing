package openai

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// gitApply applies diff to the file name in dir with git and returns the result.
func gitApply(t *testing.T, dir, name, diff string) string {
	t.Helper()
	cmd := exec.Command("git", "apply")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(diff)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\ndiff:\n%s", err, out, diff)
	}
	got, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestBuildDiffShape(t *testing.T) {
	content := "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"
	got, err := buildDiff("internal/app.go", content, 4, 4, "\tprintln(\"hello\")\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,5 +1,5 @@\n package internal\n \n func run() {\n-\tprintln(\"hi\")\n+\tprintln(\"hello\")\n }\n"
	if got != want {
		t.Errorf("diff =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildDiffTrimsWhatDoesNotChange(t *testing.T) {
	content := "a\nb\nc\nd\ne\nf\ng\nh\ni\n"
	// Lines 3-7 are given back with only "e" changed.
	got, err := buildDiff("f.txt", content, 3, 7, "c\nd\nE\nf\ng\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -2,7 +2,7 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n"
	if got != want {
		t.Errorf("diff =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildDiffAppliesAndProducesTheNewFile(t *testing.T) {
	requireGit(t)
	tests := []struct {
		name        string
		content     string
		first, last int
		replacement string
		want        string
	}{
		{"first line", "a\nb\nc\n", 1, 1, "A\n", "A\nb\nc\n"},
		{"middle line", "a\nb\nc\nd\ne\n", 3, 3, "C\n", "a\nb\nC\nd\ne\n"},
		{"last line", "a\nb\nc\n", 3, 3, "C\n", "a\nb\nC\n"},
		{"whole file", "a\nb\n", 1, 2, "x\ny\nz\n", "x\ny\nz\n"},
		{"longer replacement", "a\nb\nc\n", 2, 2, "b1\nb2\nb3\n", "a\nb1\nb2\nb3\nc\n"},
		{"shorter replacement", "a\nb\nc\nd\n", 2, 3, "bc\n", "a\nbc\nd\n"},
		{"deletion in the middle", "a\nb\nc\nd\n", 2, 3, "", "a\nd\n"},
		{"deletion of the first line", "a\nb\nc\n", 1, 1, "", "b\nc\n"},
		{"deletion of the last line", "a\nb\nc\n", 3, 3, "", "a\nb\n"},
		{"insertion by growing a line", "a\nb\n", 1, 1, "a\nnew\n", "a\nnew\nb\n"},
		{"insertion before a line", "a\nb\n", 2, 2, "new\nb\n", "a\nnew\nb\n"},
		{"replacement without trailing newline", "a\nb\nc\n", 2, 2, "B", "a\nB\nc\n"},
		{"blank line", "a\n\nc\n", 2, 2, "x\n", "a\nx\nc\n"},
		{"indentation by tabs", "f {\n\tx\n}\n", 2, 2, "\ty\n\tz\n", "f {\n\ty\n\tz\n}\n"},
		{"crlf file", "a\r\nb\r\nc\r\n", 2, 2, "B\n", "a\r\nB\r\nc\r\n"},
		{"crlf file, model sent crlf", "a\r\nb\r\nc\r\n", 2, 2, "B\r\n", "a\r\nB\r\nc\r\n"},
		{"crlf file, longer replacement", "a\r\nb\r\nc\r\n", 2, 2, "b1\nb2\n", "a\r\nb1\r\nb2\r\nc\r\n"},
		{"no final newline, change before the end", "a\nb\nc", 1, 1, "A\n", "A\nb\nc"},
		{"no final newline, change the last line", "a\nb\nc", 3, 3, "C", "a\nb\nC"},
		{"no final newline, change the last line, model added one", "a\nb\nc", 3, 3, "C\n", "a\nb\nC"},
		{"no final newline, grow the last line", "a\nb", 2, 2, "b\nc", "a\nb\nc"},
		{"no final newline, delete the last line", "a\nb\nc", 3, 3, "", "a\nb\n"},
		{"no final newline, range ends at the end", "a\nb\nc", 2, 3, "B\nC", "a\nB\nC"},
		{"no final newline, one line file", "a", 1, 1, "b", "b"},
		{"crlf and no final newline", "a\r\nb\r\nc", 2, 3, "B\nC", "a\r\nB\r\nC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := buildDiff("f.txt", tt.content, tt.first, tt.last, tt.replacement)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := gitApply(t, dir, "f.txt", diff); got != tt.want {
				t.Errorf("applying\n%s\ngives %q, want %q", diff, got, tt.want)
			}
		})
	}
}

func TestBuildDiffRejects(t *testing.T) {
	long := strings.Repeat("x\n", 250)
	tests := []struct {
		name        string
		content     string
		first, last int
		replacement string
	}{
		{"unchanged", "a\nb\nc\n", 2, 2, "b\n"},
		{"unchanged range", "a\nb\nc\n", 1, 3, "a\nb\nc\n"},
		{"unchanged but for a missing newline", "a\nb\nc", 3, 3, "c\n"},
		{"empties the file", "a\nb\n", 1, 2, ""},
		{"empties a one line file", "a\n", 1, 1, ""},
		{"range before the file", "a\n", 0, 1, "x\n"},
		{"range after the file", "a\nb\n", 2, 3, "x\n"},
		{"end before start", "a\nb\nc\n", 3, 2, "x\n"},
		{"empty file", "", 1, 1, "x\n"},
		{"over the line limit", "a\nb\nc\n", 2, 2, long},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := buildDiff("f.txt", tt.content, tt.first, tt.last, tt.replacement); err == nil {
				t.Errorf("no error, diff:\n%s", got)
			}
		})
	}
}

func TestBuildDiffLineLimitIsExact(t *testing.T) {
	// A diff of headers (3) + 1 context line before + n added lines + 1 removed
	// line + 1 context line after. Find the largest replacement that fits.
	content := "a\nb\nc\n"
	fits := func(n int) bool {
		_, err := buildDiff("f.txt", content, 2, 2, strings.Repeat("x\n", n))
		return err == nil
	}
	n := 1
	for fits(n + 1) {
		n++
	}
	if n < 150 || n > 200 {
		t.Fatalf("the largest replacement that fits is %d lines, expected one close to the limit", n)
	}
	if fits(n + 1) {
		t.Error("the limit is not a single threshold")
	}
}

func TestBuildDiffErrorsNeverRepeatTheReplacement(t *testing.T) {
	secret := "SECRET-TOKEN-" + strings.Repeat("y", 40)
	cases := []struct {
		content     string
		first, last int
		replacement string
	}{
		{"a\nb\n", 1, 2, ""},                               // empties the file
		{"a\nb\n", 5, 6, secret},                           // range outside
		{secret + "\n", 1, 1, secret},                      // unchanged
		{"a\nb\n", 1, 1, strings.Repeat(secret+"\n", 400)}, // too long
	}
	for _, c := range cases {
		_, err := buildDiff("f.txt", c.content, c.first, c.last, c.replacement)
		if err == nil {
			t.Fatalf("expected an error for %q", c.replacement[:min(len(c.replacement), 20)])
		}
		if strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("the error repeats the replacement or file text: %v", err)
		}
	}
}
