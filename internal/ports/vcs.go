package ports

import (
	"context"
	"errors"
)

// ErrPatchRejected means the patch cannot be used on this checkout: it does not
// apply, or it changes files other than the finding's. The job should end
// skipped, not failed.
var ErrPatchRejected = errors.New("patch rejected")

// Trailer is a "Key: value" line at the end of a commit message.
type Trailer struct {
	Key, Value string
}

// CommitSpec describes the commit that carries a fix.
type CommitSpec struct {
	// Branch is created from the checked-out commit and receives the commit.
	Branch string
	// File is the only path the diff may change.
	File string
	// Diff is a unified diff, already validated structurally.
	Diff string
	// Subject is the first line of the commit message. Subject and trailer
	// values may come from scanner output, so the adapter flattens them to one
	// line.
	Subject  string
	Trailers []Trailer
}

// PullRequest is a request to merge Head into Base, to be reviewed by a human.
type PullRequest struct {
	// Repo is "owner/name".
	Repo string
	// Title and Body may contain text from scanner and model output; the
	// builder is responsible for making them safe to render.
	Title, Body string
	// Head is the fix branch and Base the branch the failing commit was on.
	Head, Base string
	// Labels are label names; the adapter creates the ones that do not exist.
	Labels []string
}

// Workspace is a throwaway checkout of one commit. It must be closed, which
// deletes it from disk.
type Workspace interface {
	// ReadFile returns a file of the checkout. The path is relative to the
	// repository root and can never resolve outside it, even through symlinks
	// in the repository.
	ReadFile(path string) ([]byte, error)
	// BranchExists reports whether the remote already has the branch.
	BranchExists(ctx context.Context, branch string) (bool, error)
	// CommitPatch creates the branch, applies the diff and commits it, and
	// returns the commit id. It fails, leaving nothing committed, when the diff
	// does not apply or changes any path other than spec.File.
	CommitPatch(ctx context.Context, spec CommitSpec) (commitSHA string, err error)
	// Push publishes the branch. It never forces: a remote branch that already
	// holds other commits makes it fail.
	Push(ctx context.Context, branch string) error
	// DeleteRemoteBranch removes a branch from the remote. It is used to undo a
	// push whose pull request could not be opened. Deleting a branch that is
	// already gone is not guaranteed to succeed, so callers should not rely on it.
	DeleteRemoteBranch(ctx context.Context, branch string) error
	// Close deletes the checkout. It is safe to call more than once.
	Close() error
}

// VCS is the version control system hosting the repositories.
type VCS interface {
	// Checkout shallow-fetches exactly one commit into a fresh workspace.
	Checkout(ctx context.Context, cloneURL, sha string) (Workspace, error)
	// OpenPullRequest opens a pull request and returns its URL. Nothing here
	// merges, approves or enables auto-merge: a human always decides.
	OpenPullRequest(ctx context.Context, pr PullRequest) (url string, err error)
	// CountOpenAutoFixPRs counts the open pull requests of repo ("owner/name")
	// whose branch was created by healerd.
	CountOpenAutoFixPRs(ctx context.Context, repo string) (int, error)
}
