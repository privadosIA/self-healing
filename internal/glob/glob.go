// Package glob matches slash-separated paths against patterns with "**".
//
// A "**" segment matches any number of directories, including none. Other
// segments use path.Match syntax (*, ? and [classes]) and never cross a "/".
// A pattern without any "/" matches at every depth, as in a .gitignore file,
// so "*.lock" and "**/*.lock" are the same. Matching ignores case.
package glob

import (
	"fmt"
	"path"
	"strings"
)

// Pattern is a compiled glob.
type Pattern struct {
	raw  string
	segs []string
}

// Compile parses a pattern, rejecting malformed ones such as an unclosed "[".
func Compile(pattern string) (Pattern, error) {
	p := strings.ToLower(strings.TrimSpace(pattern))
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return Pattern{}, fmt.Errorf("glob: empty pattern")
	}
	if !strings.Contains(p, "/") {
		p = "**/" + p
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" {
			return Pattern{}, fmt.Errorf("glob: %q has an empty path segment", pattern)
		}
		if s == "**" {
			continue
		}
		if _, err := path.Match(s, ""); err != nil {
			return Pattern{}, fmt.Errorf("glob: %q is not a valid pattern: %w", pattern, err)
		}
	}
	return Pattern{raw: pattern, segs: collapse(segs)}, nil
}

// collapse merges consecutive "**" segments, which mean the same as one.
func collapse(segs []string) []string {
	out := segs[:0:0]
	for _, s := range segs {
		if s == "**" && len(out) > 0 && out[len(out)-1] == "**" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// String returns the pattern as it was written.
func (p Pattern) String() string { return p.raw }

// Match reports whether name matches the pattern. name uses "/" separators and
// is taken relative to the repository root; "./" and a leading "/" are ignored.
func (p Pattern) Match(name string) bool {
	name = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/"))
	if name == "" {
		return false
	}
	return match(p.segs, strings.Split(name, "/"))
}

func match(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // a trailing ** matches everything below
			}
			for i := 0; i <= len(name); i++ {
				if match(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// Set is a list of patterns.
type Set []Pattern

// CompileAll compiles every pattern, naming the first one that is invalid.
func CompileAll(patterns []string) (Set, error) {
	set := make(Set, 0, len(patterns))
	for _, s := range patterns {
		p, err := Compile(s)
		if err != nil {
			return nil, err
		}
		set = append(set, p)
	}
	return set, nil
}

// Match reports whether any pattern matches name.
func (s Set) Match(name string) bool {
	for _, p := range s {
		if p.Match(name) {
			return true
		}
	}
	return false
}
