package ports

import "context"

// Workspace is a throwaway checkout of one commit. It must be closed, which
// deletes it from disk.
type Workspace interface {
	// ReadFile returns a file of the checkout. The path is relative to the
	// repository root and can never resolve outside it, even through symlinks
	// in the repository.
	ReadFile(path string) ([]byte, error)
	// Close deletes the checkout. It is safe to call more than once.
	Close() error
}

// VCS is the version control system hosting the repositories.
type VCS interface {
	// Checkout shallow-fetches exactly one commit into a fresh workspace.
	Checkout(ctx context.Context, cloneURL, sha string) (Workspace, error)
}
