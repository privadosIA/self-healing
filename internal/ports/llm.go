package ports

import (
	"context"
	"errors"

	"github.com/privadosIA/self-healing/internal/core/model"
)

// MinConfidence is the lowest model confidence for which a patch is accepted.
const MinConfidence = 0.6

// MaxDiffLines is the longest diff accepted, counting every line of it.
const MaxDiffLines = 200

// ErrNoProposal means the model produced nothing usable: it declined, was not
// confident enough, answered with something unparseable, or the patch broke a
// rule. The job should end skipped, not failed; the error text says why and
// never contains source code.
var ErrNoProposal = errors.New("no usable proposal")

// FixRequest asks the model to fix one finding.
type FixRequest struct {
	Finding model.Finding
	// FileContent is the finding's file at the failing commit.
	FileContent string
	// ContextLines is the context kept on each side of the finding; zero means the default.
	ContextLines int
	// BuildCmd is the command that verifies the project builds, if any.
	BuildCmd string
}

// Proposal is a validated patch. The diff has the right shape, but whether it
// applies to the working tree is still to be checked.
type Proposal struct {
	Diff       string
	Summary    string
	Confidence float64
	TokensIn   int
	TokensOut  int
}

// LLM turns a finding into a patch.
type LLM interface {
	// Propose returns a validated proposal, or an error wrapping ErrNoProposal
	// when the model gave nothing usable. Any other error is a failure of the
	// call itself.
	Propose(ctx context.Context, req FixRequest) (Proposal, error)
}
