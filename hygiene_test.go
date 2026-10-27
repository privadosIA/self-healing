// Package hygiene checks that nothing about a real environment ends up in this
// public repository: internal host names, private addresses, secrets, personal
// email addresses or home-directory paths.
package hygiene

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// finding is one problem in one file.
type finding struct {
	line int
	what string
	text string
}

var (
	// A name ending in a label that is only ever used inside private networks.
	internalHostRE = regexp.MustCompile(`(?i)[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:lan|local|localdomain|internal|intranet|corp|home|arpa)`)

	privateIPRE = regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|169\.254\.\d{1,3}\.\d{1,3})\b`)

	// Credentials with a recognizable shape.
	tokenShapes = []*regexp.Regexp{
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{15,}`),
		regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{32,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`),
		regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----`),
	}

	// name = long random-looking value.
	secretAssignRE = regexp.MustCompile(`(?i)(?:token|secret|passw(?:or)?d|api[_-]?key)["']?\s*[:=]\s*["']?([A-Za-z0-9+/_=-]{20,})`)

	emailRE    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,})`)
	homePathRE = regexp.MustCompile(`(?:/Users/[A-Za-z][A-Za-z0-9._-]*|/home/[a-z][a-z0-9_-]*|[A-Za-z]:\\Users\\[A-Za-z][A-Za-z0-9._ -]*)`)
	personRE   = regexp.MustCompile(`(?i)\b(?:jesus|jesús|suero)\b`)
)

// scan returns the problems in text, which came from the file at path.
func scan(path, text string) []finding {
	var out []finding
	isTest := strings.HasSuffix(path, "_test.go")
	isLicense := filepath.Base(path) == "LICENSE"

	for i, line := range strings.Split(text, "\n") {
		add := func(what string) { out = append(out, finding{i + 1, what, strings.TrimSpace(line)}) }

		for _, m := range internalHostRE.FindAllStringIndex(line, -1) {
			if endsAHostName(line, m[1]) {
				add("internal host name")
				break
			}
		}
		if !isTest && privateIPRE.MatchString(line) {
			add("private address (allowed only as an input of a test)")
		}
		for _, re := range tokenShapes {
			if re.MatchString(line) {
				add("credential-shaped value")
				break
			}
		}
		for _, m := range secretAssignRE.FindAllStringSubmatch(line, -1) {
			if !placeholder(m[1]) && !strings.HasPrefix(m[1], "/") && !strings.Contains(line, "secrets.") {
				add("secret assigned a real-looking value")
				break
			}
		}
		for _, m := range emailRE.FindAllStringSubmatch(line, -1) {
			if !placeholderDomain(m[1]) {
				add("email address that is not a placeholder")
				break
			}
		}
		for _, m := range homePathRE.FindAllString(line, -1) {
			if m != "/home/healer" { // the container user's home directory
				add("home-directory path")
				break
			}
		}
		if !isLicense && personRE.MatchString(line) {
			add("a personal name (only the license names the author)")
		}
	}
	return out
}

// endsAHostName reports whether the label that ends at line[:end] really ends a
// host name, rather than being the middle of a longer one such as "corp.test".
func endsAHostName(line string, end int) bool {
	if end >= len(line) {
		return true
	}
	next := line[end]
	if next == '.' {
		return end+1 >= len(line) || !isLabelChar(line[end+1])
	}
	return !isLabelChar(next)
}

func isLabelChar(b byte) bool {
	return b == '-' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func placeholder(v string) bool {
	l := strings.ToLower(v)
	return strings.Contains(l, "changeme") || strings.Contains(l, "example") || strings.Contains(l, "placeholder") ||
		strings.HasPrefix(v, "$") || strings.HasPrefix(v, "<")
}

func placeholderDomain(d string) bool {
	d = strings.ToLower(d)
	return d == "example.com" || d == "example.org" || strings.HasSuffix(d, ".example.com") ||
		strings.HasSuffix(d, ".invalid") || strings.HasSuffix(d, ".test") || d == "users.noreply.invalid" ||
		d == "anthropic.com" // the address of the Co-Authored-By trailer on commits made with Claude Code
}

// --- the detector itself ------------------------------------------------------

// TestTheDetectorWorks makes sure the checks below cannot go blind unnoticed.
func TestTheDetectorWorks(t *testing.T) {
	bad := map[string]string{
		"a home server":              "see https://myserver.lan:3000/x",
		"a nas":                      "host nas.local",
		"an internal domain":         "https://gitea.corp.internal/api",
		"an intranet":                "wiki.intranet",
		"a private 10 address":       "listen 10.1.2.3",
		"a private 192 address":      "http://192.168.1.20:8080",
		"a private 172 address":      "172.20.0.5",
		"a GitHub token":             "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"a GitLab token":             "glpat-abcdefghijklmnop1234",
		"an AWS key":                 "AKIAABCDEFGHIJKLMNOP",
		"a private key":              "-----BEGIN RSA PRIVATE KEY-----",
		"an inline token":            `HEALER_GITEA_TOKEN=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA`,
		"a quoted secret":            `secret: "abcdefghijklmnopqrstuvwxyz012345"`,
		"a real email":               "write to someone@gmail.com",
		"a home path":                "cd /Users/alice/projects",
		"a linux home":               "/home/bob/.ssh",
		"a windows home":             `C:\Users\Carol\code`,
		"the author's name":          "written by Jesus",
		"the author's accented name": "por Jesús",
	}
	for name, text := range bad {
		if got := scan("docs/x.md", text); len(got) == 0 {
			t.Errorf("the detector missed %s: %q", name, text)
		}
	}

	good := map[string]string{
		"example hosts":           "https://gitea.example.com and llm.example.com",
		"localhost":               "http://localhost:8080 and 127.0.0.1",
		"the internal Go package": `import "github.com/privadosIA/self-healing/internal/config"`,
		"a path with internal":    "internal/app/app.go",
		"a test host":             "a.b.corp.test and corp.test.evil.org",
		"a documentation range":   "192.0.2.0/24 and 192.0.2.7",
		"a placeholder token":     "HEALER_GITEA_TOKEN=changeme",
		"an empty token":          "HEALER_REPORTS_TOKEN=",
		"a variable reference":    "HEALER_REPORTS_TOKEN=${HEALER_REPORTS_TOKEN}",
		"an Actions secret":       "token: ${{ secrets.HEALER_REPORTS_TOKEN }}",
		"a placeholder email":     "ci-healer-bot@users.noreply.invalid and t@example.com",
		"the co-author trailer":   "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>",
		"a module version":        "honnef.co/go/tools/cmd/staticcheck@v0.8.2",
		"the container home":      "--home-dir /home/healer",
		"a state directory":       "/var/lib/healerd/healerd.db",
		"a long identifier":       "TestEveryVariableIsReal",
		"a secret name, no value": "HEALER_WEBHOOK_SECRET is required",
	}
	for name, text := range good {
		if got := scan("docs/x.md", text); len(got) != 0 {
			t.Errorf("a false alarm on %s: %q -> %+v", name, text, got)
		}
	}

	// A private address is fine as an input of a test, and nowhere else.
	if got := scan("internal/config/x_test.go", `"10.1.2.3"`); len(got) != 0 {
		t.Errorf("a test input was flagged: %+v", got)
	}
	if got := scan("internal/config/x.go", `"10.1.2.3"`); len(got) == 0 {
		t.Error("a private address in production code was not flagged")
	}
	// Only the license may name the author.
	if got := scan("LICENSE", "Copyright (c) 2026 Jesús Suero"); len(got) != 0 {
		t.Errorf("the license was flagged: %+v", got)
	}
	// A hostile host is flagged even inside a test.
	if got := scan("internal/x_test.go", "host: nas.local"); len(got) == 0 {
		t.Error("an internal host name in a test was not flagged")
	}
}

// TestFindingsDoNotCarryTheirContentIntoReports guards the choice above: the
// report format has no place for the matched text.
func TestFindingsDoNotCarryTheirContentIntoReports(t *testing.T) {
	for _, f := range scan("x.md", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		if strings.Contains(f.what, "ghp_") {
			t.Errorf("a finding description repeats the secret: %q", f.what)
		}
	}
}

// --- the repository ----------------------------------------------------------

// files lists every file git tracks or would track: new files count before they
// are committed.
func files(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	var list []string
	for _, f := range strings.Split(out.String(), "\x00") {
		if f != "" {
			list = append(list, f)
		}
	}
	if len(list) < 20 {
		t.Fatalf("only %d files listed; is the check running in the repository root?", len(list))
	}
	return list
}

// TestRepositoryHasNoRealInfrastructureData scans every file of the repository.
func TestRepositoryHasNoRealInfrastructureData(t *testing.T) {
	scanned := 0
	for _, path := range files(t) {
		if path == "go.sum" || path == "hygiene_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // deleted but not yet staged
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary
		}
		scanned++
		for _, f := range scan(path, string(data)) {
			// The offending line is deliberately not printed: if it is a real
			// secret, the log of a public CI run must not repeat it.
			t.Errorf("%s:%d: %s", path, f.line, f.what)
		}
	}
	if scanned < 20 {
		t.Errorf("only %d files were scanned", scanned)
	}
}

// TestNothingSensitiveIsTracked checks the files git must never carry.
func TestNothingSensitiveIsTracked(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(^|/)(\.env|\.env\.[^/]*|[^/]*\.pem|[^/]*\.key|[^/]*\.sqlite[^/]*|[^/]*\.db(-[^/]*)?|id_rsa[^/]*|coverage\.out)$`)
	for _, path := range files(t) {
		if base := filepath.Base(path); base == ".env.example" {
			continue
		}
		if forbidden.MatchString(path) {
			t.Errorf("%s must not be tracked", path)
		}
		if strings.HasPrefix(path, "bin/") || strings.HasPrefix(path, "dist/") || strings.HasPrefix(path, "deploy/secrets/") ||
			strings.HasPrefix(path, "workdir/") || strings.HasPrefix(path, "reports/") {
			t.Errorf("%s must not be tracked", path)
		}
	}
}
