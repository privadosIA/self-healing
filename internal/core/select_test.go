package core

import (
	"math/rand"
	"testing"

	"github.com/privadosIA/self-healing/internal/core/model"
)

var defaultExcludes = []string{"**/*.lock", "**/migrations/**", "**/*secret*"}

func fnd(rule string, sev model.Severity, file string, start, end int) model.Finding {
	return model.Finding{Source: model.SourceSonar, RuleID: rule, Severity: sev, File: file, LineStart: start, LineEnd: end}
}

func newSelector(t *testing.T, globs []string) *Selector {
	t.Helper()
	s, err := NewSelector(globs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name     string
		findings []model.Finding
		excludes []string
		want     string // rule id of the chosen finding; "" means none
		excluded int
	}{
		{"no findings", nil, defaultExcludes, "", 0},
		{"single finding", []model.Finding{fnd("A", model.SeverityLow, "a.go", 1, 1)}, defaultExcludes, "A", 0},
		{"higher severity wins", []model.Finding{
			fnd("low", model.SeverityLow, "a.go", 1, 1),
			fnd("crit", model.SeverityCritical, "b.go", 1, 50),
			fnd("high", model.SeverityHigh, "c.go", 1, 1),
		}, defaultExcludes, "crit", 0},
		{"severity beats a smaller snippet", []model.Finding{
			fnd("small", model.SeverityLow, "a.go", 5, 5),
			fnd("big", model.SeverityHigh, "b.go", 1, 100),
		}, defaultExcludes, "big", 0},
		{"same severity: fewer lines wins", []model.Finding{
			fnd("wide", model.SeverityMedium, "a.go", 10, 40),
			fnd("narrow", model.SeverityMedium, "b.go", 10, 12),
			fnd("mid", model.SeverityMedium, "c.go", 10, 20),
		}, defaultExcludes, "narrow", 0},
		{"a dependency finding counts as one line", []model.Finding{
			fnd("code", model.SeverityMedium, "a.go", 10, 12),
			{Source: model.SourceTrivy, RuleID: "CVE-2099-1", Severity: model.SeverityMedium, File: "go.mod", Package: "x"},
		}, defaultExcludes, "CVE-2099-1", 0},
		{"excluded lock file is skipped", []model.Finding{
			fnd("lock", model.SeverityCritical, "web/yarn.lock", 1, 1),
			fnd("code", model.SeverityLow, "a.go", 1, 1),
		}, defaultExcludes, "code", 1},
		{"excluded migrations directory is skipped", []model.Finding{
			fnd("mig", model.SeverityCritical, "db/migrations/001_init.sql", 1, 1),
			fnd("code", model.SeverityInfo, "a.go", 1, 1),
		}, defaultExcludes, "code", 1},
		{"excluded secret file is skipped", []model.Finding{
			fnd("sec", model.SeverityCritical, "config/client-secret.yaml", 1, 1),
			fnd("code", model.SeverityInfo, "a.go", 1, 1),
		}, defaultExcludes, "code", 1},
		{"all excluded", []model.Finding{
			fnd("a", model.SeverityHigh, "x.lock", 1, 1),
			fnd("b", model.SeverityHigh, "db/migrations/1.sql", 1, 1),
		}, defaultExcludes, "", 2},
		{"no exclusions configured", []model.Finding{fnd("lock", model.SeverityHigh, "x.lock", 1, 1)}, nil, "lock", 0},
		{"exclusion matches case-insensitively", []model.Finding{fnd("a", model.SeverityHigh, "Config/MySecret.go", 1, 1)}, defaultExcludes, "", 1},
		{"custom exclusion", []model.Finding{
			fnd("v", model.SeverityHigh, "vendor/x/y.go", 1, 1),
			fnd("c", model.SeverityLow, "internal/y.go", 1, 1),
		}, []string{"vendor/**"}, "c", 1},
		{"finding without a rule is unusable", []model.Finding{
			fnd("", model.SeverityCritical, "a.go", 1, 1),
			fnd("ok", model.SeverityLow, "b.go", 1, 1),
		}, defaultExcludes, "ok", 1},
		{"finding without a file is unusable", []model.Finding{
			fnd("nofile", model.SeverityCritical, "", 1, 1),
			fnd("ok", model.SeverityLow, "b.go", 1, 1),
		}, defaultExcludes, "ok", 1},
		{"paths that leave the repository are unusable", []model.Finding{
			fnd("up", model.SeverityCritical, "../outside.go", 1, 1),
			fnd("abs", model.SeverityCritical, "/etc/passwd", 1, 1),
			fnd("git", model.SeverityCritical, ".git/config", 1, 1),
			fnd("deep", model.SeverityCritical, "a/../../b.go", 1, 1),
			fnd("ok", model.SeverityLow, "b.go", 1, 1),
		}, defaultExcludes, "ok", 4},
		{"an unknown severity never beats a known low one", []model.Finding{
			fnd("weird", "bogus", "a.go", 1, 1),
			fnd("low", model.SeverityLow, "b.go", 1, 1),
		}, defaultExcludes, "low", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newSelector(t, tt.excludes).Select(tt.findings)
			if tt.want == "" {
				if got.Found {
					t.Errorf("chose %+v, want nothing", got.Finding)
				}
			} else if !got.Found || got.Finding.RuleID != tt.want {
				t.Errorf("chose %q (found %v), want %q", got.Finding.RuleID, got.Found, tt.want)
			}
			if got.Considered != len(tt.findings) || got.Excluded != tt.excluded {
				t.Errorf("considered %d excluded %d, want %d and %d", got.Considered, got.Excluded, len(tt.findings), tt.excluded)
			}
		})
	}
}

