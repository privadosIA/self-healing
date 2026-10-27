package core

import (
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/glob"
)

// Selection is the outcome of choosing a finding.
type Selection struct {
	// Finding is the chosen one; meaningful only when Found is true.
	Finding model.Finding
	Found   bool
	// Considered is how many findings were offered, Excluded how many were
	// dropped (excluded paths or findings that cannot be fixed) before choosing.
	Considered, Excluded int
}

// Selector chooses the one finding a job will try to fix.
type Selector struct {
	exclude glob.Set
}

// NewSelector compiles the exclusion globs.
func NewSelector(excludeGlobs []string) (*Selector, error) {
	set, err := glob.CompileAll(excludeGlobs)
	if err != nil {
		return nil, err
	}
	return &Selector{exclude: set}, nil
}

// Select picks the most severe finding; among equals, the one that spans the
// fewest lines, since it makes the smallest snippet and the smallest patch.
// Findings whose file matches an exclusion glob are never chosen, nor are
// those without a rule or a usable repository path. The order is total, so the
// same findings always give the same choice whatever order they arrive in.
func (s *Selector) Select(findings []model.Finding) Selection {
	sel := Selection{Considered: len(findings)}
	var candidates []model.Finding
	for _, f := range findings {
		file, ok := cleanPath(f.File)
		if !ok || f.RuleID == "" || s.exclude.Match(file) {
			sel.Excluded++
			continue
		}
		f.File = file
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		return sel
	}
	sort.SliceStable(candidates, func(i, j int) bool { return less(candidates[i], candidates[j]) })
	sel.Finding, sel.Found = candidates[0], true
	return sel
}

// less orders findings from most to least preferred.
func less(a, b model.Finding) bool {
	if ra, rb := a.Severity.Rank(), b.Severity.Rank(); ra != rb {
		return ra > rb
	}
	if sa, sb := span(a), span(b); sa != sb {
		return sa < sb
	}
	if a.File != b.File {
		return a.File < b.File
	}
	if a.LineStart != b.LineStart {
		return a.LineStart < b.LineStart
	}
	if a.RuleID != b.RuleID {
		return a.RuleID < b.RuleID
	}
	return a.Source < b.Source
}

// span is the number of lines a finding covers; a finding with no line, such as
// a dependency, counts as one.
func span(f model.Finding) int {
	if f.LineStart <= 0 || f.LineEnd < f.LineStart {
		return 1
	}
	return f.LineEnd - f.LineStart + 1
}

// cleanPath normalizes a repository path and reports whether it is usable: a
// relative path inside the repository, outside .git.
func cleanPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(strings.TrimPrefix(p, "./"))
	if p == "." || !fs.ValidPath(p) {
		return "", false
	}
	if first, _, _ := strings.Cut(p, "/"); first == ".git" {
		return "", false
	}
	return p, true
}
