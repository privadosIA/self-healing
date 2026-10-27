package model

import (
	"fmt"
	"strings"
)

// AutoFixPrefix starts the name of every branch healerd creates. Events on
// such branches are ignored so a failing fix cannot trigger another fix.
const AutoFixPrefix = "auto-fix/"

const maxSlugLen = 50

// AutoFixBranch names the branch for a fix: auto-fix/{rule-id-slug}/{short-sha}.
func AutoFixBranch(ruleID, sha string) string {
	return AutoFixPrefix + slug(ruleID) + "/" + shortSHA(sha)
}

// slug lowercases s and keeps letters and digits, joining runs of anything else
// with a single hyphen, so it is always safe inside a branch name.
func slug(s string) string {
	var b strings.Builder
	dash := true // swallows leading separators
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > maxSlugLen {
		out = strings.TrimRight(out[:maxSlugLen], "-")
	}
	if out == "" {
		return "finding"
	}
	return out
}

// shortSHA returns the first seven hexadecimal characters of sha.
func shortSHA(sha string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(sha) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
			if b.Len() == 7 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// CommitSubject is the first line of the fix commit.
func CommitSubject(f Finding) string {
	return fmt.Sprintf("fix(%s): %s in %s", f.Source, f.RuleID, f.File)
}
