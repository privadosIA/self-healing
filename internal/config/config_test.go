package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validEnv returns a minimal valid configuration using placeholder values.
func validEnv() map[string]string {
	return map[string]string{
		"HEALER_ALLOWED_HOSTS":  "example.com,10.0.0.0/8",
		"HEALER_GITEA_URL":      "https://gitea.example.com/",
		"HEALER_GITEA_TOKEN":    "changeme-gitea",
		"HEALER_WEBHOOK_SECRET": "changeme-hmac",
		"HEALER_LLM_URL":        "http://llm.example.com/v1",
		"HEALER_LLM_MODEL":      "local-model",
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr []string // substrings that must appear in the error
	}{
		{name: "valid minimal"},
		{
			name: "valid with sonar and cidr host",
			mutate: func(e map[string]string) {
				e["HEALER_SONAR_URL"] = "http://10.1.2.3:9000"
				e["HEALER_SONAR_TOKEN"] = "changeme"
			},
		},
		{
			name:    "empty allowlist refuses to start",
			mutate:  func(e map[string]string) { delete(e, "HEALER_ALLOWED_HOSTS") },
			wantErr: []string{"HEALER_ALLOWED_HOSTS is required"},
		},
		{
			name:    "blank allowlist refuses to start",
			mutate:  func(e map[string]string) { e["HEALER_ALLOWED_HOSTS"] = " , " },
			wantErr: []string{"HEALER_ALLOWED_HOSTS is required"},
		},
		{
			name:    "invalid cidr",
			mutate:  func(e map[string]string) { e["HEALER_ALLOWED_HOSTS"] = "10.0.0.0/99" },
			wantErr: []string{"HEALER_ALLOWED_HOSTS is invalid"},
		},
		{
			name:    "gitea host outside allowlist",
			mutate:  func(e map[string]string) { e["HEALER_GITEA_URL"] = "https://gitea.other.test" },
			wantErr: []string{"HEALER_GITEA_URL host is not permitted"},
		},
		{
			name:    "llm host outside allowlist",
			mutate:  func(e map[string]string) { e["HEALER_LLM_URL"] = "http://192.168.1.5/v1" },
			wantErr: []string{"HEALER_LLM_URL host is not permitted"},
		},
		{
			name:    "suffix must match a label boundary",
			mutate:  func(e map[string]string) { e["HEALER_GITEA_URL"] = "https://evilexample.com" },
			wantErr: []string{"HEALER_GITEA_URL host is not permitted"},
		},
		{
			name:    "url with embedded credentials",
			mutate:  func(e map[string]string) { e["HEALER_GITEA_URL"] = "https://bot:pw@gitea.example.com" },
			wantErr: []string{"HEALER_GITEA_URL must not embed credentials"},
		},
		{
			name:    "url with bad scheme",
			mutate:  func(e map[string]string) { e["HEALER_LLM_URL"] = "ftp://llm.example.com" },
			wantErr: []string{"HEALER_LLM_URL must be an http(s) URL"},
		},
		{
			name:    "missing secrets",
			mutate:  func(e map[string]string) { delete(e, "HEALER_GITEA_TOKEN"); delete(e, "HEALER_WEBHOOK_SECRET") },
			wantErr: []string{"HEALER_GITEA_TOKEN", "HEALER_WEBHOOK_SECRET"},
		},
		{
			name:    "sonar url without token",
			mutate:  func(e map[string]string) { e["HEALER_SONAR_URL"] = "https://sonar.example.com" },
			wantErr: []string{"HEALER_SONAR_TOKEN"},
		},
		{
			name:    "missing model",
			mutate:  func(e map[string]string) { delete(e, "HEALER_LLM_MODEL") },
			wantErr: []string{"HEALER_LLM_MODEL is required"},
		},
		{
			name:    "workers below one",
			mutate:  func(e map[string]string) { e["HEALER_WORKERS"] = "0" },
			wantErr: []string{"HEALER_WORKERS must be an integer >= 1"},
		},
		{
			name:    "bad log level",
			mutate:  func(e map[string]string) { e["HEALER_LOG_LEVEL"] = "verbose" },
			wantErr: []string{"HEALER_LOG_LEVEL must be one of"},
		},
		{
			name:    "all problems reported together",
			mutate:  func(e map[string]string) { clear(e) },
			wantErr: []string{"HEALER_ALLOWED_HOSTS", "HEALER_GITEA_URL", "HEALER_GITEA_TOKEN", "HEALER_LLM_URL", "HEALER_LLM_MODEL"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			if tt.mutate != nil {
				tt.mutate(env)
			}
			_, err := Load(lookupFrom(env))
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 2 || c.MaxOpenPRs != 3 || c.SnippetContext != 30 || c.LogLevel != "info" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if got := strings.Join(c.ExcludeGlobs, ","); got != defaultExcludeGlobs {
		t.Errorf("ExcludeGlobs = %q", got)
	}
	if c.GiteaURL != "https://gitea.example.com" {
		t.Errorf("trailing slash not trimmed: %q", c.GiteaURL)
	}
}

func TestSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("changeme-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file value is used and trimmed", func(t *testing.T) {
		env := validEnv()
		delete(env, "HEALER_GITEA_TOKEN")
		env["HEALER_GITEA_TOKEN_FILE"] = good
		c, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatal(err)
		}
		if c.GiteaToken.Reveal() != "changeme-from-file" {
			t.Errorf("token not read from file")
		}
	})
	t.Run("both direct and file is an error", func(t *testing.T) {
		env := validEnv()
		env["HEALER_GITEA_TOKEN_FILE"] = good
		if _, err := Load(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), "only one of") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unreadable file does not echo its path", func(t *testing.T) {
		env := validEnv()
		delete(env, "HEALER_GITEA_TOKEN")
		missing := filepath.Join(dir, "does-not-exist")
		env["HEALER_GITEA_TOKEN_FILE"] = missing
		_, err := Load(lookupFrom(env))
		if err == nil || !strings.Contains(err.Error(), "HEALER_GITEA_TOKEN_FILE") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), missing) {
			t.Errorf("error leaks the file path: %v", err)
		}
	})
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	env := validEnv()
	env["HEALER_ALLOWED_HOSTS"] = "10.0.0.0/99"
	env["HEALER_GITEA_URL"] = "https://bot:changeme-pw@gitea.example.com"
	_, err := Load(lookupFrom(env))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, secret := range []string{"changeme-pw", "changeme-gitea", "changeme-hmac"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error contains secret %q", secret)
		}
	}
}

