package gitea

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"unicode"

	"github.com/privadosIA/self-healing/internal/gitcli"
	"github.com/privadosIA/self-healing/internal/ports"
)

const maxLineRunes = 200

var trailerKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// BranchExists asks the remote whether the branch exists.
func (w *workspace) BranchExists(ctx context.Context, branch string) (bool, error) {
	ok, err := gitcli.RemoteBranchExists(ctx, w.dir, branch, w.opts)
	if err != nil {
		return false, fmt.Errorf("gitea: checking branch: %w", err)
	}
	return ok, nil
}

// Push publishes the fix branch to the remote, without forcing.
func (w *workspace) Push(ctx context.Context, branch string) error {
	if err := gitcli.Push(ctx, w.dir, branch, w.opts); err != nil {
		return fmt.Errorf("gitea: push failed: %w", err)
	}
	return nil
}

// DeleteRemoteBranch removes a branch from the remote.
func (w *workspace) DeleteRemoteBranch(ctx context.Context, branch string) error {
	if err := gitcli.DeleteRemoteBranch(ctx, w.dir, branch, w.opts); err != nil {
		return fmt.Errorf("gitea: deleting the remote branch failed: %w", err)
	}
	return nil
}

// CommitPatch creates spec.Branch at the checked-out commit, applies the diff
// and commits it. The diff must change exactly spec.File: that is checked on
// what git actually staged, not on what the diff claims. On any failure the
// changes are discarded and no commit is made.
func (w *workspace) CommitPatch(ctx context.Context, spec ports.CommitSpec) (string, error) {
	if !gitcli.ValidBranch(spec.Branch) {
		return "", errors.New("gitea: invalid branch name")
	}
	if !fs.ValidPath(spec.File) || spec.File == "." || strings.HasPrefix(spec.File, ".git/") || spec.File == ".git" {
		return "", errors.New("gitea: invalid file path")
	}
	message, err := FormatCommitMessage(spec.Subject, spec.Trailers)
	if err != nil {
		return "", err
	}

	if err := gitcli.CreateBranch(ctx, w.dir, spec.Branch, w.opts); err != nil {
		return "", fmt.Errorf("gitea: creating branch: %w", err)
	}
	if err := gitcli.ApplyToIndex(ctx, w.dir, spec.Diff, w.opts); err != nil {
		return "", fmt.Errorf("gitea: the patch does not apply: %w", err)
	}
	staged, err := gitcli.StagedFiles(ctx, w.dir, w.opts)
	if err != nil {
		return "", fmt.Errorf("gitea: inspecting the patch: %w", err)
	}
	if len(staged) != 1 || staged[0] != spec.File {
		_ = gitcli.ResetHard(ctx, w.dir, w.opts)
		return "", errors.New("gitea: the patch changes files other than the finding's")
	}
	sha, err := gitcli.Commit(ctx, w.dir, message, w.opts)
	if err != nil {
		return "", fmt.Errorf("gitea: committing: %w", err)
	}
	return sha, nil
}

// FormatCommitMessage builds a commit message from a subject and trailers. Both
// may carry text from scanner output, so every value is flattened to a single
// line: a line break could otherwise forge extra trailers such as
// "Co-Authored-By". A trailer with an empty value is dropped; an invalid key is
// an error.
func FormatCommitMessage(subject string, trailers []ports.Trailer) (string, error) {
	subject = oneLine(subject)
	if subject == "" {
		return "", errors.New("gitea: the commit subject is empty")
	}
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n")

	first := true
	for _, t := range trailers {
		if !trailerKeyRE.MatchString(t.Key) {
			return "", fmt.Errorf("gitea: invalid trailer key %q", t.Key)
		}
		value := oneLine(t.Value)
		if value == "" {
			continue
		}
		if first {
			b.WriteString("\n")
			first = false
		}
		b.WriteString(t.Key + ": " + value + "\n")
	}
	return b.String(), nil
}

// oneLine collapses whitespace, removes control characters and bounds the length.
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
	out := strings.Join(strings.Fields(b.String()), " ")
	if rs := []rune(out); len(rs) > maxLineRunes {
		out = string(rs[:maxLineRunes])
	}
	return out
}
