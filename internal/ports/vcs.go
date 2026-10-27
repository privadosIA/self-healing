package ports

import "context"

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
	// Close deletes the checkout. It is safe to call more than once.
	Close() error
}

// VCS is the version control system hosting the repositories.
type VCS interface {
	// Checkout shallow-fetches exactly one commit into a fresh workspace.
	Checkout(ctx context.Context, cloneURL, sha string) (Workspace, error)
}