func TestSelectIsDeterministicWhateverTheOrder(t *testing.T) {
	findings := []model.Finding{
		fnd("R2", model.SeverityMedium, "b.go", 10, 10),
		fnd("R1", model.SeverityMedium, "a.go", 10, 10),
		fnd("R3", model.SeverityMedium, "a.go", 5, 5),
		fnd("R0", model.SeverityMedium, "a.go", 5, 5),
	}
	s := newSelector(t, defaultExcludes)
	want := s.Select(findings).Finding // a.go:5 and the smaller rule id
	if want.RuleID != "R0" || want.File != "a.go" || want.LineStart != 5 {
		t.Fatalf("tie-break chose %+v", want)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		shuffled := append([]model.Finding(nil), findings...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := s.Select(shuffled).Finding; got != want {
			t.Fatalf("order changed the choice: %+v vs %+v", got, want)
		}
	}
}

func TestSelectDoesNotModifyItsInput(t *testing.T) {
	in := []model.Finding{fnd("b", model.SeverityLow, "./b.go", 1, 1), fnd("a", model.SeverityHigh, "a.go", 1, 1)}
	snapshot := append([]model.Finding(nil), in...)
	newSelector(t, nil).Select(in)
	for i := range in {
		if in[i] != snapshot[i] {
			t.Errorf("input[%d] changed: %+v", i, in[i])
		}
	}
}

func TestSelectNormalizesThePath(t *testing.T) {
	got := newSelector(t, nil).Select([]model.Finding{fnd("a", model.SeverityLow, `.\internal\\app.go`, 1, 1)})
	if !got.Found || got.Finding.File != "internal/app.go" {
		t.Errorf("file = %q", got.Finding.File)
	}
	// An exclusion applies to the normalized path.
	if s := newSelector(t, []string{"internal/**"}).Select([]model.Finding{fnd("a", model.SeverityLow, "./internal/app.go", 1, 1)}); s.Found {
		t.Error("an exclusion did not apply to a path written with ./")
	}
}

func TestNewSelectorRejectsBadGlobs(t *testing.T) {
	if _, err := NewSelector([]string{"**/*.lock", "[unclosed"}); err == nil {
		t.Error("expected an error for an invalid glob")
	}
}

func TestSpan(t *testing.T) {
	for _, tt := range []struct {
		f    model.Finding
		want int
	}{
		{fnd("a", "", "f", 5, 5), 1},
		{fnd("a", "", "f", 5, 9), 5},
		{fnd("a", "", "f", 0, 0), 1},
		{fnd("a", "", "f", 9, 5), 1}, // inverted range
		{fnd("a", "", "f", -3, 4), 1},
	} {
		if got := span(tt.f); got != tt.want {
			t.Errorf("span(%d..%d) = %d, want %d", tt.f.LineStart, tt.f.LineEnd, got, tt.want)
		}
	}
}
