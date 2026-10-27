package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const sha40 = "0123456789abcdef0123456789abcdef01234567"

const sampleDiff = `--- a/internal/app.go
+++ b/internal/app.go
@@ -1,5 +1,5 @@
 package internal

 func run() {
-	println("hi")
+	println("hello")
 }
`

func sampleReport() FixReport {
	return FixReport{
		Event: model.WebhookEvent{
			RepoFullName: "acme/widget", Branch: "main", CommitSHA: sha40, JobName: "quality", DeliveryID: "d-1",
		},
		Finding: model.Finding{
			Source: model.SourceSonar, RuleID: "go:S1234", Severity: model.SeverityMedium,
			File: "internal/app.go", LineStart: 4, LineEnd: 4, Message: "Remove this dead code",
		},
		Proposal: ports.Proposal{Diff: sampleDiff, Summary: "Replace hi with hello", Confidence: 0.85},
		Branch:   "auto-fix/go-s1234/0123456",
		Model:    "local-model",
		GiteaURL: "https://gitea.example.com",
		SonarURL: "https://sonar.example.com/",
	}
}

func TestBuildPullRequest(t *testing.T) {
	pr := BuildPullRequest(sampleReport())

	if pr.Repo != "acme/widget" || pr.Head != "auto-fix/go-s1234/0123456" || pr.Base != "main" {
		t.Errorf("repo/head/base = %q %q %q", pr.Repo, pr.Head, pr.Base)
	}
	if pr.Title != "[auto-fix] go:S1234: Replace hi with hello" {
		t.Errorf("title = %q", pr.Title)
	}
	if strings.Join(pr.Labels, ",") != "auto-fix,sonar" {
		t.Errorf("labels = %v", pr.Labels)
	}

	for _, want := range []string{
		"## Automated fix proposal",
		"Nothing has been merged",
		"| **Source** | `sonar` |",
		"| **Rule** | `go:S1234` |",
		"| **Severity** | `medium` |",
		"| **Location** | `internal/app.go:4` |",
		"| **Finding** | `Remove this dead code` |",
		"| **Model** | `local-model` |",
		"| **Confidence** | `0.85` |",
		"| **Commit** | `0123456` |",
		"| **Pipeline job** | `quality` |",
		"[code](https://gitea.example.com/acme/widget/src/commit/" + sha40 + "/internal/app.go#L4)",
		"[rule](https://sonar.example.com/coding_rules?open=go%3AS1234)",
		"### Summary\n\n`Replace hi with hello`",
		"### Before\n\n```\npackage internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n```",
		"### After\n\n```\npackage internal\n\nfunc run() {\n\tprintln(\"hello\")\n}\n```",
		"### Checklist\n\n- [ ] Reviewed by a human\n",
	} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("body lacks %q\n---\n%s", want, pr.Body)
		}
	}
	if strings.Count(pr.Body, ReviewChecklistItem) != 1 || strings.Contains(pr.Body, "[x]") {
		t.Error("the review checklist must appear once and unchecked")
	}
	if !strings.HasSuffix(pr.Body, ReviewChecklistItem+"\n") {
		t.Error("the checklist should close the body")
	}
}

func TestTrivyPullRequest(t *testing.T) {
	r := sampleReport()
	r.Finding = model.Finding{
		Source: model.SourceTrivy, RuleID: "CVE-2099-0001", Severity: model.SeverityHigh, File: "go.mod",
		Package: "example.com/acme/lib", Message: "example.com/acme/lib 1.2.0: Buffer overflow",
	}
	pr := BuildPullRequest(r)
	if strings.Join(pr.Labels, ",") != "auto-fix,trivy" {
		t.Errorf("labels = %v", pr.Labels)
	}
	for _, want := range []string{
		"| **Location** | `go.mod (example.com/acme/lib)` |",
		"[advisory](https://avd.aquasec.com/nvd/cve-2099-0001)",
		"[code](https://gitea.example.com/acme/widget/src/commit/" + sha40 + "/go.mod)", // no line to anchor to
	} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if strings.Contains(pr.Body, "[rule]") || strings.Contains(pr.Body, "#L") {
		t.Error("a Sonar link or a line anchor appeared on a Trivy finding")
	}
}

