// Package gitcli wraps the git executable. Git is always run with a fixed
// argument list, never through a shell, and with the user's and the system's
// configuration ignored.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const maxStderr = 500

// run executes git in dir with stdin and returns its stdout. The error holds a
// short, trimmed stderr, which is where git explains itself.
func run(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed executable; arguments are built by this package
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > maxStderr {
			msg = msg[:maxStderr] + "..."
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// ApplyCheck reports whether diff applies cleanly to the working tree in dir,
// without changing anything. Paths must carry git's a/ and b/ prefixes.
func ApplyCheck(ctx context.Context, dir, diff string) error {
	if strings.TrimSpace(diff) == "" {
		return errors.New("gitcli: empty diff")
	}
	_, err := run(ctx, dir, []byte(diff), "apply", "--check", "-")
	return err
}
