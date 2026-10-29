package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/gitcli"
	"github.com/privadosIA/self-healing/internal/ports"
)

// fixSource is the file the tests ask to fix; the finding is on line 4.
const fixSource = "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"

// goodReplacement replaces line 4 of fixSource.
const goodReplacement = "\tprintln(\"hello\")\n"

// goodDiff is the diff healerd builds for goodReplacement.
const goodDiff = "--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,5 +1,5 @@\n package internal\n \n func run() {\n-\tprintln(\"hi\")\n+\tprintln(\"hello\")\n }\n"

var (
	target = model.Finding{File: "internal/app.go", RuleID: "go:S1234", LineStart: 4, LineEnd: 4}
	// shown is the whole of fixSource, as the model sees it.
	shown = Range{First: 1, Last: 5}
)

func reply(start, end int, replacement string, confidence float64, skip bool) string {
	b, _ := json.Marshal(map[string]any{
		"start_line": start, "end_line": end, "replacement": replacement,
		"summary": "Replace hi with hello", "confidence": confidence, "skip": skip, "skip_reason": "",
	})
	return string(b)
}

func good(confidence float64) string { return reply(4, 4, goodReplacement, confidence, false) }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func parse(content string) (ports.Proposal, error) {
	return ParseResponse(content, target, fixSource, shown)
}

func TestParseResponseAccepts(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantDiff string // empty means goodDiff
	}{
		{"plain object", good(0.9), ""},
		{"exactly the minimum confidence", good(0.6), ""},
		{"leading and trailing whitespace", "\n  " + good(0.9) + "\n", ""},
		{"wrapped in a json code fence", "```json\n" + good(0.9) + "\n```", ""},
		{"wrapped in a bare code fence", "```\n" + good(0.9) + "\n```\n", ""},
		{"replacement without a trailing newline", reply(4, 4, strings.TrimSuffix(goodReplacement, "\n"), 0.9, false), ""},
		{"unchanged lines are given back too", reply(3, 5, "func run() {\n\tprintln(\"hello\")\n}\n", 0.9, false), ""},
		{"unknown extra fields are ignored", `{"start_line":4,"end_line":4,"replacement":` + quote(goodReplacement) + `,"summary":"s","confidence":0.9,"skip":false,"skip_reason":"","extra":1}`, ""},
		{"an empty replacement deletes the lines", reply(4, 4, "", 0.9, false),
			"--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,5 +1,4 @@\n package internal\n \n func run() {\n-\tprintln(\"hi\")\n }\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parse(tt.content)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := tt.wantDiff
			if want == "" {
				want = goodDiff
			}
			if p.Diff != want {
				t.Errorf("diff =\n%q\nwant\n%q", p.Diff, want)
			}
			if p.Summary == "" || p.Confidence < ports.MinConfidence {
				t.Errorf("proposal = %+v", p)
			}
		})
	}
}

