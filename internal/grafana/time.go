// Package grafana talks to a Grafana instance over the read-only datasource
// query API and converts its dataframes into a small, stable result shape.
package grafana

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Version is stamped into the User-Agent header. cmd/server overwrites it with
// the build version.
var Version = "dev"

// TargetPoints is the number of points a series should have when the caller
// does not pin a step.
const TargetPoints = 300

// timeFormat is the wire format of Result log times: UTC, always three decimals.
const timeFormat = "2006-01-02T15:04:05.000Z"

// ResolveTime turns a user supplied time into an absolute instant.
//
// Accepted: "now"; "now-<n><unit>" with n a positive integer and unit in
// s,m,h,d,w; epoch milliseconds (all digits); RFC3339 / RFC3339Nano; and
// "YYYY-MM-DDTHH:MM:SS[.frac]" without an offset, which is read as UTC.
// Leading and trailing spaces are trimmed. An empty string means "now".
func ResolveTime(value string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(value)
	if s == "" || s == "now" {
		return now, nil
	}

	if rest, ok := strings.CutPrefix(s, "now-"); ok {
		if d, ok := parseRelative(rest); ok {
			return now.Add(-d), nil
		}
		return time.Time{}, invalidTime(value)
	}

	if isAllDigits(s) {
		ms, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return time.UnixMilli(ms).UTC(), nil
		}
		return time.Time{}, invalidTime(value)
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UTC(), nil
	}

	return time.Time{}, invalidTime(value)
}

// invalidTime builds the ResolveTime error for value.
func invalidTime(value string) error {
	return fmt.Errorf(`time must be "now", "now-<n><s|m|h|d|w>", epoch ms or RFC3339, got %q`, value)
}

// parseRelative parses "<n><unit>" where unit is one of s,m,h,d,w and n is a
// positive integer.
func parseRelative(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	digits := s[:len(s)-1]
	if !isAllDigits(digits) {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, false
	}
	var unitDuration time.Duration
	switch unit {
	case 's':
		unitDuration = time.Second
	case 'm':
		unitDuration = time.Minute
	case 'h':
		unitDuration = time.Hour
	case 'd':
		unitDuration = 24 * time.Hour
	case 'w':
		unitDuration = 7 * 24 * time.Hour
	default:
		return 0, false
	}
	// A huge n would wrap around int64 and land on an unrelated window.
	if int64(n) > math.MaxInt64/int64(unitDuration) {
		return 0, false
	}
	return time.Duration(n) * unitDuration, true
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseStep resolves a step argument. An empty string means "auto" and returns
// zero. A bare integer is read as seconds; anything else goes through
// time.ParseDuration. The result must be a whole number of seconds between 1s
// and 24h.
func ParseStep(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if n, err := strconv.Atoi(s); err == nil {
		// Bound n before multiplying so a huge value cannot wrap into range.
		if n < 1 || n > 86400 {
			return 0, invalidStep(s)
		}
		d = time.Duration(n) * time.Second
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return 0, invalidStep(s)
		}
		d = parsed
	}
	if d < time.Second || d > 24*time.Hour || d%time.Second != 0 {
		return 0, invalidStep(s)
	}
	return d, nil
}

// invalidStep builds the ParseStep error for s.
func invalidStep(s string) error {
	return fmt.Errorf("step must be a whole number of seconds between 1s and 24h (e.g. 30s, 5m, 60), got %q", s)
}

// AutoStep picks a resolution that aims at roughly TargetPoints points for the
// window, never below one second, rounded up to whole seconds.
func AutoStep(from, to time.Time) time.Duration {
	d := to.Sub(from)
	if d <= 0 {
		return time.Second
	}
	per := d / TargetPoints
	if d%TargetPoints != 0 {
		per++
	}
	seconds := per / time.Second
	if per%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds * time.Second
}

// FormatTime renders an epoch-millisecond timestamp as an RFC3339 UTC string
// with exactly three fractional digits.
func FormatTime(ms float64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(timeFormat)
}
