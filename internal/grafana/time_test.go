package grafana

import (
	"testing"
	"time"
)

// TestResolveTime ports test_to_grafana_time.
func TestResolveTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"now", "now", now},
		{"empty means now", "", now},
		{"now-1h", "now-1h", now.Add(-time.Hour)},
		{"now-15m", "now-15m", now.Add(-15 * time.Minute)},
		{"now-30s", "now-30s", now.Add(-30 * time.Second)},
		{"now-2d", "now-2d", now.Add(-48 * time.Hour)},
		{"now-1w", "now-1w", now.Add(-7 * 24 * time.Hour)},
		{"epoch ms", "1759838400000", time.UnixMilli(1759838400000).UTC()},
		{"rfc3339 zulu", "2026-10-07T10:00:00Z", time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)},
		{"rfc3339 offset", "2026-10-07T17:00:00+07:00", time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)},
		{"rfc3339 nano", "2026-10-07T10:00:00.123456789Z", time.Date(2026, 10, 7, 10, 0, 0, 123456789, time.UTC)},
		{"no offset is utc", "2026-10-07T10:00:00", time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)},
		{"no offset with frac is utc", "2026-10-07T10:00:00.250", time.Date(2026, 10, 7, 10, 0, 0, 250000000, time.UTC)},
		{"trimmed", "  2026-10-07T10:00:00Z  ", time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTime(tc.value, now)
			if err != nil {
				t.Fatalf("ResolveTime(%q): %v", tc.value, err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("ResolveTime(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

// TestResolveTime_Invalid ports test_to_grafana_time_sai_dinh_dang and adds the
// intentionally rejected Grafana-relative forms.
func TestResolveTime_Invalid(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	values := []string{
		"yesterday",
		"now-",
		"now-1",
		"now-1x",
		"now-0h",
		"now--1h",
		"now-1h/h",
		"now/d",
		"1.5h",
		"2026-10-07",
		"2026-13-07T10:00:00Z",
		"12:00:00",
		"now-36028797018967568s", // 2^55+3600 seconds wraps to 1h without the bound
		"now-9223372036854775807w",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			_, err := ResolveTime(value, now)
			if err == nil {
				t.Fatalf("ResolveTime(%q) must fail", value)
			}
			want := `time must be "now", "now-<n><s|m|h|d|w>", epoch ms or RFC3339, got "` + value + `"`
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestParseStep(t *testing.T) {
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"60", time.Minute},
		{"1", time.Second},
		{"24h", 24 * time.Hour},
		{"90m", 90 * time.Minute},
	}
	for _, tc := range ok {
		t.Run("ok/"+tc.in, func(t *testing.T) {
			got, err := ParseStep(tc.in)
			if err != nil {
				t.Fatalf("ParseStep(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseStep(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}

	bad := []string{"-5s", "0", "abc", "500ms", "25h", "1.5s", "86401", "36028797018963969"}
	for _, in := range bad {
		t.Run("bad/"+in, func(t *testing.T) {
			_, err := ParseStep(in)
			if err == nil {
				t.Fatalf("ParseStep(%q) must fail", in)
			}
			want := `step must be a whole number of seconds between 1s and 24h (e.g. 30s, 5m, 60), got "` + in + `"`
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestAutoStep(t *testing.T) {
	from := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		to   time.Time
		want time.Duration
	}{
		{"15m", from.Add(15 * time.Minute), 3 * time.Second},
		{"1h", from.Add(time.Hour), 12 * time.Second},
		{"24h", from.Add(24 * time.Hour), 288 * time.Second},
		{"2m", from.Add(2 * time.Minute), time.Second},
		{"6h", from.Add(6 * time.Hour), 72 * time.Second},
		{"empty window", from, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutoStep(from, tc.to); got != tc.want {
				t.Fatalf("AutoStep = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFormatTime(t *testing.T) {
	got := FormatTime(1791374400123)
	want := "2026-10-07T12:00:00.123Z"
	if got != want {
		t.Fatalf("FormatTime = %q, want %q", got, want)
	}
}
