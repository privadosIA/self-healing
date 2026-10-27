// Package gitcli wraps the git executable. Git is always run with a fixed
// argument list, never through a shell, with the user's and the system's
// configuration ignored, and without ambient GIT_* variables or proxies.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	maxStderr = 500
	// DefaultProtocols are the only transports git may use unless Options says otherwise.
	DefaultProtocols = "http:https"
)

// Options tune how git talks to a remote.
type Options struct {
	// Token authenticates with "Authorization: token <Token>". It travels in
	// the environment, never in the arguments, in a URL or in .git/config.
	Token string
	// Protocols is the GIT_ALLOW_PROTOCOL value; DefaultProtocols when empty.
	Protocols string
}

// environment builds the environment for a git process: the caller's, minus
// anything that could redirect or reconfigure git, plus a locked-down config.
func (o Options) environment() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_") || upper == "HOME" || upper == "XDG_CONFIG_HOME" || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		env = append(env, kv)
	}

	protocols := o.Protocols
	if protocols == "" {
		protocols = DefaultProtocols
	}
	config := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"http.followRedirects", "false"},
		{"http.proxy", ""},
		{"credential.helper", ""},
	}
	if o.Token != "" {
		config = append(config, [2]string{"http.extraHeader", "Authorization: token " + o.Token})
	}

	env = append(env,
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+protocols,
		"LC_ALL=C",
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(config)),
	)
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// command prepares a git invocation without starting it.
func command(ctx context.Context, dir string, stdin []byte, o Options, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed executable; arguments are built by this package
	cmd.Dir = dir
	cmd.Env = o.environment()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd
}

// run executes git in dir and returns its stdout. On failure the error holds a
// short stderr, with the token scrubbed, which is where git explains itself.
func run(ctx context.Context, dir string, stdin []byte, o Options, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, stdin, o, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s: %s", args[0], scrub(stderr.String(), o.Token))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

func scrub(msg, token string) string {
	msg = strings.TrimSpace(msg)
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}
	if len(msg) > maxStderr {
		msg = msg[:maxStderr] + "..."
	}
	return msg
}

// ApplyCheck reports whether diff applies cleanly to the working tree in dir,
// without changing anything. Paths must carry git's a/ and b/ prefixes.
func ApplyCheck(ctx context.Context, dir, diff string) error {
	if strings.TrimSpace(diff) == "" {
		return errors.New("gitcli: empty diff")
	}
	_, err := run(ctx, dir, []byte(diff), Options{}, "apply", "--check", "-")
	return err
}

var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Clone fetches exactly one commit, with no history and no tags, into the
// empty directory dir and checks it out detached. The remote URL must not
// carry credentials: use Options.Token. Submodules are never fetched.
func Clone(ctx context.Context, dir, remoteURL, sha string, o Options) error {
	if !fullSHA.MatchString(sha) {
		return errors.New("gitcli: sha must be a full lowercase hexadecimal commit id")
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", remoteURL},
		{"fetch", "-q", "--depth", "1", "--no-tags", "--no-recurse-submodules", "origin", sha},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := run(ctx, dir, nil, o, args...); err != nil {
			return err
		}
	}
	head, err := run(ctx, dir, nil, o, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(head)); got != sha {
		return fmt.Errorf("gitcli: checked out %s instead of the requested commit", got)
	}
	return nil
}
