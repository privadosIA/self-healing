// Package deploy_test checks the example deployment files, since neither
// systemd nor Docker is available wherever the tests run.
package deploy_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// knownVariables returns every variable named in .env.example.
func knownVariables(t *testing.T) map[string]bool {
	t.Helper()
	known := map[string]bool{}
	for _, line := range strings.Split(read(t, "../.env.example"), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.HasPrefix(k, "HEALER_") {
			known[k] = true
		}
	}
	if len(known) == 0 {
		t.Fatal("no variables found in .env.example")
	}
	return known
}

var variableRE = regexp.MustCompile(`HEALER_[A-Z0-9_]+`)

// TestEveryVariableIsReal catches typos: a misspelled variable is ignored
// silently by the daemon, so a deployment would run with the wrong setting.
func TestEveryVariableIsReal(t *testing.T) {
	known := knownVariables(t)
	for _, file := range []string{"healerd.service", "docker-compose.example.yml"} {
		for _, name := range variableRE.FindAllString(read(t, file), -1) {
			if known[name] || known[strings.TrimSuffix(name, "_FILE")] {
				continue
			}
			t.Errorf("%s uses %s, which healerd does not read", file, name)
		}
	}
}

func TestServiceUnit(t *testing.T) {
	unit := read(t, "healerd.service")

	// Parse the INI structure: sections, then key=value pairs.
	sections := map[string]map[string][]string{}
	var cur string
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			cur = line[1 : len(line)-1]
			sections[cur] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || cur == "" {
				t.Fatalf("malformed line %q", raw)
			}
			sections[cur][k] = append(sections[cur][k], v)
		}
	}
	for _, s := range []string{"Unit", "Service", "Install"} {
		if sections[s] == nil {
			t.Fatalf("missing the [%s] section", s)
		}
	}
	svc := sections["Service"]
	one := func(key string) string {
		t.Helper()
		if len(svc[key]) != 1 {
			t.Fatalf("%s is set %d times", key, len(svc[key]))
		}
		return svc[key][0]
	}

	if one("ExecStart") != "/usr/local/bin/healerd" || one("User") == "root" || one("User") == "" {
		t.Errorf("ExecStart %q, User %q", svc["ExecStart"], svc["User"])
	}
	if one("Restart") != "on-failure" || sections["Install"]["WantedBy"][0] != "multi-user.target" {
		t.Error("restart policy or install target changed")
	}
	// systemd must wait longer than the daemon's own grace period before SIGKILL.
	stop, err := time.ParseDuration(one("TimeoutStopSec"))
	if err != nil || stop < 45*time.Second {
		t.Errorf("TimeoutStopSec = %q (%v): must exceed the default HEALER_SHUTDOWN_GRACE of 30s", svc["TimeoutStopSec"], err)
	}
	for key, want := range map[string]string{
		"NoNewPrivileges": "true", "ProtectSystem": "strict", "ProtectHome": "true", "PrivateDevices": "true",
		"PrivateTmp": "true", "RestrictSUIDSGID": "true", "LockPersonality": "true", "ProtectKernelTunables": "true",
		"RestrictNamespaces": "true", "StateDirectory": "healerd",
	} {
		if got := one(key); got != want {
			t.Errorf("%s=%s, want %s", key, got, want)
		}
	}
	if caps, ok := svc["CapabilityBoundingSet"]; !ok || caps[0] != "" {
		t.Error("the unit must drop every capability")
	}
	if strings.Contains(one("RestrictAddressFamilies"), "AF_PACKET") || strings.Contains(one("RestrictAddressFamilies"), "AF_NETLINK") {
		t.Error("only inet and unix sockets are needed")
	}
	// Secrets are never in the unit itself.
	for _, line := range svc["Environment"] {
		if k, v, _ := strings.Cut(line, "="); strings.Contains(k, "TOKEN") || strings.Contains(k, "SECRET") || strings.Contains(k, "KEY") {
			if !strings.HasSuffix(k, "_FILE") {
				t.Errorf("a secret is set in the unit: %s=%s", k, v)
			}
		}
	}
}

func TestComposeFile(t *testing.T) {
	compose := read(t, "docker-compose.example.yml")
	for _, want := range []string{
		"services:", "healerd:", "restart: unless-stopped", "build:", "context: ..",
		`"127.0.0.1:8080:8080"`, "read_only: true", "cap_drop:", "- ALL", "no-new-privileges:true",
		`user: "65532:65532"`, "healerd-data:/data", "HEALER_DB_PATH: /data/healerd.db", "stop_grace_period: 60s",
		"env_file:", "- .env",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("the compose file lacks %q", want)
		}
	}
	// The default must not publish the port on every interface.
	if regexp.MustCompile(`(?m)^\s*-\s*"?8080:8080"?\s*$`).MatchString(compose) || strings.Contains(compose, `"0.0.0.0:`) {
		t.Error("the port is published on all interfaces")
	}
	// Tabs are not valid YAML indentation.
	if strings.Contains(compose, "\t") {
		t.Error("the compose file contains a tab")
	}
}

// TestOnlyPlaceholders makes sure the examples carry no real-looking data.
func TestOnlyPlaceholders(t *testing.T) {
	hosts := regexp.MustCompile(`https?://([A-Za-z0-9.-]+)`)
	ip := regexp.MustCompile(`\b(?:10|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d+\.\d+(?:\.\d+)?\b`)
	for _, file := range []string{"healerd.service", "docker-compose.example.yml"} {
		text := read(t, file)
		for _, m := range hosts.FindAllStringSubmatch(text, -1) {
			h := m[1]
			if !strings.HasSuffix(h, "example.com") && h != "github.com" && h != "127.0.0.1" {
				t.Errorf("%s mentions the host %q: use example.com placeholders", file, h)
			}
		}
		if m := ip.FindString(text); m != "" {
			t.Errorf("%s contains the private address %s", file, m)
		}
		for _, line := range strings.Split(text, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "#") {
				continue
			}
			if k, v, ok := strings.Cut(l, ":"); ok && regexp.MustCompile(`(?i)(token|secret|password|api_?key)$`).MatchString(k) && strings.TrimSpace(v) != "" {
				t.Errorf("%s sets a secret inline: %s", file, l)
			}
		}
	}
}
