package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtures writes a minimal, clean bench output set.
func fixtures(t *testing.T) (rows, meta, extra string) {
	t.Helper()
	dir := t.TempDir()

	rows = filepath.Join(dir, "rows.jsonl")
	body := strings.Join([]string{
		`{"target":"go","scenario":"S0","concurrency":1,"n":30,"errors":0,"throttled":0,"p50_ms":1.1,"p95_ms":1.4,"max_ms":1.9,"rps":900,"resp_bytes_p50":4096,"points_per_series_max":0,"lines_p50":100}`,
		`{"target":"python","scenario":"S0","concurrency":1,"n":30,"errors":0,"throttled":0,"p50_ms":2.2,"p95_ms":2.6,"max_ms":3.1,"rps":450,"resp_bytes_p50":4096,"points_per_series_max":0,"lines_p50":100}`,
		`{"target":"go","scenario":"S3","concurrency":1,"n":30,"errors":0,"throttled":0,"p50_ms":12,"p95_ms":18,"max_ms":25,"rps":80,"resp_bytes_p50":10240,"points_per_series_max":300,"lines_p50":0}`,
		`{"target":"python","scenario":"S3","concurrency":1,"n":30,"errors":0,"throttled":0,"p50_ms":40,"p95_ms":55,"max_ms":70,"rps":25,"resp_bytes_p50":122880,"points_per_series_max":3600,"lines_p50":0}`,
	}, "\n")
	if err := os.WriteFile(rows, []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write rows: %v", err)
	}

	meta = filepath.Join(dir, "meta.json")
	if err := os.WriteFile(meta, []byte(`{"cpu_model":"Example CPU 9000","vcpu":8,"ram_gb":16,"os":"linux",`+
		`"docker_version":"29.1.3","go_version":"go1.25","gosdk_version":"v1.8.0","commit":"abcdef1234567890",`+
		`"timestamp":"2026-10-13T00:00:00Z","upstream":"a test Grafana + Loki"}`), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	extra = filepath.Join(dir, "extra.json")
	if err := os.WriteFile(extra, []byte(`{"image_size_mb":{"go":18.5,"python":210.2},`+
		`"cold_start_ms":{"go":12,"python":900},`+
		`"rss_mb":{"go":{"idle":9.5,"peak":28.1},"python":{"idle":85.4,"peak":190.7}}}`), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	return rows, meta, extra
}

func readmeWithMarkers(t *testing.T, before, after string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "README.md")
	content := before + benchStart + "\nstale table\n" + benchEnd + after
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	return path
}

// TestBenchreport_ReplacesOnlyBetweenMarkers: everything outside the markers
// survives byte for byte, and the table appears exactly once.
func TestBenchreport_ReplacesOnlyBetweenMarkers(t *testing.T) {
	rows, meta, extra := fixtures(t)
	before := "# Title\n\nintro line\n\n"
	after := "\n\nfooter line\n"
	readme := readmeWithMarkers(t, before, after)
	doc := filepath.Join(t.TempDir(), "benchmarks.md")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readme, "--doc", doc}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	updated, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.HasPrefix(string(updated), before+benchStart) {
		t.Fatalf("the text before the markers changed:\n%s", updated)
	}
	if !strings.HasSuffix(string(updated), benchEnd+after) {
		t.Fatalf("the text after the markers changed:\n%s", updated)
	}
	if strings.Contains(string(updated), "stale table") {
		t.Fatalf("the previous table was not replaced")
	}
	if got := strings.Count(string(updated), "| Scenario |"); got != 1 {
		t.Fatalf("the table appears %d times", got)
	}

	docBody, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if !strings.HasPrefix(string(docBody), docHeader) {
		t.Fatalf("docs/benchmarks.md must start with the generated header")
	}
	if !strings.Contains(string(docBody), "Measured on 8 vCPU Example CPU 9000, 16 GB RAM") {
		t.Fatalf("the closing line is missing:\n%s", docBody)
	}
	if !strings.Contains(string(docBody), "| S0 | 1.1 / 1.4 | 2.2 / 2.6 | 0 | 0 |\n") {
		t.Fatalf("the S0 row is wrong:\n%s", docBody)
	}
	if !strings.Contains(string(docBody), "| S3 points per series | 300 | 3600 |") {
		t.Fatalf("the S3 resource row is wrong:\n%s", docBody)
	}
}

