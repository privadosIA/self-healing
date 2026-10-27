package gitcli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidBranch reports whether name is a branch name this package will pass to
// git. It is deliberately stricter than git: it never starts with "-" and
// rules out every character that has a special meaning in a ref.
func ValidBranch(name string) bool {
	return len(name) <= 200 && branchRE.MatchString(name) &&
		!strings.Contains(name, "..") && !strings.Contains(name, "//") &&
		!strings.HasSuffix(name, "/") && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, ".lock") &&
		!strings.Contains(name, "/.") && !strings.Contains(name, "@{")
}

func checkBranch(name string) error {
	if !ValidBranch(name) {
		return errors.New("gitcli: invalid branch name")
	}
	return nil
}

// RemoteBranchExists reports whether origin has the branch. It asks the remote
// and does not rely on any local state.
func RemoteBranchExists(ctx context.Context, dir, branch string, o Options) (bool, error) {
	if err := checkBranch(branch); err != nil {
		return false, err
	}
	out, err := run(ctx, dir, nil, o, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// CreateBranch creates the branch at the current commit and switches to it.
func CreateBranch(ctx context.Context, dir, branch string, o Options) error {
	if err := checkBranch(branch); err != nil {
		return err
	}
	_, err := run(ctx, dir, nil, o, "checkout", "-q", "-b", branch)
	return err
}

// ApplyToIndex applies diff to the working tree and stages the result.
func ApplyToIndex(ctx context.Context, dir, diff string, o Options) error {
	if strings.TrimSpace(diff) == "" {
		return errors.New("gitcli: empty diff")
	}
	_, err := run(ctx, dir, []byte(diff), o, "apply", "--index", "-")
	return err
}

// StagedFiles lists the paths changed in the index relative to HEAD.
func StagedFiles(ctx context.Context, dir string, o Options) ([]string, error) {
	out, err := run(ctx, dir, nil, o, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// ResetHard discards every staged and unstaged change.
func ResetHard(ctx context.Context, dir string, o Options) error {
	_, err := run(ctx, dir, nil, o, "reset", "-q", "--hard", "HEAD")
	return err
}

// Commit commits the index with message and returns the new commit id. The
// author is taken from o.
func Commit(ctx context.Context, dir, message string, o Options) (string, error) {
	if o.AuthorName == "" || o.AuthorEmail == "" {
		return "", errors.New("gitcli: an author name and email are required")
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("gitcli: empty commit message")
	}
	if _, err := run(ctx, dir, []byte(message), o, "commit", "-q", "-F", "-"); err != nil {
		return "", err
	}
	out, err := run(ctx, dir, nil, o, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !fullSHA.MatchString(sha) {
		return "", fmt.Errorf("gitcli: unexpected commit id")
	}
	return sha, nil
}
