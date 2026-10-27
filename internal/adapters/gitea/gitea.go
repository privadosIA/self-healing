package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/gitcli"
	"github.com/privadosIA/self-healing/internal/ports"
)

// Config configures the adapter.
type Config struct {
	// BaseURL is the Gitea root, for example https://gitea.example.com. Only
	// repositories hosted there can be cloned.
	BaseURL string
	// Token is the bot account's token.
	Token string
	// Allow is the egress allowlist; git does not go through the safehttp
	// client, so the clone host is checked here.
	Allow config.Allowlist
	// WorkRoot is the directory workspaces are created in; os.TempDir() when empty.
	WorkRoot string
}

// Adapter is a ports.VCS backed by Gitea.
type Adapter struct {
	base      *url.URL
	token     string
	allow     config.Allowlist
	workRoot  string
	protocols string // git transports; DefaultProtocols unless a test overrides it
}

var _ ports.VCS = (*Adapter)(nil)

// New validates cfg and returns an Adapter.
func New(cfg Config) (*Adapter, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return nil, errors.New("gitea: BaseURL must be an http(s) URL with a host")
	}
	if base.User != nil {
		return nil, errors.New("gitea: BaseURL must not embed credentials")
	}
	if cfg.Token == "" {
		return nil, errors.New("gitea: Token is required")
	}
	if !cfg.Allow.Allows(base.Hostname()) {
		return nil, errors.New("gitea: the Gitea host is not permitted by the allowlist")
	}
	root := cfg.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	return &Adapter{base: base, token: cfg.Token, allow: cfg.Allow, workRoot: root}, nil
}

// checkCloneURL accepts only a plain http(s) URL on the configured Gitea host.
// The URL comes from a webhook, so it is checked even though the webhook is
// authenticated. Errors never echo the URL.
func (a *Adapter) checkCloneURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return errors.New("gitea: the clone URL must be an http(s) URL with a host")
	case u.User != nil:
		return errors.New("gitea: the clone URL must not embed credentials")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("gitea: the clone URL must not have a query or a fragment")
	case !a.allow.Allows(u.Hostname()):
		return errors.New("gitea: the clone URL host is not permitted by the allowlist")
	case !sameAuthority(u, a.base):
		return errors.New("gitea: the clone URL is not on the configured Gitea host")
	}
	return nil
}

// sameAuthority compares host and effective port, ignoring case and default ports.
func sameAuthority(a, b *url.URL) bool {
	return strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// Checkout shallow-fetches the commit into a new private directory. The
// directory is deleted if the checkout fails, and by Close otherwise.
func (a *Adapter) Checkout(ctx context.Context, cloneURL, sha string) (ports.Workspace, error) {
	if err := a.checkCloneURL(cloneURL); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(a.workRoot, "healerd-")
	if err != nil {
		return nil, fmt.Errorf("gitea: creating workspace: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()

	opts := gitcli.Options{Token: a.token, Protocols: a.protocols}
	if err := gitcli.Clone(ctx, dir, cloneURL, sha, opts); err != nil {
		return nil, fmt.Errorf("gitea: clone failed: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("gitea: opening workspace: %w", err)
	}
	keep = true
	return &workspace{dir: dir, root: root}, nil
}