func TestSecretIsRedactedWhenFormatted(t *testing.T) {
	c, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, c); strings.Contains(out, "changeme-gitea") || strings.Contains(out, "changeme-hmac") {
			t.Errorf("format %s leaks a secret: %s", format, out)
		}
	}
}

func TestAllowlistAllows(t *testing.T) {
	al, err := ParseAllowlist("example.com, .corp.test ,10.0.0.0/8,192.0.2.7,2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"Gitea.Example.COM", true},
		{"gitea.example.com.", true},
		{"a.b.corp.test", true},
		{"evilexample.com", false},
		{"example.com.evil.test", false},
		{"corp.test.evil.org", false},
		{"10.20.30.40", true},
		{"11.0.0.1", false},
		{"192.0.2.7", true},
		{"192.0.2.8", false},
		{"::ffff:10.1.1.1", true},
		{"2001:db8::1", true},
		{"2001:db9::1", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := al.Allows(tt.host); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
	if (Allowlist{}).Allows("example.com") {
		t.Error("zero-value allowlist must allow nothing")
	}
}

func TestParseAllowlistErrors(t *testing.T) {
	for _, in := range []string{"10.0.0.0/99", "bad host", "a@b.com", "host:8080", "."} {
		if _, err := ParseAllowlist(in); err == nil {
			t.Errorf("ParseAllowlist(%q) expected an error", in)
		}
	}
}

// parseEnvFile reads KEY=VALUE lines, skipping blanks and comments.
func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed line in %s: %q", path, line)
		}
		env[k] = v
	}
	return env
}

// TestEnvExampleCoversEveryVariable fails when the code reads a HEALER_
// variable that .env.example does not list.
func TestEnvExampleCoversEveryVariable(t *testing.T) {
	example := parseEnvFile(t, "../../.env.example")

	read := map[string]bool{}
	_, _ = Load(func(k string) (string, bool) {
		read[k] = true
		return "", false
	})
	for name := range read {
		if strings.HasSuffix(name, "_FILE") {
			continue // documented once in the header of .env.example
		}
		if _, ok := example[name]; !ok {
			t.Errorf(".env.example is missing %s", name)
		}
	}
	for name := range example {
		if !read[name] {
			t.Errorf(".env.example lists %s, which the code never reads", name)
		}
	}
}

// TestEnvExampleIsValid ensures the template loads as-is and holds only placeholders.
func TestEnvExampleIsValid(t *testing.T) {
	example := parseEnvFile(t, "../../.env.example")
	if _, err := Load(lookupFrom(example)); err != nil {
		t.Fatalf(".env.example does not load: %v", err)
	}
	for k, v := range example {
		if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "secret") || strings.Contains(strings.ToLower(k), "key") {
			if v != "" && v != "changeme" {
				t.Errorf("%s must be empty or changeme, got %q", k, v)
			}
		}
	}
}

// TestConfigurationDocCoversEveryVariable fails when the code reads a HEALER_
// variable that docs/configuration.md does not document.
func TestConfigurationDocCoversEveryVariable(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)

	read := map[string]bool{}
	_, _ = Load(func(k string) (string, bool) {
		read[k] = true
		return "", false
	})
	for name := range read {
		if !strings.Contains(doc, "`"+name+"`") && !strings.HasSuffix(name, "_FILE") {
			t.Errorf("docs/configuration.md does not document %s", name)
		}
	}
}
