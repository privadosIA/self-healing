package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"unicode"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const maxSummaryRunes = 120

var _ ports.LLM = (*Client)(nil)

// Propose builds the prompt for req, calls the model and validates the answer.
// Anything unusable comes back wrapping ports.ErrNoProposal.
func (c *Client) Propose(ctx context.Context, req ports.FixRequest) (ports.Proposal, error) {
	msg, err := BuildUserMessage(PromptInput{
		Finding: req.Finding, FileContent: req.FileContent, ContextLines: req.ContextLines, BuildCmd: req.BuildCmd,
	})
	if err != nil {
		if errors.Is(err, ErrNotLocated) || errors.Is(err, ErrSnippetTooLarge) {
			return ports.Proposal{}, fmt.Errorf("%w: %v", ports.ErrNoProposal, err)
		}
		return ports.Proposal{}, err
	}
	done, err := c.Chat(ctx, SystemPrompt, msg)
	if err != nil {
		if errors.Is(err, ErrTruncated) {
			return ports.Proposal{}, fmt.Errorf("%w: %v", ports.ErrNoProposal, err)
		}
		return ports.Proposal{}, err
	}
	p, err := ParseResponse(done.Content, req.Finding)
	if err != nil {
		return ports.Proposal{}, err
	}
	p.TokensIn, p.TokensOut = done.TokensIn, done.TokensOut
	return p, nil
}

type rawResponse struct {
	Diff       string   `json:"diff"`
	Summary    string   `json:"summary"`
	Confidence *float64 `json:"confidence"`
	Skip       bool     `json:"skip"`
	SkipReason string   `json:"skip_reason"`
}

// ParseResponse checks the model's answer against the finding it was asked to
// fix. The answer is untrusted, so the result is only a proposal: the diff must
// still pass `git apply --check` against the working tree.
//
// It returns an error wrapping ports.ErrNoProposal when the model declined,
// was not confident enough, did not answer in the required format, or proposed
// a patch that breaks a rule: it must be a plain unified diff for exactly one
// file, the finding's, within ports.MaxDiffLines. The error text never repeats
// the model's output.
func ParseResponse(content string, f model.Finding) (ports.Proposal, error) {
	var raw rawResponse
	dec := json.NewDecoder(strings.NewReader(stripFences(content)))
	if err := dec.Decode(&raw); err != nil {
		return ports.Proposal{}, noProposal("the response is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ports.Proposal{}, noProposal("the response has content after the JSON object")
	}

	if raw.Skip {
		return ports.Proposal{}, noProposal("the model declined to propose a fix")
	}
	if raw.Confidence == nil || math.IsNaN(*raw.Confidence) || *raw.Confidence < 0 || *raw.Confidence > 1 {
		return ports.Proposal{}, noProposal("the confidence is missing or out of range")
	}
	if *raw.Confidence < ports.MinConfidence {
		return ports.Proposal{}, noProposal(fmt.Sprintf("confidence %.2f is below %.2f", *raw.Confidence, ports.MinConfidence))
	}
	if err := checkDiff(raw.Diff, f.File); err != nil {
		return ports.Proposal{}, noProposal("invalid patch: " + err.Error())
	}

	summary := cleanSummary(raw.Summary)
	if summary == "" {
		summary = "fix " + f.RuleID
	}
	return ports.Proposal{Diff: raw.Diff, Summary: summary, Confidence: *raw.Confidence}, nil
}

func noProposal(why string) error { return fmt.Errorf("%w: %s", ports.ErrNoProposal, why) }

var fenceRE = regexp.MustCompile("(?s)^\\s*```[a-zA-Z]*\\s*\n(.*?)\n?```\\s*$")

// stripFences removes a markdown code fence some models wrap JSON in.
func stripFences(s string) string {
	if m := fenceRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// cleanSummary makes the summary safe to put in a pull request title: one
// line, no control characters, bounded length.
func cleanSummary(s string) string {
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
	out := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(out); len(r) > maxSummaryRunes {
		out = string(r[:maxSummaryRunes])
	}
	return out
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// checkDiff verifies diff is a plain unified diff of exactly one file, wantFile,
// by walking its structure with the hunk line counts. Renames, mode changes,
// file creation or deletion and binary patches are all rejected.
func checkDiff(diff, wantFile string) error {
	if strings.TrimSpace(diff) == "" {
		return errors.New("the diff is empty")
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if len(lines) > ports.MaxDiffLines {
		return fmt.Errorf("the diff has %d lines, the limit is %d", len(lines), ports.MaxDiffLines)
	}
	i := 0

	// Optional git preamble.
	if strings.HasPrefix(lines[i], "diff --git ") {
		if lines[i] != "diff --git a/"+wantFile+" b/"+wantFile {
			return errors.New("the diff does not target the finding's file")
		}
		i++
		if i < len(lines) && strings.HasPrefix(lines[i], "index ") {
			i++
		}
	}

	// File headers.
	if i+1 >= len(lines) || !strings.HasPrefix(lines[i], "--- ") || !strings.HasPrefix(lines[i+1], "+++ ") {
		return errors.New("missing file headers (an unsupported kind of change?)")
	}
	if headerPath(lines[i][4:]) != "a/"+wantFile || headerPath(lines[i+1][4:]) != "b/"+wantFile {
		return errors.New("the diff does not target exactly the finding's file")
	}
	i += 2

	hunks, changed := 0, 0
	for i < len(lines) {
		m := hunkRE.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.HasPrefix(lines[i], "--- ") || strings.HasPrefix(lines[i], "diff --git ") {
				return errors.New("the diff touches more than one file")
			}
			return errors.New("unexpected content after a hunk")
		}
		hunks++
		oldLeft, newLeft := count(m[1]), count(m[2])
		i++
		for oldLeft > 0 || newLeft > 0 {
			if i >= len(lines) {
				return errors.New("a hunk is shorter than its header says")
			}
			l := lines[i]
			switch {
			case l == "" || l[0] == ' ': // an empty line is context whose blank was stripped
				oldLeft--
				newLeft--
			case l[0] == '-':
				oldLeft--
				changed++
			case l[0] == '+':
				newLeft--
				changed++
			case l[0] == '\\': // "\ No newline at end of file"
			default:
				return errors.New("a hunk has a malformed line")
			}
			if oldLeft < 0 || newLeft < 0 {
				return errors.New("a hunk is longer than its header says")
			}
			i++
		}
		for i < len(lines) && strings.HasPrefix(lines[i], "\\") {
			i++
		}
	}
	if hunks == 0 {
		return errors.New("the diff has no hunks")
	}
	if changed == 0 {
		return errors.New("the diff changes nothing")
	}
	return nil
}

// headerPath returns the path of a ---/+++ line, dropping a trailing tab and
// timestamp. Quoted paths are not supported and never match.
func headerPath(s string) string {
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	}
	return strings.TrimRight(s, " ")
}

func count(s string) int {
	if s == "" {
		return 1 // "@@ -5 +5 @@" means one line
	}
	n := 0
	for _, c := range []byte(s) {
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 1 << 20
		}
	}
	return n
}
