package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tokens and addresses are assembled at run time: a literal deny-listed string
// in this file would make the repository's own scanner fail.

func TestDenylist_Default(t *testing.T) {
	d := Default()

	if len(d.Patterns()) != len(defaultPatterns) {
		t.Fatalf("Patterns() = %v, want the %d built-ins", d.Patterns(), len(defaultPatterns))
	}

	hits := []struct {
		name  string
		value string
	}{
		{"private 10/8", "host " + "10" + "." + "1" + "." + "2" + "." + "3" + " seen"},
		{"private 172.16/12", "host " + "172" + ".16." + "5.5" + " seen"},
		{"private 192.168/16", "host " + "192" + ".168." + "1.10" + " seen"},
		{"cgnat 100.64/10 low", "host " + "100" + ".64." + "0.1" + " seen"},
		{"cgnat 100.64/10 high", "host " + "100" + ".127." + "9.9" + " seen"},
		{"unique local ipv6", "host " + "fc" + "00::1" + " seen"},
		{"sa token", "glsa" + "_" + strings.Repeat("a", 12)},
		{"cloud token", "glc" + "_" + strings.Repeat("b", 12)},
		{"bearer header", "Authorization" + ": " + "Bearer " + strings.Repeat("c", 12)},
	}
	for _, tc := range hits {
		t.Run(tc.name, func(t *testing.T) {
			pattern, ok := d.Match(tc.value)
			if !ok {
				t.Fatalf("Match(%q) found nothing", tc.value)
			}
			if !strings.Contains(strings.Join(defaultPatterns, "\n"), pattern) {
				t.Fatalf("Match returned %q, which is not a built-in pattern", pattern)
			}
		})
	}

	misses := []string{
		`{app="demo"} |= "error"`,
		"level=error msg=\"boom\"",
		"192.168.1",     // not a full address
		"100.63.1.1",    // just below 100.64/10
		"100.128.1.1",   // just above 100.64/10
		"glsa_short",    // below the token length floor
		"Authorization", // no credential attached
	}
	for _, value := range misses {
		if pattern, ok := d.Match(value); ok {
			t.Fatalf("Match(%q) matched %q, want no match", value, pattern)
		}
	}
}

func TestDenylist_WithFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denylist.local")
	content := "# a comment\n\nacme\ncontoso\\.internal\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := Default()
	builtinCount := len(d.Patterns())
	if err := d.WithFile(path); err != nil {
		t.Fatalf("WithFile: %v", err)
	}

	patterns := d.Patterns()
	if len(patterns) != builtinCount+2 {
		t.Fatalf("Patterns() = %v, want %d built-ins plus 2 file patterns", patterns, builtinCount)
	}
	if patterns[builtinCount] != "acme" || patterns[builtinCount+1] != `contoso\.internal` {
		t.Fatalf("file patterns out of order: %v", patterns[builtinCount:])
	}

	if pattern, ok := d.Match("ACME corp"); !ok || pattern != "acme" {
		t.Fatalf("Match(%q) = %q, %v; want the file pattern, case-insensitively", "ACME corp", pattern, ok)
	}
	if pattern, ok := d.Match("host contoso.internal seen"); !ok || pattern != `contoso\.internal` {
		t.Fatalf("a regexp line must match too: %q, %v", pattern, ok)
	}
	if _, ok := d.Match("nothing to see " + `{app="demo"}`); ok {
		t.Fatalf("file patterns replaced the built-ins")
	}
	if _, ok := d.Match("a " + "10" + "." + "9" + "." + "9.9" + " b"); !ok {
		t.Fatalf("WithFile must not disturb the built-ins")
	}
}

func TestDenylist_WithFileErrors(t *testing.T) {
	d := Default()

	if err := d.WithFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("a missing file must be an error")
	}

	path := filepath.Join(t.TempDir(), "bad.local")
	if err := os.WriteFile(path, []byte("([unclosed\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := d.WithFile(path); err == nil {
		t.Fatalf("an invalid pattern must be an error")
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain text", `{app="demo"}`, false},
		{"private v4", "10" + "." + "1.2.3", true},
		{"private v4 in sentence", "cpu acme-vm at " + "192" + ".168." + "4.4" + " now", true},
		{"public v4", "8" + "." + "8.8.8", true},
		{"loopback v4", "127" + ".0.0.1", false},
		{"loopback v4 with port", "127" + ".0.0.1:8000", false},
		{"loopback v6", "::1", false},
		{"loopback v6 in brackets", "[::1]:8000", false},
		{"unique local v6", "[" + "fc" + "00::1]" + ":8000", true},
		{"clock is not an address", "12:34:56", false},
		{"version string", "go1.25", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPrivateIP(tc.in); got != tc.want {
				t.Fatalf("IsPrivateIP(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
