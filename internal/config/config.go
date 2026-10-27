// Package config loads and validates the daemon configuration from the
// environment. Every variable is prefixed HEALER_; secrets may also be given
// as a file path through HEALER_<NAME>_FILE.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/privadosIA/self-healing/internal/glob"
)

const (
	envPrefix = "HEALER_"

	defaultExcludeGlobs = "**/*.lock,**/migrations/**,**/*secret*"
)

// Secret is a string that never prints itself, so a Config can be logged or
// formatted without leaking credentials.
type Secret string

func (Secret) String() string                 { return "[redacted]" }
func (Secret) GoString() string               { return "[redacted]" }
func (Secret) LogValue() slog.Value           { return slog.StringValue("[redacted]") }
func (s Secret) Reveal() string               { return string(s) }
func (s Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Config is the validated daemon configuration.
type Config struct {
	ListenAddr string
	DBPath     string
	LogLevel   string

	GiteaURL      string
	GiteaToken    Secret
	WebhookSecret Secret
	ReportsToken  Secret // empty disables PUT /reports

	SonarURL   string // empty disables the Sonar scanner
	SonarToken Secret

	LLMURL    string
	LLMAPIKey Secret
	LLMModel  string

	AllowedHosts Allowlist

	GitAuthorName  string
	GitAuthorEmail string

	LLMTimeout     time.Duration
	JobTimeout     time.Duration
	ShutdownGrace  time.Duration
	Workers        int
	MaxOpenPRs     int
	SnippetContext int
	ExcludeGlobs   []string
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

// Load builds a Config using lookup to read variables. It reports every
// problem at once, naming the variable but never its value.
func Load(lookup func(string) (string, bool)) (Config, error) {
	l := &loader{lookup: lookup}
	var c Config

	c.ListenAddr = l.str("LISTEN_ADDR", ":8080")
	c.DBPath = l.str("DB_PATH", "healerd.db")
	c.LogLevel = strings.ToLower(l.str("LOG_LEVEL", "info"))
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		l.fail("%sLOG_LEVEL must be one of debug, info, warn, error", envPrefix)
	}

	// The allowlist comes first: it gates every endpoint below.
	al, err := ParseAllowlist(l.str("ALLOWED_HOSTS", ""))
	switch {
	case err != nil:
		l.fail("%sALLOWED_HOSTS is invalid: %v", envPrefix, err)
	case al.Empty():
		l.fail("%sALLOWED_HOSTS is required: set the host suffixes or CIDRs the daemon may contact", envPrefix)
	}
	c.AllowedHosts = al

	c.GiteaURL = l.endpoint("GITEA_URL", true, al)
	c.GiteaToken = l.secret("GITEA_TOKEN", true)
	c.WebhookSecret = l.secret("WEBHOOK_SECRET", true)
	c.ReportsToken = l.secret("REPORTS_TOKEN", false)

	c.SonarURL = l.endpoint("SONAR_URL", false, al)
	c.SonarToken = l.secret("SONAR_TOKEN", c.SonarURL != "")

	c.LLMURL = l.endpoint("LLM_URL", true, al)
	c.LLMAPIKey = l.secret("LLM_API_KEY", false)
	c.LLMModel = l.str("LLM_MODEL", "")
	if c.LLMModel == "" {
		l.fail("%sLLM_MODEL is required", envPrefix)
	}

	c.GitAuthorName = l.identity("GIT_AUTHOR_NAME", "ci-healer-bot")
	c.GitAuthorEmail = l.identity("GIT_AUTHOR_EMAIL", "ci-healer-bot@users.noreply.invalid")

	c.LLMTimeout = l.duration("LLM_TIMEOUT", 5*time.Minute)
	c.JobTimeout = l.duration("JOB_TIMEOUT", 15*time.Minute)
	c.ShutdownGrace = l.duration("SHUTDOWN_GRACE", 30*time.Second)

	c.Workers = l.integer("WORKERS", 2, 1)
	c.MaxOpenPRs = l.integer("MAX_OPEN_PRS", 3, 1)
	c.SnippetContext = l.integer("SNIPPET_CONTEXT_LINES", 30, 1)
	for _, g := range strings.Split(l.str("EXCLUDE_GLOBS", defaultExcludeGlobs), ",") {
		if g = strings.TrimSpace(g); g != "" {
			c.ExcludeGlobs = append(c.ExcludeGlobs, g)
		}
	}
	if _, err := glob.CompileAll(c.ExcludeGlobs); err != nil {
		l.fail("%sEXCLUDE_GLOBS is invalid: %v", envPrefix, err)
	}

	if err := errors.Join(l.errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

type loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) str(name, def string) string {
	if v, ok := l.lookup(envPrefix + name); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

// identity reads a git author field, which must be one plain line.
func (l *loader) identity(name, def string) string {
	v := l.str(name, def)
	if strings.ContainsAny(v, "<>\r\n\x00") {
		l.fail("%s%s must not contain angle brackets or line breaks", envPrefix, name)
		return def
	}
	return v
}

// duration reads a positive Go duration such as 90s or 5m.
func (l *loader) duration(name string, def time.Duration) time.Duration {
	raw := l.str(name, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		l.fail("%s%s must be a positive duration such as 90s or 5m", envPrefix, name)
		return def
	}
	return d
}

func (l *loader) integer(name string, def, lowest int) int {
	raw := l.str(name, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lowest {
		l.fail("%s%s must be an integer >= %d", envPrefix, name, lowest)
		return def
	}
	return n
}

// secret reads HEALER_<name> or, alternatively, the file named by
// HEALER_<name>_FILE.
func (l *loader) secret(name string, required bool) Secret {
	direct := l.str(name, "")
	file := l.str(name+"_FILE", "")
	switch {
	case direct != "" && file != "":
		l.fail("set only one of %s%s and %s%s_FILE", envPrefix, name, envPrefix, name)
	case file != "":
		b, err := os.ReadFile(file) // #nosec G304 -- path is set by the operator via HEALER_*_FILE, never from request data
		if err != nil {
			// Report the OS reason only; the path is not echoed.
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			l.fail("cannot read %s%s_FILE: %v", envPrefix, name, err)
			return ""
		}
		direct = strings.TrimSpace(string(b))
	}
	if direct == "" && required {
		l.fail("%s%s (or %s%s_FILE) is required", envPrefix, name, envPrefix, name)
	}
	return Secret(direct)
}

// endpoint reads a base URL and checks that its host is on the allowlist.
func (l *loader) endpoint(name string, required bool, al Allowlist) string {
	raw := l.str(name, "")
	if raw == "" {
		if required {
			l.fail("%s%s is required", envPrefix, name)
		}
		return ""
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		l.fail("%s%s must be an http(s) URL with a host", envPrefix, name)
		return ""
	case u.User != nil:
		l.fail("%s%s must not embed credentials", envPrefix, name)
		return ""
	case !al.Empty() && !al.Allows(u.Hostname()):
		l.fail("%s%s host is not permitted by %sALLOWED_HOSTS", envPrefix, name, envPrefix)
		return ""
	}
	return strings.TrimRight(raw, "/")
}
