package trivy

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/core/model"
)

func TestParseFixture(t *testing.T) {
	f, err := os.Open("../../../testdata/trivy/report.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	got, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Finding{
		{
			Source: "trivy", RuleID: "CVE-2099-0001", Severity: model.SeverityHigh, File: "go.mod",
			Message: "example.com/acme/lib 1.2.0: Buffer overflow in the parser", FixHint: "1.2.5",
			RuleDescription: "A crafted input can overflow the parser buffer in acme lib.", Package: "example.com/acme/lib",
		},
		{
			Source: "trivy", RuleID: "CVE-2099-0002", Severity: model.SeverityCritical, File: "go.mod",
			Message: "example.com/acme/other 0.9.0: Remote code execution", FixHint: "",
			RuleDescription: "No fix is available yet.", Package: "example.com/acme/other",
		},
		{
			Source: "trivy", RuleID: "CVE-2099-0003", Severity: model.SeverityMedium, File: "web/package-lock.json",
			Message: "acme-widget 3.0.1: Prototype pollution", FixHint: "3.0.2, 3.1.0", Package: "acme-widget",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d =\n%+v\nwant\n%+v", i, got[i], want[i])
		}
		if got[i].LineStart != 0 || got[i].LineEnd != 0 {
			t.Errorf("finding %d must not claim a line", i)
		}
	}
}

func TestParseIgnoresNonVulnerabilities(t *testing.T) {
	// The fixture also holds a misconfiguration and an empty result.
	f, _ := os.Open("../../../testdata/trivy/report.json")
	defer func() { _ = f.Close() }()
	got, _ := Parse(f)
	for _, fd := range got {
		if fd.File == "Dockerfile" || fd.File == "clean/go.sum" {
			t.Errorf("unexpected finding from %s", fd.File)
		}
	}
}

func TestParseAcceptedReports(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"no vulnerabilities", `{"SchemaVersion":2,"Results":[{"Target":"go.mod"}]}`, 0},
		{"results omitted", `{"SchemaVersion":2,"ArtifactName":"."}`, 0},
		{"results null", `{"Results":null}`, 0},
		{"empty results", `{"Results":[]}`, 0},
		{"schema version only", `{"SchemaVersion":2}`, 0},
		{"unknown severity", `{"Results":[{"Target":"go.mod","Vulnerabilities":[{"VulnerabilityID":"CVE-2099-1","Severity":"UNKNOWN"}]}]}`, 1},
		{"skips entry without id", `{"Results":[{"Target":"go.mod","Vulnerabilities":[{"PkgName":"x"},{"VulnerabilityID":"CVE-2099-1"}]}]}`, 1},
		{"skips result without target", `{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-2099-1"}]}]}`, 0},
		{"unknown fields are ignored", `{"Results":[{"Target":"go.mod","Extra":1,"Vulnerabilities":[{"VulnerabilityID":"CVE-2099-1","New":{}}]}]}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Errorf("got %d findings, want %d", len(got), tt.want)
			}
		})
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"not json", `{oops`, nil},
		{"empty body", ``, nil},
		{"json array", `[1,2]`, nil},
		{"valid json but not a report", `{"foo":"bar"}`, ErrNotTrivyReport},
		{"empty object", `{}`, ErrNotTrivyReport},
		{"results of the wrong type", `{"Results":"nope"}`, ErrNotTrivyReport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.in))
			if err == nil {
				t.Fatalf("expected an error, got %+v", got)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseEnforcesSizeLimit(t *testing.T) {
	in := `{"SchemaVersion":2,"Results":[]}` + strings.Repeat(" ", 100)
	if _, err := parse(strings.NewReader(in), 50); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size error", err)
	}
	if _, err := parse(strings.NewReader(`{"Results":[]}`), 50); err != nil {
		t.Errorf("a report within the limit failed: %v", err)
	}
}

func TestMessageFallbacks(t *testing.T) {
	tests := []struct {
		v    vulnerability
		want string
	}{
		{vulnerability{PkgName: "lib", InstalledVersion: "1.0", Title: "Bad"}, "lib 1.0: Bad"},
		{vulnerability{PkgName: "lib", InstalledVersion: "1.0"}, "lib 1.0 is vulnerable"},
		{vulnerability{Title: "Only a title"}, "Only a title"},
	}
	for _, tt := range tests {
		if got := message(tt.v); got != tt.want {
			t.Errorf("message(%+v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestDescriptionIsCapped(t *testing.T) {
	long := strings.Repeat("é", maxDescRunes*2)
	in := `{"Results":[{"Target":"go.mod","Vulnerabilities":[{"VulnerabilityID":"CVE-2099-1","Description":"` + long + `"}]}]}`
	got, err := Parse(strings.NewReader(in))
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].RuleDescription)); n != maxDescRunes {
		t.Errorf("description has %d runes, want %d", n, maxDescRunes)
	}
}

func TestMapSeverity(t *testing.T) {
	for in, want := range map[string]model.Severity{
		"CRITICAL": model.SeverityCritical, "HIGH": model.SeverityHigh, "MEDIUM": model.SeverityMedium,
		"LOW": model.SeverityLow, "UNKNOWN": model.SeverityInfo, "": model.SeverityInfo, "high": model.SeverityHigh,
	} {
		if got := mapSeverity(in); got != want {
			t.Errorf("mapSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}