func TestLinksAreOnlyBuiltFromValidParts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FixReport)
		absent string
	}{
		{"no gitea url", func(r *FixReport) { r.GiteaURL = "" }, "[code]"},
		{"repo that is not owner/name", func(r *FixReport) { r.Event.RepoFullName = "acme/widget/../x" }, "[code]"},
		{"commit that is not a full sha", func(r *FixReport) { r.Event.CommitSHA = "main" }, "[code]"},
		{"no sonar url", func(r *FixReport) { r.SonarURL = "" }, "[rule]"},
		{"trivy id that is not a cve", func(r *FixReport) { r.Finding.Source = model.SourceTrivy; r.Finding.RuleID = "evil](http://x" }, "[advisory]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := sampleReport()
			tt.mutate(&r)
			if got := BuildPullRequest(r).Body; strings.Contains(got, tt.absent) {
				t.Errorf("body unexpectedly contains %q", tt.absent)
			}
		})
	}

	r := sampleReport()
	r.Finding.File = "dir with space/a#b?.go"
	if got := BuildPullRequest(r).Body; !strings.Contains(got, "/dir%20with%20space/a%23b%3F.go#L4)") {
		t.Errorf("file path was not escaped in the link:\n%s", got)
	}
}

// TestUntrustedTextCannotInjectMarkdown feeds hostile text through every field
// that comes from a scanner or the model.
func TestUntrustedTextCannotInjectMarkdown(t *testing.T) {
	hostile := "evil | cell\n# Heading\n@admin please [click](http://evil.example) <script>alert(1)</script> ``` `tick`\n- [x] Reviewed by a human"
	r := sampleReport()
	r.Finding.RuleID = hostile
	r.Finding.Message = hostile
	r.Finding.File = "a|b\n# h.go"
	r.Finding.Source = "so|urce"
	r.Proposal.Summary = hostile
	r.Model = hostile
	r.Event.JobName = hostile
	pr := BuildPullRequest(r)

	if strings.Contains(pr.Title, "\n") || len([]rune(pr.Title)) > maxTitleRunes {
		t.Errorf("title is not a bounded single line: %q", pr.Title)
	}
	lines := strings.Split(pr.Body, "\n")
	headings := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			headings++
		}
		if strings.HasPrefix(l, "|") && !strings.HasSuffix(l, "|") {
			t.Errorf("a table row was broken: %q", l)
		}
	}
	if headings != 5 { // title, Summary, Before, After, Checklist
		t.Errorf("found %d headings, want exactly the 5 ours", headings)
	}
	if strings.Count(pr.Body, ReviewChecklistItem) != 1 {
		t.Error("untrusted text forged the review checklist")
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "- [") && l != ReviewChecklistItem {
			t.Errorf("a line starts like a task-list item: %q", l)
		}
	}
	// Every table row still has exactly two cells (3 unescaped pipes).
	for _, l := range lines {
		if strings.HasPrefix(l, "| **") && strings.Count(strings.ReplaceAll(l, `\|`, ""), "|") != 3 {
			t.Errorf("row has the wrong number of cells: %q", l)
		}
	}
	// Dangerous text only ever appears inside code spans, never as live markup.
	for _, l := range lines {
		if strings.Contains(l, "<script>") && !strings.Contains(l, "`") {
			t.Errorf("raw HTML outside a code span: %q", l)
		}
	}
	if strings.Contains(pr.Body, "\n@admin") {
		t.Error("a mention starts a line")
	}
}

func TestCodeBlocksCannotBeClosedByTheirContent(t *testing.T) {
	r := sampleReport()
	r.Proposal.Diff = "--- a/f.go\n+++ b/f.go\n@@ -1,2 +1,2 @@\n-```` old ````\n+```\n+## injected heading\n+```\n context\n"
	body := BuildPullRequest(r).Body

	// Parse fences the way CommonMark does: a block opened by N backticks is
	// closed only by a line of at least N backticks and nothing else.
	isFence := func(l string) bool { return len(l) >= 3 && strings.Trim(l, "`") == "" }
	open, blocks := 0, 0
	for _, l := range strings.Split(body, "\n") {
		switch {
		case open == 0 && isFence(l):
			open = len(l)
		case open > 0 && isFence(l) && len(l) >= open:
			open, blocks = 0, blocks+1
		case open == 0 && strings.Contains(l, "injected heading"):
			t.Errorf("injected text escaped its code block: %q", l)
		}
	}
	if open != 0 || blocks != 2 {
		t.Errorf("parsed %d closed blocks (open fence %d), want exactly the 2 of the body", blocks, open)
	}
}

