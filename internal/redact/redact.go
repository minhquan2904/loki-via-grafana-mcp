// Package redact holds the deny list and the address checks shared by the
// benchmark report renderer and the repository scanner. Only generic patterns
// live here: anything naming a real host, tenant or account belongs in the
// gitignored .denylist.local file of the machine that runs the scan, never in
// this public repository.
package redact

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// defaultPatterns are the built-in generic shapes that must never reach a public
// repository: private address ranges, Grafana token prefixes and a bearer
// credential that got pasted into a file.
var defaultPatterns = []string{
	`\b10\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`,
	`\b172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}\b`,
	`\b192\.168\.\d{1,3}\.\d{1,3}\b`,
	`(fc|fd)[0-9a-f]{2}:`,
	`glsa_[A-Za-z0-9]{8,}`,
	`glc_[A-Za-z0-9]{8,}`,
	`Authorization: Bearer \S{8,}`,
}

// entry pairs a pattern with its compiled form so Patterns and Match can report
// the source text rather than the regexp.
type entry struct {
	pattern string
	re      *regexp.Regexp
}

// Denylist is an ordered set of deny patterns.
type Denylist struct {
	entries []entry
}

// Default returns a Denylist holding the built-in generic patterns.
func Default() *Denylist {
	d := &Denylist{}
	for _, pattern := range defaultPatterns {
		d.addPattern(pattern)
	}
	return d
}

// WithFile appends one pattern per non-empty, non-comment line of path. Patterns
// are matched case-insensitively.
func (d *Denylist) WithFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("denylist file %s is unreadable: %w", path, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		pattern := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return fmt.Errorf("denylist file %s line %d: invalid pattern: %w", path, i+1, err)
		}
		d.entries = append(d.entries, entry{pattern: pattern, re: re})
	}
	return nil
}

// Match reports the first pattern that matches s, so callers can name the rule
// that fired without printing the value that tripped it.
func (d *Denylist) Match(s string) (string, bool) {
	for _, e := range d.entries {
		if e.re.MatchString(s) {
			return e.pattern, true
		}
	}
	return "", false
}

// Patterns returns the configured patterns: built-ins first, then the lines of
// every WithFile call in file order.
func (d *Denylist) Patterns() []string {
	out := make([]string, 0, len(d.entries))
	for _, e := range d.entries {
		out = append(out, e.pattern)
	}
	return out
}

func (d *Denylist) addPattern(pattern string) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return
	}
	d.entries = append(d.entries, entry{pattern: pattern, re: re})
}

// ipCandidate matches IPv4 and colon-separated IPv6 literals inside free text.
var ipCandidate = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}|[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,}`)

// IsPrivateIP reports whether s contains an address literal other than
// loopback. A benchmark report has no business carrying any address at all, so
// anything that parses and is not 127.0.0.0/8 or ::1 counts as refused.
func IsPrivateIP(s string) bool {
	for _, candidate := range ipCandidate.FindAllString(s, -1) {
		ip := net.ParseIP(strings.Trim(candidate, "[]"))
		if ip == nil {
			continue
		}
		if !ip.IsLoopback() {
			return true
		}
	}
	return false
}