func TestParseResponseRejects(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{"not json", "Sure! Here is the fix:", "not valid JSON"},
		{"empty", "", "not valid JSON"},
		{"json array", `[1,2]`, "not valid JSON"},
		{"trailing content", good(0.9) + " thanks!", "after the JSON"},
		{"two objects", good(0.9) + good(0.9), "after the JSON"},
		{"model skips", reply(4, 4, goodReplacement, 0.99, true), "declined"},
		{"confidence below the minimum", good(0.59), "below"},
		{"confidence zero", good(0), "below"},
		{"confidence above one", good(1.5), "out of range"},
		{"confidence negative", good(-0.1), "out of range"},
		{"confidence missing", `{"start_line":4,"end_line":4,"replacement":` + quote(goodReplacement) + `,"summary":"s","skip":false}`, "missing"},
		{"the old format with a diff", `{"diff":` + quote(goodDiff) + `,"summary":"s","confidence":0.9,"skip":false,"skip_reason":""}`, "does not give the lines"},
		{"start_line missing", `{"end_line":4,"replacement":"x","summary":"s","confidence":0.9}`, "does not give the lines"},
		{"end_line missing", `{"start_line":4,"replacement":"x","summary":"s","confidence":0.9}`, "does not give the lines"},
		{"replacement missing", `{"start_line":4,"end_line":4,"summary":"s","confidence":0.9}`, "does not give the lines"},
		{"replacement null", `{"start_line":4,"end_line":4,"replacement":null,"summary":"s","confidence":0.9}`, "does not give the lines"},
		{"line number with a fraction", `{"start_line":4.5,"end_line":4,"replacement":"x","summary":"s","confidence":0.9}`, "not valid JSON"},
		{"line number as text", `{"start_line":"4","end_line":4,"replacement":"x","summary":"s","confidence":0.9}`, "not valid JSON"},
		{"replacement is not text", `{"start_line":4,"end_line":4,"replacement":["x"],"summary":"s","confidence":0.9}`, "not valid JSON"},
		{"end before start", reply(5, 4, goodReplacement, 0.9, false), "outside the lines"},
		{"start before the snippet", reply(0, 4, goodReplacement, 0.9, false), "outside the lines"},
		{"negative start", reply(-3, 4, goodReplacement, 0.9, false), "outside the lines"},
		{"end after the snippet", reply(4, 6, goodReplacement, 0.9, false), "outside the lines"},
		{"far outside the file", reply(900, 901, goodReplacement, 0.9, false), "outside the lines"},
		{"nothing changes", reply(4, 4, "\tprintln(\"hi\")\n", 0.9, false), "changes nothing"},
		{"the whole file is deleted", reply(1, 5, "", 0.9, false), "empty"},
		{"too long", reply(4, 4, strings.Repeat("x\n", 300), 0.9, false), "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(tt.content)
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

// TestRangeIsLimitedToWhatTheModelWasShown uses a narrower snippet than the
// file: lines outside it exist, but were never shown, so they cannot change.
func TestRangeIsLimitedToWhatTheModelWasShown(t *testing.T) {
	file := "a\nb\nc\nd\ne\nf\ng\n"
	narrow := Range{First: 3, Last: 5}
	f := model.Finding{File: "x.txt", RuleID: "r", LineStart: 4}
	for _, tt := range []struct {
		start, end int
		ok         bool
	}{{3, 5, true}, {4, 4, true}, {2, 4, false}, {4, 6, false}, {1, 7, false}} {
		_, err := ParseResponse(reply(tt.start, tt.end, "X\n", 0.9, false), f, file, narrow)
		if (err == nil) != tt.ok {
			t.Errorf("range %d-%d: err = %v, want ok = %v", tt.start, tt.end, err, tt.ok)
		}
	}
}

func TestDiffLineLimitIsExactThroughParseResponse(t *testing.T) {
	fits := func(n int) bool {
		_, err := parse(reply(4, 4, strings.Repeat("x\n", n), 0.9, false))
		return err == nil
	}
	n := 1
	for fits(n + 1) {
		n++
	}
	if n < 150 || n > 200 {
		t.Fatalf("the largest replacement that fits is %d lines", n)
	}
	if _, err := parse(reply(4, 4, strings.Repeat("x\n", n+1), 0.9, false)); !errors.Is(err, ports.ErrNoProposal) {
		t.Errorf("a diff over the limit was accepted: %v", err)
	}
}

func TestErrorsNeverRepeatModelOutput(t *testing.T) {
	marker := "SECRET-FROM-THE-MODEL-OR-REPO"
	for _, content := range []string{
		marker,
		reply(4, 4, marker, 0.9, true),
		reply(900, 901, marker, 0.9, false),
		reply(1, 5, "", 0.9, false),
		reply(4, 4, strings.Repeat(marker+"\n", 300), 0.9, false),
		`{"start_line":4,"end_line":4,"replacement":"x","summary":"` + marker + `","confidence":0.1,"skip":false,"skip_reason":"` + marker + `"}`,
		`{"diff":"` + marker + `","summary":"s","confidence":0.9}`,
	} {
		_, err := parse(content)
		if err == nil {
			t.Fatalf("expected an error for %.40q", content)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("error repeats the model output: %v", err)
		}
	}
	// The file's own text is just as untrusted as the model's.
	_, err := ParseResponse(reply(1, 1, marker+"\n", 0.9, false), target, marker+"\n", Range{1, 1})
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("err = %v: an unchanged line must be refused without repeating it", err)
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
		content := `{"start_line":4,"end_line":4,"replacement":` + quote(goodReplacement) + `,"summary":` + quote(tt.in) + `,"confidence":0.9,"skip":false,"skip_reason":""}`
		p, err := parse(content)
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
// --check, and one built from a file that is not the one in the tree is not.
func TestAcceptedDiffAppliesToARealTree(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "app.go"), []byte(fixSource), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := parse(good(0.9))
	if err != nil {
		t.Fatal(err)
	}
	if err := gitcli.ApplyCheck(context.Background(), dir, p.Diff); err != nil {
		t.Errorf("a valid proposal does not apply: %v", err)
	}

	changed := strings.Replace(fixSource, `println("hi")`, `println("changed since")`, 1)
	if err := os.WriteFile(filepath.Join(dir, "internal", "app.go"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitcli.ApplyCheck(context.Background(), dir, p.Diff); err == nil {
		t.Error("a diff built from different content than the tree's must not apply")
	}
}

func proposeRequest() ports.FixRequest {
	return ports.FixRequest{Finding: target, FileContent: fixSource}
}

func TestProposeEndToEnd(t *testing.T) {
	var cap captured
	srv := newServer(t, &cap, okHandler(good(0.85)))
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
		{"model skips", okHandler(reply(4, 4, goodReplacement, 0.9, true)), proposeRequest},
		{"low confidence", okHandler(good(0.2)), proposeRequest},
		{"not json", okHandler("sorry, no"), proposeRequest},
		{"response cut by the token limit", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{"},"finish_reason":"length"}]}`))
		}, proposeRequest},
		{"finding not in the file", okHandler(good(0.9)), func() ports.FixRequest {
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
	srv := newServer(t, &cap, okHandler(good(0.9)))
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
