package model

import (
	"strings"
	"testing"
)

func TestAutoFixBranch(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		rule, sha, want string
	}{
		{"go:S1234", sha, "auto-fix/go-s1234/0123456"},
		{"CVE-2099-0001", sha, "auto-fix/cve-2099-0001/0123456"},
		{"  --weird__Rule!!  ", sha, "auto-fix/weird-rule/0123456"},
		{"squid:S00117", "ABCDEF1234", "auto-fix/squid-s00117/abcdef1"},
		{"", sha, "auto-fix/finding/0123456"},
		{"!!!", sha, "auto-fix/finding/0123456"},
		{"rule", "", "auto-fix/rule/unknown"},
		{"rule", "zz", "auto-fix/rule/unknown"},
		{"rule", "abc", "auto-fix/rule/abc"},
		{"../../etc", sha, "auto-fix/etc/0123456"},
		{"a/b\\c d", sha, "auto-fix/a-b-c-d/0123456"},
		{"ünïcode-rülé", sha, "auto-fix/n-code-r-l/0123456"},
	}
	for _, tt := range tests {
		if got := AutoFixBranch(tt.rule, tt.sha); got != tt.want {
			t.Errorf("AutoFixBranch(%q, %q) = %q, want %q", tt.rule, tt.sha, got, tt.want)
		}
	}
}

func TestAutoFixBranchIsAlwaysSafe(t *testing.T) {
	long := strings.Repeat("abc-", 100)
	for _, rule := range []string{long, "a b\nc\td", "-leading", "trailing-", "x..y", "x.lock", "~^:?*[\\", "\x00\x01"} {
		got := AutoFixBranch(rule, "0123456789abcdef")
		if !strings.HasPrefix(got, AutoFixPrefix) || len(got) > len(AutoFixPrefix)+maxSlugLen+8 {
			t.Errorf("unexpected branch %q for %q", got, rule)
		}
		rest := strings.TrimPrefix(got, AutoFixPrefix)
		for _, bad := range []string{"..", "//", ".lock", " ", "~", "^", ":", "?", "*", "[", "\\", "\n", "\x00"} {
			if strings.Contains(rest, bad) {
				t.Errorf("branch %q for %q contains %q", got, rule, bad)
			}
		}
		if strings.HasSuffix(rest, "-") || strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, ".") {
			t.Errorf("branch %q has an unsafe edge", got)
		}
	}
}

func TestSlugIsBounded(t *testing.T) {
	if got := slug(strings.Repeat("a", 500)); len(got) != maxSlugLen {
		t.Errorf("slug length = %d, want %d", len(got), maxSlugLen)
	}
	if got := slug(strings.Repeat("ab-", 40)); strings.HasSuffix(got, "-") {
		t.Errorf("slug %q ends with a hyphen", got)
	}
}

func TestCommitSubject(t *testing.T) {
	f := Finding{Source: SourceSonar, RuleID: "go:S1234", File: "internal/app.go"}
	if got := CommitSubject(f); got != "fix(sonar): go:S1234 in internal/app.go" {
		t.Errorf("subject = %q", got)
	}
}
