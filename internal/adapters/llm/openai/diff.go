package openai

import (
	"errors"
	"fmt"
	"strings"

	"github.com/privadosIA/self-healing/internal/ports"
)

// diffContext is how many unchanged lines surround the change in a diff.
const diffContext = 3

const noNewlineMarker = `\ No newline at end of file`

// buildDiff returns the unified diff that replaces lines first..last (1-based,
// inclusive) of content with replacement, for the file named file.
//
// The model only chooses the range and the new text, so the hunk header and its
// counts are right by construction. The diff has a single hunk, trimmed to the
// lines that really change plus up to diffContext real lines on each side. An
// empty replacement deletes the lines; a trailing newline in a non-empty
// replacement is ignored. The file's CRLF line endings and its missing final
// newline are kept.
//
// Errors never repeat the replacement, which is untrusted model output.
func buildDiff(file, content string, first, last int, replacement string) (string, error) {
	lines := splitLines(content)
	if first < 1 || last < first || last > len(lines) {
		return "", fmt.Errorf("the line range %d-%d is not inside the %d-line file", first, last, len(lines))
	}
	noEOL := !strings.HasSuffix(content, "\n")
	reachesEnd := last == len(lines)
	old := lines[first-1 : last]
	repl := replacementLines(replacement, strings.HasSuffix(old[0], "\r"), noEOL && reachesEnd)

	if len(lines)-len(old)+len(repl) == 0 {
		return "", errors.New("the replacement would leave the file empty")
	}
	if equalLines(old, repl) {
		return "", errors.New("the replacement changes nothing")
	}

	// Trim what the replacement leaves alone. When the range ends a file that has
	// no final newline, its last line stays in the change, so the diff can state
	// that the newline is missing on both sides.
	prefix, suffix := 0, 0
	maxPrefix := min(len(old), len(repl))
	if noEOL && reachesEnd {
		maxPrefix = min(maxPrefix, len(old)-1)
	}
	for prefix < maxPrefix && old[prefix] == repl[prefix] {
		prefix++
	}
	if !noEOL || !reachesEnd {
		for suffix < min(len(old), len(repl))-prefix && old[len(old)-1-suffix] == repl[len(repl)-1-suffix] {
			suffix++
		}
	}
	oldMid, newMid := old[prefix:len(old)-suffix], repl[prefix:len(repl)-suffix]
	if len(oldMid) == 0 && len(newMid) == 0 {
		return "", errors.New("the replacement changes nothing")
	}

	at := first - 1 + prefix // zero-based index of the first changed line
	before := lines[max(0, at-diffContext):at]
	afterStart := at + len(oldMid)
	after := lines[afterStart:min(len(lines), afterStart+diffContext)]
	hunkStart := at - len(before) + 1

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", file, file)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", hunkStart, len(before)+len(oldMid)+len(after), hunkStart, len(before)+len(newMid)+len(after))
	emit := func(prefix string, ls []string) {
		for _, l := range ls {
			b.WriteString(prefix + l + "\n")
		}
	}
	emit(" ", before)
	emit("-", oldMid)
	if noEOL && len(after) == 0 && afterStart == len(lines) && len(oldMid) > 0 {
		b.WriteString(noNewlineMarker + "\n")
	}
	emit("+", newMid)
	if noEOL && len(after) == 0 && afterStart == len(lines) && len(newMid) > 0 {
		b.WriteString(noNewlineMarker + "\n")
	}
	emit(" ", after)
	if noEOL && len(after) > 0 && afterStart+len(after) == len(lines) {
		b.WriteString(noNewlineMarker + "\n")
	}

	diff := b.String()
	if n := strings.Count(diff, "\n"); n > ports.MaxDiffLines {
		return "", fmt.Errorf("the diff has %d lines, the limit is %d", n, ports.MaxDiffLines)
	}
	return diff, nil
}

// replacementLines splits the model's text into file lines. crlf gives every
// line a carriage return, as the lines it replaces have; noEOL leaves the last
// one without, because the file's last line has none.
func replacementLines(s string, crlf, noEOL bool) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	out := strings.Split(s, "\n")
	for i, l := range out {
		l = strings.TrimSuffix(l, "\r")
		if crlf && (!noEOL || i != len(out)-1) {
			l += "\r"
		}
		out[i] = l
	}
	return out
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
