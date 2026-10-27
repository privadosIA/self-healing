package core

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const (
	// AutoFixLabel marks every pull request healerd opens.
	AutoFixLabel = "auto-fix"

	maxTitleRunes = 200
	maxCellRunes  = 300
	maxBlockBytes = 6000
)

// ReviewChecklistItem is left unchecked for a human to tick.
const ReviewChecklistItem = "- [ ] Reviewed by a human"

// FixReport is everything the pull request describes.
type FixReport struct {
	Event    model.WebhookEvent
	Finding  model.Finding
	Proposal ports.Proposal
	// Branch is the fix branch, the head of the pull request.
	Branch string
	// Model is the name of the model that wrote the patch.
	Model string
	// GiteaURL and SonarURL build links; SonarURL may be empty.
	GiteaURL, SonarURL string
}

// BuildPullRequest turns a fix into a pull request. Rule ids, messages, file
// names and the model's summary come from scanners and a model, so none of them
// is trusted: each is flattened to one line and shown as a code span, which
// stops mentions, links, HTML and headings, and the code blocks use a fence
// longer than any backtick run they contain.
func BuildPullRequest(r FixReport) ports.PullRequest {
	f := r.Finding
	labels := []string{AutoFixLabel}
	if src := slugLabel(f.Source); src != "" && src != AutoFixLabel {
		labels = append(labels, src)
	}
	return ports.PullRequest{
		Repo:   r.Event.RepoFullName,
		Title:  title(f.RuleID, r.Proposal.Summary),
		Body:   body(r),
		Head:   r.Branch,
		Base:   r.Event.Branch,
		Labels: labels,
	}
}

func title(rule, summary string) string {
	t := "[auto-fix] " + oneLine(rule) + ": " + oneLine(summary)
	if rs := []rune(t); len(rs) > maxTitleRunes {
		t = string(rs[:maxTitleRunes])
	}
	return t
}

func body(r FixReport) string {
	f := r.Finding
	before, after := beforeAfter(r.Proposal.Diff)

	var b strings.Builder
	b.WriteString("## Automated fix proposal\n\n")
	b.WriteString("This pull request was opened automatically by healerd. Nothing has been merged: please review the change before merging it.\n\n")

	b.WriteString("| | |\n|---|---|\n")
	row := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "| **%s** | %s |\n", name, value)
		}
	}
	row("Source", codeSpan(f.Source))
	row("Rule", codeSpan(f.RuleID))
	row("Severity", codeSpan(string(f.Severity)))
	row("Location", codeSpan(location(f)))
	row("Finding", codeSpan(f.Message))
	row("Model", codeSpan(r.Model))
	row("Confidence", codeSpan(fmt.Sprintf("%.2f", r.Proposal.Confidence)))
	row("Commit", codeSpan(shortSHA(r.Event.CommitSHA)))
	row("Pipeline job", codeSpan(r.Event.JobName))
	row("Links", links(r))
	b.WriteString("\n")

	b.WriteString("### Summary\n\n")
	b.WriteString(codeSpan(r.Proposal.Summary) + "\n\n")

	b.WriteString("### Before\n\n" + fenced(before) + "\n")
	b.WriteString("### After\n\n" + fenced(after) + "\n")

	b.WriteString("### Checklist\n\n" + ReviewChecklistItem + "\n")
	return b.String()
}

func location(f model.Finding) string {
	switch {
	case f.LineStart > 0:
		return fmt.Sprintf("%s:%d", f.File, f.LineStart)
	case f.Package != "":
		return fmt.Sprintf("%s (%s)", f.File, f.Package)
	default:
		return f.File
	}
}

var (
	repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cveRE  = regexp.MustCompile(`^CVE-\d{4}-\d{4,}$`)
)

// links builds the finding links from trusted base URLs and validated parts.
func links(r FixReport) string {
	var out []string
	f := r.Finding

	if r.GiteaURL != "" && repoRE.MatchString(r.Event.RepoFullName) && fullSHA.MatchString(r.Event.CommitSHA) && f.File != "" {
		parts := strings.Split(f.File, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		link := strings.TrimRight(r.GiteaURL, "/") + "/" + r.Event.RepoFullName + "/src/commit/" + r.Event.CommitSHA + "/" + strings.Join(parts, "/")
		if f.LineStart > 0 {
			link += fmt.Sprintf("#L%d", f.LineStart)
		}
		out = append(out, "[code]("+link+")")
	}
	switch f.Source {
	case model.SourceSonar:
		if r.SonarURL != "" && f.RuleID != "" {
			out = append(out, "[rule]("+strings.TrimRight(r.SonarURL, "/")+"/coding_rules?open="+url.QueryEscape(f.RuleID)+")")
		}
	case model.SourceTrivy:
		if cveRE.MatchString(f.RuleID) {
			out = append(out, "[advisory](https://avd.aquasec.com/nvd/"+strings.ToLower(f.RuleID)+")")
		}
	}
	return strings.Join(out, " · ")
}

var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// beforeAfter derives the old and new text of each hunk of a unified diff.
// Context lines appear on both sides; hunks are separated by a line with "...".
// The diff has already been validated, so everything after the first "@@" is
// hunk content.
func beforeAfter(diff string) (before, after string) {
	var b, a []string
	started := false
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		if strings.HasPrefix(line, "@@") {
			if started {
				b, a = append(b, "..."), append(a, "...")
			}
			started = true
			continue
		}
		if !started {
			continue
		}
		switch {
		case strings.HasPrefix(line, "\\"):
		case strings.HasPrefix(line, "-"):
			b = append(b, line[1:])
		case strings.HasPrefix(line, "+"):
			a = append(a, line[1:])
		case strings.HasPrefix(line, " "):
			b, a = append(b, line[1:]), append(a, line[1:])
		default: // a blank context line whose space was stripped
			b, a = append(b, line), append(a, line)
		}
	}
	return clip(strings.Join(b, "\n")), clip(strings.Join(a, "\n"))
}

func clip(s string) string {
	if len(s) <= maxBlockBytes {
		return s
	}
	cut := s[:maxBlockBytes]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1] // do not leave half of a character
	}
	return cut + "\n... (truncated)"
}

// fenced wraps content in a code fence that no line of the content can close.
func fenced(content string) string {
	fence := strings.Repeat("`", max(3, longestRun(content, '`')+1))
	return fence + "\n" + content + "\n" + fence + "\n"
}

// codeSpan renders s as inline code on one line, or "" when s is empty. The
// delimiter is longer than any backtick run inside, so s cannot break out.
func codeSpan(s string) string {
	s = oneLine(s)
	if s == "" {
		return ""
	}
	if rs := []rune(s); len(rs) > maxCellRunes {
		s = string(rs[:maxCellRunes])
	}
	delim := strings.Repeat("`", longestRun(s, '`')+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return delim + escapePipes(s) + delim
}

// escapePipes keeps a "|" inside a table cell from ending the cell.
func escapePipes(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func longestRun(s string, c rune) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == c {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// oneLine collapses whitespace and drops control characters.
func oneLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// slugLabel makes a label name from a scanner name, or "" if nothing is left.
func slugLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
