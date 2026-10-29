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
	msg, shown, err := buildPrompt(PromptInput{
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
	p, err := ParseResponse(done.Content, req.Finding, req.FileContent, shown)
	if err != nil {
		return ports.Proposal{}, err
	}
	p.TokensIn, p.TokensOut = done.TokensIn, done.TokensOut
	return p, nil
}

// rawResponse is the model's answer. The line numbers and the replacement are
// pointers so that a missing field differs from a zero value: an empty
// replacement deletes the lines, a missing one is an unusable answer.
type rawResponse struct {
	StartLine   *int     `json:"start_line"`
	EndLine     *int     `json:"end_line"`
	Replacement *string  `json:"replacement"`
	Summary     string   `json:"summary"`
	Confidence  *float64 `json:"confidence"`
	Skip        bool     `json:"skip"`
	SkipReason  string   `json:"skip_reason"`
}

// ParseResponse checks the model's answer against the finding it was asked to
// fix and turns it into a proposal. The answer is untrusted: it only chooses a
// range of lines and their new text, and the unified diff is built here from
// the file at the failing commit. The diff must still pass `git apply --check`
// against the working tree.
//
// shown is the span of file lines the model was given; a range outside it is
// refused, so the model can only edit what it saw. file is the finding's file
// content.
//
// It returns an error wrapping ports.ErrNoProposal when the model declined,
// was not confident enough, did not answer in the required format, or asked
// for a change that breaks a rule: it must replace lines of the finding's file
// that were shown, change something, and fit within ports.MaxDiffLines. The
// error text never repeats the model's output.
func ParseResponse(content string, f model.Finding, file string, shown Range) (ports.Proposal, error) {
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
	if raw.StartLine == nil || raw.EndLine == nil || raw.Replacement == nil {
		return ports.Proposal{}, noProposal("invalid patch: the response does not give the lines to replace and their new text")
	}
	if *raw.EndLine < *raw.StartLine || *raw.StartLine < shown.First || *raw.EndLine > shown.Last {
		return ports.Proposal{}, noProposal("invalid patch: the lines to replace are outside the lines the model was shown")
	}
	diff, err := buildDiff(f.File, file, *raw.StartLine, *raw.EndLine, *raw.Replacement)
	if err != nil {
		return ports.Proposal{}, noProposal("invalid patch: " + err.Error())
	}

	summary := cleanSummary(raw.Summary)
	if summary == "" {
		summary = "fix " + f.RuleID
	}
	return ports.Proposal{Diff: diff, Summary: summary, Confidence: *raw.Confidence}, nil
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
