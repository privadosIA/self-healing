// Package docs_test keeps the documentation honest: placeholders only, real
// variable names, working links and English only.
package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// documents are the files that make up the public documentation.
func documents(t *testing.T) []string {
	t.Helper()
	files := []string{"../README.md", "../CLAUDE.md", "../SECURITY.md", "../THREAT_MODEL.md", "../.env.example"}
	more, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)
	scripts, _ := filepath.Glob("../scripts/*.md")
	files = append(files, scripts...)
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected document %s: %v", f, err)
		}
	}
	return files
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOnlyPlaceholderHosts(t *testing.T) {
	urlRE := regexp.MustCompile(`https?://([A-Za-z0-9._-]+)`)
	privateIP := regexp.MustCompile(`\b(?:10\.\d+\.\d+\.\d+|172\.(?:1[6-9]|2\d|3[01])\.\d+\.\d+|192\.168\.\d+\.\d+)\b`)
	allowed := func(h string) bool {
		return h == "example.com" || strings.HasSuffix(h, ".example.com") ||
			h == "github.com" || h == "localhost" || h == "127.0.0.1" || h == "avd.aquasec.com"
	}
	for _, file := range documents(t) {
		text := read(t, file)
		for _, m := range urlRE.FindAllStringSubmatch(text, -1) {
			if !allowed(m[1]) {
				t.Errorf("%s mentions the host %q: use an example.com placeholder", file, m[1])
			}
		}
		if ip := privateIP.FindString(text); ip != "" {
			t.Errorf("%s contains the private address %s", file, ip)
		}
	}
}

func TestEveryVariableMentionedIsReal(t *testing.T) {
	known := map[string]bool{}
	for _, line := range strings.Split(read(t, "../.env.example"), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.HasPrefix(k, "HEALER_") {
			known[k] = true
		}
	}
	re := regexp.MustCompile(`HEALER_[A-Z0-9_]*[A-Z0-9]`)
	for _, file := range documents(t) {
		for _, name := range re.FindAllString(read(t, file), -1) {
			if known[name] || known[strings.TrimSuffix(name, "_FILE")] {
				continue
			}
			t.Errorf("%s mentions %s, which healerd does not read", file, name)
		}
	}
}

func TestRelativeLinksResolve(t *testing.T) {
	linkRE := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, file := range documents(t) {
		if !strings.HasSuffix(file, ".md") {
			continue
		}
		for _, m := range linkRE.FindAllStringSubmatch(read(t, file), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if _, err := os.Stat(filepath.Join(filepath.Dir(file), target)); err != nil {
				t.Errorf("%s links to %q, which does not exist", file, m[1])
			}
		}
	}
}

func TestEnglishOnly(t *testing.T) {
	accented := regexp.MustCompile(`[À-ÖØ-öø-ÿ¿¡]`)
	spanish := regexp.MustCompile(`(?i)\b(el|los|las|para|que|una|del|como|esto|pero|cuando|debe|puede)\b`)
	for _, file := range documents(t) {
		for i, line := range strings.Split(read(t, file), "\n") {
			if accented.MatchString(line) {
				t.Errorf("%s:%d has non-English characters: %q", file, i+1, line)
			}
			if spanish.MatchString(line) {
				t.Errorf("%s:%d looks Spanish: %q", file, i+1, line)
			}
		}
	}
}

func TestCodeFencesAreBalanced(t *testing.T) {
	for _, file := range documents(t) {
		if !strings.HasSuffix(file, ".md") {
			continue
		}
		n := 0
		for _, line := range strings.Split(read(t, file), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				n++
			}
		}
		if n%2 != 0 {
			t.Errorf("%s has an unclosed code fence", file)
		}
	}
}

func TestNoSecretsInTheDocs(t *testing.T) {
	// A value after a secret-looking name must be empty or a placeholder.
	re := regexp.MustCompile(`(?i)(HEALER_[A-Z_]*(?:TOKEN|SECRET|API_KEY))=([A-Za-z0-9_.${}/"'-]*)`)
	for _, file := range documents(t) {
		for _, m := range re.FindAllStringSubmatch(read(t, file), -1) {
			v := strings.Trim(m[2], `"'`)
			if v != "" && v != "changeme" && !strings.HasPrefix(v, "${") && !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "/") {
				t.Errorf("%s sets %s to %q: use changeme or leave it empty", file, m[1], v)
			}
		}
	}
}