// TestBenchreport_RefusesDenied: a denied value stops the run, names the field
// and never prints the value.
func TestBenchreport_RefusesDenied(t *testing.T) {
	t.Run("built-in pattern", func(t *testing.T) {
		rows, meta, extra := fixtures(t)
		if err := os.WriteFile(meta, []byte(`{"cpu_model":"host 10.`+`1.2.3","vcpu":8,"commit":"abcdef"}`), 0o644); err != nil {
			t.Fatalf("write meta: %v", err)
		}
		readme := readmeWithMarkers(t, "", "")
		doc := filepath.Join(t.TempDir(), "benchmarks.md")

		var stdout, stderr bytes.Buffer
		code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readme, "--doc", doc}, &stdout, &stderr)

		if code == 0 {
			t.Fatalf("the run must refuse")
		}
		if !strings.Contains(stderr.String(), "refusing to publish: field meta.cpu_model matches a denied pattern") {
			t.Fatalf("stderr = %q", stderr.String())
		}
		if strings.Contains(stderr.String(), "10."+"1.2.3") {
			t.Fatalf("the refused value was printed: %q", stderr.String())
		}
		content, err := os.ReadFile(readme)
		if err != nil {
			t.Fatalf("read readme: %v", err)
		}
		if !strings.Contains(string(content), "stale table") {
			t.Fatalf("the readme must not be touched when the run refuses")
		}
	})

	t.Run("redact file pattern", func(t *testing.T) {
		rows, meta, extra := fixtures(t)
		if err := os.WriteFile(meta, []byte(`{"cpu_model":"acme-vm 9000","vcpu":8,"commit":"abcdef"}`), 0o644); err != nil {
			t.Fatalf("write meta: %v", err)
		}
		redactFile := filepath.Join(t.TempDir(), "denylist.local")
		if err := os.WriteFile(redactFile, []byte("# comment\nacme\n"), 0o600); err != nil {
			t.Fatalf("write redact file: %v", err)
		}
		readme := readmeWithMarkers(t, "", "")
		doc := filepath.Join(t.TempDir(), "benchmarks.md")

		var stdout, stderr bytes.Buffer
		code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readme,
			"--doc", doc, "--redact-file", redactFile}, &stdout, &stderr)

		if code == 0 {
			t.Fatalf("the run must refuse")
		}
		if !strings.Contains(stderr.String(), "field meta.cpu_model") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

func TestReplaceBetweenMarkers_MissingMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(path, []byte("# no markers here\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := replaceBetweenMarkers(path, "body"); err == nil {
		t.Fatalf("a README without markers must be refused")
	}
}

// TestPrintDenylist is the single source the scanner reads.
func TestPrintDenylist(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--print-denylist"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 7 {
		t.Fatalf("printed %d patterns, want the built-in set", len(lines))
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("an empty pattern line was printed")
		}
	}
	if strings.Contains(stdout.String(), "refusing") {
		t.Fatalf("--print-denylist printed more than the patterns")
	}
}

// TestRun_RequiresEveryPath: the flags are mandatory, so a caller cannot render
// half a report.
func TestRun_RequiresEveryPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code == 0 {
		t.Fatalf("a run without flags must fail")
	}
	if !strings.Contains(stderr.String(), "required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBenchreport_UpstreamAndMissingScenarios(t *testing.T) {
	rows, meta, extra := fixtures(t)
	readme := readmeWithMarkers(t, "", "")
	doc := filepath.Join(t.TempDir(), "benchmarks.md")
	var stderr bytes.Buffer
	if code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readme, "--doc", doc}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	body, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"| Scenario | Go p50 / p95 (ms) | Python p50 / p95 (ms) | Go errors | Python errors |\n|---|---|---|---|---|\n",
		"server overhead only. S3 ran against a test Grafana + Loki. Not run: S1, S2, S4.",
		"GB RAM, against a test Grafana + Loki, commit",
		"| Image size, compressed (MB) |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("README lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Grafana Cloud") {
		t.Errorf("the report names a stack the meta did not:\n%s", text)
	}
	docBody, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(docBody), "## All measurements") || !strings.Contains(string(docBody), "| python | S3 | 1 | 30 |") {
		t.Errorf("docs/benchmarks.md lacks the full table:\n%s", docBody)
	}
}

func TestBenchreport_RequiresUpstream(t *testing.T) {
	rows, _, extra := fixtures(t)
	meta := filepath.Join(t.TempDir(), "meta.json")
	if err := os.WriteFile(meta, []byte(`{"cpu_model":"Example CPU","vcpu":4,"commit":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readmeWithMarkers(t, "", ""), "--doc", filepath.Join(t.TempDir(), "d.md")}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "meta.upstream is required") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}

func TestBenchreport_RSSNullIsNotMeasured(t *testing.T) {
	rows, meta, _ := fixtures(t)
	extra := filepath.Join(t.TempDir(), "extra.json")
	if err := os.WriteFile(extra, []byte(`{"rss_mb":{"go":{"idle":9.5,"peak":null},"python":{"idle":80,"peak":0}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	readme := readmeWithMarkers(t, "", "")
	var stderr bytes.Buffer
	if code := run([]string{"--rows", rows, "--meta", meta, "--extra", extra, "--readme", readme, "--doc", filepath.Join(t.TempDir(), "d.md")}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	body, _ := os.ReadFile(readme)
	if !strings.Contains(string(body), "| RSS peak under S4 (MB) | not measured | not measured |") {
		t.Fatalf("README:\n%s", body)
	}
}
