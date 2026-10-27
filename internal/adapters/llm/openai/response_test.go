package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/gitcli"
	"github.com/privadosIA/self-healing/internal/ports"
)

const goodDiff = `--- a/internal/app.go
+++ b/internal/app.go
@@ -1,5 +1,5 @@
 package internal

 func run() {
-	println("hi")
+	println("hello")
 }
`

var target = model.Finding{File: "internal/app.go", RuleID: "go:S1234", LineStart: 4, LineEnd: 4}

func respond(diff string, confidence float64, skip bool) string {
	b, _ := json.Marshal(map[string]any{"diff": diff, "summary": "Replace hi with hello", "confidence": confidence, "skip": skip, "skip_reason": ""})
	return string(b)
}

func TestParseResponseAccepts(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"plain object", respond(goodDiff, 0.9, false)},
		{"exactly the minimum confidence", respond(goodDiff, 0.6, false)},
		{"leading and trailing whitespace", "\n  " + respond(goodDiff, 0.9, false) + "\n"},
		{"wrapped in a json code fence", "```json\n" + respond(goodDiff, 0.9, false) + "\n```"},
		{"wrapped in a bare code fence", "```\n" + respond(goodDiff, 0.9, false) + "\n```\n"},
		{"git style headers", respond("diff --git a/internal/app.go b/internal/app.go\nindex 1111111..2222222 100644\n"+goodDiff, 0.9, false)},
		{"timestamps after the path", respond(strings.Replace(strings.Replace(goodDiff, "--- a/internal/app.go", "--- a/internal/app.go\t2026-01-01 00:00:00", 1), "+++ b/internal/app.go", "+++ b/internal/app.go\t2026-01-01 00:00:01", 1), 0.9, false)},
		{"two hunks", respond(`--- a/internal/app.go
+++ b/internal/app.go
@@ -1,3 +1,3 @@
 package internal
-
+// x
 func a() {}
@@ -10,2 +10,2 @@
-old
+new
 end
`, 0.9, false)},
		{"no newline marker", respond(strings.Replace(goodDiff, " }\n", " }\n\\ No newline at end of file\n", 1), 0.9, false)},
		{"stripped blank context line", respond(strings.Replace(goodDiff, " package internal\n \n", " package internal\n\n", 1), 0.9, false)},
		{"single-line hunk header", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -5 +5 @@\n-old\n+new\n", 0.9, false)},
		{"removed line that looks like a header", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,2 +1,1 @@\n--- a SQL comment\n keep\n", 0.9, false)},
		{"unknown extra fields are ignored", `{"diff":` + quote(goodDiff) + `,"summary":"s","confidence":0.9,"skip":false,"skip_reason":"","extra":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseResponse(tt.content, target)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Diff == "" || p.Summary == "" || p.Confidence < ports.MinConfidence {
				t.Errorf("proposal = %+v", p)
			}
		})
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestParseResponseRejects(t *testing.T) {
	long := "--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,300 +1,300 @@\n" + strings.Repeat("-x\n+y\n", 150)
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{"not json", "Sure! Here is the fix:", "not valid JSON"},
		{"empty", "", "not valid JSON"},
		{"json array", `[1,2]`, "not valid JSON"},
		{"trailing content", respond(goodDiff, 0.9, false) + " thanks!", "after the JSON"},
		{"two objects", respond(goodDiff, 0.9, false) + respond(goodDiff, 0.9, false), "after the JSON"},
		{"model skips", respond(goodDiff, 0.99, true), "declined"},
		{"confidence below the minimum", respond(goodDiff, 0.59, false), "below"},
		{"confidence zero", respond(goodDiff, 0, false), "below"},
		{"confidence above one", respond(goodDiff, 1.5, false), "out of range"},
		{"confidence negative", respond(goodDiff, -0.1, false), "out of range"},
		{"confidence missing", `{"diff":` + quote(goodDiff) + `,"summary":"s","skip":false}`, "missing"},
		{"empty diff", respond("", 0.9, false), "empty"},
		{"blank diff", respond("  \n", 0.9, false), "empty"},
		{"prose instead of a diff", respond("I changed the print call.", 0.9, false), "file headers"},
		{"another file", respond(strings.ReplaceAll(goodDiff, "internal/app.go", "internal/other.go"), 0.9, false), "finding's file"},
		{"path traversal", respond(strings.ReplaceAll(goodDiff, "internal/app.go", "../etc/passwd"), 0.9, false), "finding's file"},
		{"absolute path", respond(strings.ReplaceAll(goodDiff, "a/internal/app.go", "/internal/app.go"), 0.9, false), "finding's file"},
		{"missing a/ b/ prefixes", respond(strings.ReplaceAll(goodDiff, "a/internal", "internal"), 0.9, false), "finding's file"},
		{"old and new paths differ", respond(strings.Replace(goodDiff, "+++ b/internal/app.go", "+++ b/internal/moved.go", 1), 0.9, false), "finding's file"},
		{"git header names another file", respond("diff --git a/x.go b/x.go\n"+goodDiff, 0.9, false), "finding's file"},
		{"two files", respond(goodDiff+"--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-a\n+b\n", 0.9, false), "more than one file"},
		{"two files with git headers", respond(goodDiff+"diff --git a/other.go b/other.go\n--- a/other.go\n+++ b/other.go\n@@ -1 +1 @@\n-a\n+b\n", 0.9, false), "more than one file"},
		{"file creation", respond("--- /dev/null\n+++ b/internal/app.go\n@@ -0,0 +1 @@\n+x\n", 0.9, false), "finding's file"},
		{"file deletion", respond("--- a/internal/app.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n", 0.9, false), "finding's file"},
		{"rename", respond("diff --git a/internal/app.go b/internal/app.go\nsimilarity index 90%\nrename from internal/app.go\nrename to internal/new.go\n", 0.9, false), "file headers"},
		{"mode change", respond("diff --git a/internal/app.go b/internal/app.go\nold mode 100644\nnew mode 100755\n", 0.9, false), "file headers"},
		{"binary patch", respond("diff --git a/internal/app.go b/internal/app.go\nGIT binary patch\nliteral 0\n", 0.9, false), "file headers"},
		{"headers without a hunk", respond("--- a/internal/app.go\n+++ b/internal/app.go\n", 0.9, false), "no hunks"},
		{"hunk shorter than declared", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,5 +1,5 @@\n-a\n+b\n", 0.9, false), "shorter"},
		{"hunk longer than declared", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1 +1 @@\n-a\n+b\n+c\n", 0.9, false), "unexpected content"},
		{"malformed line in a hunk", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,2 +1,2 @@\n-a\n*b\n", 0.9, false), "malformed"},
		{"trailing prose after the diff", respond(goodDiff+"Hope that helps!\n", 0.9, false), "unexpected content"},
		{"context only", respond("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1 +1 @@\n a\n", 0.9, false), "changes nothing"},
		{"more than 200 lines", respond(long, 0.9, false), "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseResponse(tt.content, target)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !errors.Is(err, ports.ErrNoProposal) {
				t.Errorf("err = %v, want it to wrap ErrNoProposal", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}

func TestDiffLineLimitIsExact(t *testing.T) {
	body := func(pairs int) string {
		return fmt.Sprintf("--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,%d +1,%d @@\n", pairs, pairs) + strings.Repeat(" c\n", pairs-1) + "-z\n+w\n"
	}
	// 3 header lines + (n-1) context + 2 changes: n = 196 gives exactly 200 lines.
	atLimit, over := body(196), body(197)
	if n := len(strings.Split(strings.TrimSuffix(atLimit, "\n"), "\n")); n != ports.MaxDiffLines {
		t.Fatalf("test diff has %d lines, want %d", n, ports.MaxDiffLines)
	}
	if _, err := ParseResponse(respond(atLimit, 0.9, false), target); err != nil {
		t.Errorf("a diff at the limit was rejected: %v", err)
	}
	if _, err := ParseResponse(respond(over, 0.9, false), target); !errors.Is(err, ports.ErrNoProposal) {
		t.Errorf("a diff over the limit was accepted: %v", err)
	}
}

func TestErrorsNeverRepeatModelOutput(t *testing.T) {
	marker := "SECRET-FROM-THE-MODEL-OR-REPO"
	for _, content := range []string{
		marker,
		respond(strings.ReplaceAll(goodDiff, "internal/app.go", marker), 0.9, false),
		respond(goodDiff+marker+"\n", 0.9, false),
		`{"diff":"x","summary":"` + marker + `","confidence":0.1,"skip":false,"skip_reason":"` + marker + `"}`,
		respond(goodDiff, 0.9, true),
	} {
		_, err := ParseResponse(content, target)
		if err == nil {
			t.Fatalf("expected an error for %q", content)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("error repeats the model output: %v", err)
		}
	}
}

func TestSummaryIsSanitized(t *testing.T) {
	long := strings.Repeat("é", 500)
	tests := []struct{ in, want string }{
		{"Replace hi with hello", "Replace hi with hello"},
		{"  spaced \t out \n\n text ", "spaced out text"},
		{"line one\nline two", "line one line two"},
		{"bell\x07 and\x00nul", "bell andnul"},
		{"", "fix go:S1234"},
		{"  \n ", "fix go:S1234"},
		{long, strings.Repeat("é", maxSummaryRunes)},
	}
	for _, tt := range tests {
		content := `{"diff":` + quote(goodDiff) + `,"summary":` + quote(tt.in) + `,"confidence":0.9,"skip":false,"skip_reason":""}`
		p, err := ParseResponse(content, target)
		if err != nil {
			t.Fatal(err)
		}
		if p.Summary != tt.want {
			t.Errorf("summary(%q) = %q, want %q", tt.in, p.Summary, tt.want)
		}
	}
}

// TestAcceptedDiffAppliesToARealTree checks the full chain the daemon relies
// on: a response that passes ParseResponse is also accepted by git apply
// --check, and one that passes the shape checks but not the content is not.
func TestAcceptedDiffAppliesToARealTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "app.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := ParseResponse(respond(goodDiff, 0.9, false), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := gitcli.ApplyCheck(context.Background(), dir, p.Diff); err != nil {
		t.Errorf("a valid proposal does not apply: %v", err)
	}

	stale := strings.Replace(goodDiff, `-	println("hi")`, `-	println("not there")`, 1)
	p, err = ParseResponse(respond(stale, 0.9, false), target)
	if err != nil {
		t.Fatalf("the shape of a stale diff is valid: %v", err)
	}
	if err := gitcli.ApplyCheck(context.Background(), dir, p.Diff); err == nil {
		t.Error("a diff against content that is not in the file must not apply")
	}
}

// fixSource is the file the Propose tests ask to fix.
const fixSource = "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"

func proposeRequest() ports.FixRequest {
	return ports.FixRequest{Finding: target, FileContent: fixSource}
}

func TestProposeEndToEnd(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler(respond(goodDiff, 0.85, false)))
	c := newClient(t, srv.URL+"/v1", testKey, 2*time.Second)

	p, err := c.Propose(context.Background(), proposeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if p.Diff != goodDiff || p.Summary != "Replace hi with hello" || p.Confidence != 0.85 || p.TokensIn != 812 || p.TokensOut != 96 {
		t.Errorf("proposal = %+v", p)
	}
	// The model received the fixed system prompt and the bounded user message.
	if len(cap.body.Messages) != 2 || cap.body.Messages[0].Content != SystemPrompt {
		t.Fatalf("messages = %+v", cap.body.Messages)
	}
	var m userMessage
	if err := json.Unmarshal([]byte(cap.body.Messages[1].Content), &m); err != nil || m.FilePath != "internal/app.go" || m.Snippet.StartLine != 1 {
		t.Errorf("user message = %+v, %v", m, err)
	}
}

func TestProposeMapsUnusableAnswersToErrNoProposal(t *testing.T) {
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		req     func() ports.FixRequest
	}{
		{"model skips", okHandler(respond(goodDiff, 0.9, true)), proposeRequest},
		{"low confidence", okHandler(respond(goodDiff, 0.2, false)), proposeRequest},
		{"not json", okHandler("sorry, no"), proposeRequest},
		{"response cut by the token limit", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{"},"finish_reason":"length"}]}`))
		}, proposeRequest},
		{"finding not in the file", okHandler(respond(goodDiff, 0.9, false)), func() ports.FixRequest {
			r := proposeRequest()
			r.Finding.LineStart, r.Finding.LineEnd = 99, 99
			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cap captured
			srv := newServer(t, &cap, tt.handler)
			_, err := newClient(t, srv.URL+"/v1", "", 2*time.Second).Propose(context.Background(), tt.req())
			if !errors.Is(err, ports.ErrNoProposal) {
				t.Errorf("err = %v, want it to wrap ErrNoProposal", err)
			}
		})
	}
}

func TestProposeDoesNotCallTheModelForAnUnlocatableFinding(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler(respond(goodDiff, 0.9, false)))
	req := proposeRequest()
	req.Finding.LineStart, req.Finding.LineEnd = 99, 99
	_, _ = newClient(t, srv.URL+"/v1", "", 2*time.Second).Propose(context.Background(), req)
	if cap.calls != 0 {
		t.Errorf("the model was called %d times", cap.calls)
	}
}

func TestProposeCallFailuresAreNotNoProposal(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	_, err := newClient(t, srv.URL+"/v1", "", 2*time.Second).Propose(context.Background(), proposeRequest())
	if err == nil || errors.Is(err, ports.ErrNoProposal) {
		t.Errorf("err = %v: a failing call must be a plain error so the job is failed, not skipped", err)
	}
}
