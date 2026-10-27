package model

// Finding sources.
const (
	SourceSonar = "sonar"
	SourceTrivy = "trivy"
)

// Severity is a scanner-independent severity, ordered from Info (lowest) to
// Critical (highest).
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Rank orders severities; higher is more severe. Unknown values rank as Info.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// Repo identifies the repository and branch a scan refers to.
type Repo struct {
	ID            int64
	FullName      string
	DefaultBranch string
	Branch        string
}

// Finding is one issue reported by a scanner.
type Finding struct {
	Source          string
	RuleID          string
	Severity        Severity
	File            string
	LineStart       int
	LineEnd         int
	Message         string
	FixHint         string
	RuleDescription string
	// Package is the affected dependency for dependency findings (Trivy). Such
	// findings carry no line: it is located in File once the repository is cloned.
	Package string
}