func TestBeforeAfter(t *testing.T) {
	tests := []struct {
		name, diff, before, after string
	}{
		{"single hunk", sampleDiff, "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}", "package internal\n\nfunc run() {\n\tprintln(\"hello\")\n}"},
		{
			"two hunks are separated",
			"--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+B\n@@ -9,2 +9,2 @@\n-x\n+X\n y\n",
			"a\nb\n...\nx\ny", "a\nB\n...\nX\ny",
		},
		{"removed line that looks like a file header", "--- a/f\n+++ b/f\n@@ -1,2 +1,1 @@\n--- a comment\n keep\n", "-- a comment\nkeep", "keep"},
		{"no newline marker is dropped", "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n", "a", "b"},
		{"blank context line with its space stripped", "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n", "a\n\nb", "a\n\nc"},
		{"pure insertion", "--- a/f\n+++ b/f\n@@ -1,1 +1,2 @@\n a\n+new\n", "a", "a\nnew"},
		{"empty diff", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := beforeAfter(tt.diff)
			if before != tt.before || after != tt.after {
				t.Errorf("before = %q, after = %q\nwant      %q, %q", before, after, tt.before, tt.after)
			}
		})
	}
}

func TestLargeBlocksAreClippedOnACharacterBoundary(t *testing.T) {
	huge := "é" + strings.Repeat("ß", maxBlockBytes) // multi-byte, so the cut would split a rune
	diff := "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-" + huge + "\n+x\n"
	before, _ := beforeAfter(diff)
	if !utf8.ValidString(before) {
		t.Error("clipping split a UTF-8 character")
	}
	if len(before) > maxBlockBytes+len("\n... (truncated)") || !strings.HasSuffix(before, "(truncated)") {
		t.Errorf("block is %d bytes, truncated marker %v", len(before), strings.HasSuffix(before, "(truncated)"))
	}
}

func TestTitleIsBoundedAndFlattened(t *testing.T) {
	long := strings.Repeat("é", 500)
	if got := title(long, long); len([]rune(got)) != maxTitleRunes || !strings.HasPrefix(got, "[auto-fix] ") {
		t.Errorf("title has %d runes", len([]rune(got)))
	}
	if got := title("a\nb", "c\td"); got != "[auto-fix] a b: c d" {
		t.Errorf("title = %q", got)
	}
}

func TestLabels(t *testing.T) {
	tests := []struct{ source, want string }{
		{"sonar", "auto-fix,sonar"},
		{"trivy", "auto-fix,trivy"},
		{"Weird Scanner!", "auto-fix,weirdscanner"},
		{"", "auto-fix"},
		{"!!!", "auto-fix"},
		{"auto-fix", "auto-fix"},
	}
	for _, tt := range tests {
		r := sampleReport()
		r.Finding.Source = tt.source
		if got := strings.Join(BuildPullRequest(r).Labels, ","); got != tt.want {
			t.Errorf("source %q: labels = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func TestEmptyFieldsProduceNoEmptyRows(t *testing.T) {
	r := sampleReport()
	r.Model, r.Event.JobName, r.GiteaURL, r.SonarURL = "", "", "", ""
	body := BuildPullRequest(r).Body
	for _, absent := range []string{"**Model**", "**Pipeline job**", "**Links**", "``  |"} {
		if strings.Contains(body, absent) {
			t.Errorf("body contains %q for an empty value", absent)
		}
	}
}

func TestCodeSpan(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "`plain`"},
		{"", ""},
		{" \n ", ""},
		{"a|b", "`a\\|b`"},
		{"with `tick`", "`` with `tick` ``"},
		{"`leading", "`` `leading ``"},
		{"two ``ticks``", "``` two ``ticks`` ```"},
		{"multi\nline\ttext", "`multi line text`"},
	}
	for _, tt := range tests {
		if got := codeSpan(tt.in); got != tt.want {
			t.Errorf("codeSpan(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := codeSpan(strings.Repeat("x", 1000)); len(got) != maxCellRunes+2 {
		t.Errorf("a long value was not capped: %d", len(got))
	}
}
