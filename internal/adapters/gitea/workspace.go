package gitea

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/privadosIA/self-healing/internal/gitcli"
)

// maxFileBytes bounds what ReadFile returns; source files that need a fix are
// far smaller, and the content ends up in a prompt.
const maxFileBytes = 1 << 20

// ErrFileTooLarge is returned by ReadFile for a file over maxFileBytes.
var ErrFileTooLarge = errors.New("gitea: file is too large")

type workspace struct {
	dir  string
	root *os.Root
	opts gitcli.Options

	once     sync.Once
	closeErr error
}

// ReadFile reads a regular file of the checkout. It goes through os.Root, so a
// symlink in the repository cannot lead outside the workspace, and it refuses
// git's own metadata.
func (w *workspace) ReadFile(path string) ([]byte, error) {
	if !fs.ValidPath(path) || path == "." {
		return nil, fmt.Errorf("gitea: %q is not a valid repository path", path)
	}
	if first, _, _ := strings.Cut(path, "/"); first == ".git" {
		return nil, errors.New("gitea: the .git directory is not readable")
	}

	f, err := w.root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gitea: opening file: %w", unwrapPathError(err))
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("gitea: reading file: %w", unwrapPathError(err))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("gitea: not a regular file")
	}
	if info.Size() > maxFileBytes {
		return nil, ErrFileTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gitea: reading file: %w", unwrapPathError(err))
	}
	if len(data) > maxFileBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}

// Close deletes the workspace.
func (w *workspace) Close() error {
	w.once.Do(func() {
		rootErr := w.root.Close()
		removeErr := os.RemoveAll(w.dir)
		w.closeErr = errors.Join(rootErr, removeErr)
	})
	return w.closeErr
}

// unwrapPathError drops the absolute path os puts in its errors, which would
// reveal where workspaces live.
func unwrapPathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
