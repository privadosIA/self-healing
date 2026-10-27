package trivy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/privadosIA/self-healing/internal/core/model"
)

const (
	// maxReportBytes bounds how much of an uploaded report is read.
	maxReportBytes = 50 << 20
	maxDescRunes   = 1500
)

// ErrNotTrivyReport is returned when the input is valid JSON but not a Trivy report.
var ErrNotTrivyReport = errors.New("trivy: not a Trivy JSON report")

// report mirrors the parts of `trivy --format json` that are used. Pointers
// tell an absent field from an empty one.
type report struct {
	SchemaVersion *int            `json:"SchemaVersion"`
	Results       json.RawMessage `json:"Results"`
}

type result struct {
	Target          string          `json:"Target"`
	Vulnerabilities []vulnerability `json:"Vulnerabilities"`
}

type vulnerability struct {
	VulnerabilityID  string `json:"VulnerabilityID"`
	PkgName          string `json:"PkgName"`
	InstalledVersion string `json:"InstalledVersion"`
	FixedVersion     string `json:"FixedVersion"`
	Severity         string `json:"Severity"`
	Title            string `json:"Title"`
	Description      string `json:"Description"`
}

// Parse extracts the vulnerabilities of a Trivy JSON report as Findings, in
// report order. Misconfigurations and secrets are ignored.
//
// A vulnerability points at a dependency, not at a line: File is the Target
// (for example go.mod) and the line range is left at zero. Entries without a
// target or a vulnerability id are skipped.
func Parse(r io.Reader) ([]model.Finding, error) {
	return parse(r, maxReportBytes)
}

func parse(r io.Reader, limit int64) ([]model.Finding, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("trivy: reading report: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("trivy: report exceeds %d bytes", limit)
	}

	var rep report
	if err := json.Unmarshal(body, &rep); err != nil {
		return nil, fmt.Errorf("trivy: invalid JSON: %w", err)
	}
	if rep.SchemaVersion == nil && rep.Results == nil {
		return nil, ErrNotTrivyReport
	}

	var results []result
	if len(rep.Results) > 0 {
		if err := json.Unmarshal(rep.Results, &results); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotTrivyReport, err)
		}
	}

	var findings []model.Finding
	for _, res := range results {
		if res.Target == "" {
			continue
		}
		for _, v := range res.Vulnerabilities {
			if v.VulnerabilityID == "" {
				continue
			}
			findings = append(findings, model.Finding{
				Source:          model.SourceTrivy,
				RuleID:          v.VulnerabilityID,
				Severity:        mapSeverity(v.Severity),
				File:            res.Target,
				Message:         message(v),
				FixHint:         strings.TrimSpace(v.FixedVersion),
				RuleDescription: truncate(squash(v.Description), maxDescRunes),
				Package:         v.PkgName,
			})
		}
	}
	return findings, nil
}

func message(v vulnerability) string {
	subject := strings.TrimSpace(v.PkgName + " " + v.InstalledVersion)
	title := squash(v.Title)
	switch {
	case subject != "" && title != "":
		return subject + ": " + title
	case subject != "":
		return subject + " is vulnerable"
	default:
		return title
	}
}

var spaceRE = regexp.MustCompile(`\s+`)

func squash(s string) string { return strings.TrimSpace(spaceRE.ReplaceAllString(s, " ")) }

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func mapSeverity(s string) model.Severity {
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return model.SeverityCritical
	case "HIGH":
		return model.SeverityHigh
	case "MEDIUM":
		return model.SeverityMedium
	case "LOW":
		return model.SeverityLow
	default: // UNKNOWN and anything unexpected
		return model.SeverityInfo
	}
}
