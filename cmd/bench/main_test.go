package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/redact"
)

// TestStats_NearestRank pins the quantile method the report depends on.
func TestStats_NearestRank(t *testing.T) {
	samples := []float64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}

	cases := []struct {
		q    float64
		want float64
	}{
		{0.50, 5},
		{0.95, 10},
		{1.00, 10},
		{0.10, 1},
	}
	for _, tc := range cases {
		if got := nearestRank(samples, tc.q); got != tc.want {
			t.Fatalf("nearestRank(q=%v) = %v, want %v", tc.q, got, tc.want)
		}
	}
	if got := nearestRank(nil, 0.5); got != 0 {
		t.Fatalf("nearestRank(nil) = %v, want 0", got)
	}
	if got := maxOf(samples); got != 10 {
		t.Fatalf("maxOf = %v, want 10", got)
	}

	// The caller's slice must survive untouched: the report reads it again.
	if samples[0] != 10 || samples[9] != 5 {
		t.Fatalf("nearestRank reordered the caller's samples: %v", samples)
	}
}

func TestBuildRow(t *testing.T) {
	got := buildRow("go", "S1", 1, []sample{
		{latencyMS: 10, respBytes: 100, lines: 5},
		{latencyMS: 20, respBytes: 200, lines: 6},
		{latencyMS: 30, respBytes: 300, lines: 7, failed: true, throttled: true},
	})

	if got.Target != "go" || got.Scenario != "S1" || got.Concurrency != 1 {
		t.Fatalf("identity fields = %+v", got)
	}
	if got.N != 3 || got.Errors != 1 || got.Throttled != 1 {
		t.Fatalf("counters = %+v", got)
	}
	if got.P50MS != 20 || got.P95MS != 30 || got.MaxMS != 30 {
		t.Fatalf("latency = %+v", got)
	}
	if got.LinesP50 != 6 {
		t.Fatalf("lines p50 = %v, want 6", got.LinesP50)
	}
	if got.RPS <= 0 {
		t.Fatalf("rps = %v, want a positive rate", got.RPS)
	}
}

func TestCheckErrorRate(t *testing.T) {
	if err := checkErrorRate(Row{N: 10, Errors: 0}, 0.05); err != nil {
		t.Fatalf("a clean run must pass: %v", err)
	}
	if err := checkErrorRate(Row{N: 10, Errors: 2}, 0.05); err == nil {
		t.Fatalf("an error rate above the limit must stop the run")
	}
	if err := checkErrorRate(Row{N: 0}, 0.05); err == nil {
		t.Fatalf("a run without samples must stop")
	}
}

func TestParseScenarios(t *testing.T) {
	got, err := parseScenarios(" S1 , S3 ,S4 ")
	if err != nil {
		t.Fatalf("parseScenarios: %v", err)
	}
	if strings.Join(got, ",") != "S1,S3,S4" {
		t.Fatalf("scenarios = %v", got)
	}

	for _, value := range []string{"", "S9", "S1,S9"} {
		if _, err := parseScenarios(value); err == nil {
			t.Fatalf("parseScenarios(%q) must fail", value)
		}
	}
}

// TestScenarioArgs pins the arguments contract C8 prescribes per scenario.
func TestScenarioArgs(t *testing.T) {
	opts := options{dsUID: "loki-demo", selector: `{app="demo"}`, filter: "error"}

	s1 := scenarioArgs("S1", opts)
	if s1["start"] != "now-15m" || s1["limit"] != 100 || s1["logql"] != `{app="demo"}` {
		t.Fatalf("S1 args = %v", s1)
	}
	if _, hasStep := s1["step"]; hasStep {
		t.Fatalf("S1 must not pin a step: %v", s1)
	}

	s2 := scenarioArgs("S2", opts)
	if s2["start"] != "now-1h" || s2["limit"] != 500 || s2["logql"] != `{app="demo"} |= "error"` {
		t.Fatalf("S2 args = %v", s2)
	}

	s3 := scenarioArgs("S3", opts)
	if s3["logql"] != `sum(count_over_time({app="demo"}[5m]))` {
		t.Fatalf("S3 logql = %v", s3["logql"])
	}
	if _, hasLimit := s3["limit"]; hasLimit {
		t.Fatalf("S3 must not send a log limit: %v", s3)
	}

	// S0 always talks to the fake Grafana, whose datasource is fixed.
	s0 := scenarioArgs("S0", options{dsUID: "ignored", selector: `{app="demo"}`})
	if s0["datasource_uid"] != "loki-demo" {
		t.Fatalf("S0 uid = %v, want loki-demo", s0["datasource_uid"])
	}
}

// TestFakeQueryResponse builds the S0 payload and checks it is clean.
func TestFakeQueryResponse(t *testing.T) {
	raw := fakeQueryResponse()

	var parsed struct {
		Results map[string]struct {
			Frames []struct {
				Data struct {
					Values [][]any `json:"values"`
				} `json:"data"`
			} `json:"frames"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the fake response is not JSON: %v", err)
	}
	frames := parsed.Results["A"].Frames
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	columns := frames[0].Data.Values
	if len(columns) != 3 || len(columns[2]) != fakeLogRows {
		t.Fatalf("columns = %d, line column = %d, want 3 and %d", len(columns), len(columns[2]), fakeLogRows)
	}

	denylist := redact.Default()
	if pattern, ok := denylist.Match(string(raw)); ok {
		t.Fatalf("the fake payload matches the deny pattern %q", pattern)
	}
	if redact.IsPrivateIP(string(raw)) {
		t.Fatalf("the fake payload carries an address")
	}
}

func TestFakeGrafanaHandler(t *testing.T) {
	srv := httptest.NewServer(fakeGrafanaHandler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/datasources")
	if err != nil {
		t.Fatalf("GET /api/datasources: %v", err)
	}
	defer res.Body.Close()
	var datasources []struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(res.Body).Decode(&datasources); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(datasources) != 1 || datasources[0].UID != "loki-demo" || datasources[0].Type != "loki" {
		t.Fatalf("datasources = %+v", datasources)
	}

	res, err = http.Post(srv.URL+"/api/ds/query", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/ds/query: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	res, err = http.Get(srv.URL + "/api/ds/query")
	if err != nil {
		t.Fatalf("GET /api/ds/query: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}

func TestReadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("sa-token-abc123\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := readToken(path)
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "sa-token-abc123" {
		t.Fatalf("token = %q, want the trimmed value", got)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readToken(empty); err == nil {
		t.Fatalf("an empty token file must be an error")
	}
	if _, err := readToken(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("a missing token file must be an error")
	}
}

// TestParseOptions covers the required inputs: nothing is hardcoded, everything
// comes from flags or the documented environment variables.
func TestParseOptions(t *testing.T) {
	env := []string{"BENCH_DS_UID=loki-demo", "BENCH_SELECTOR={app=\"demo\"}", "BENCH_FILTER=error"}
	args := []string{"--target", "go=http://127.0.0.1:18101", "--target", "python=http://127.0.0.1:18102",
		"--token-file", "/tmp/token", "--out", "/tmp/rows.jsonl", "--scenarios", "S1,S2"}

	opts, err := parseOptions(args, env)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if len(opts.targets) != 2 || opts.targets[0].name != "go" || opts.targets[1].name != "python" {
		t.Fatalf("targets = %+v", opts.targets)
	}
	if opts.warmup != 3 || opts.iterations != 30 || opts.concurrencyS4 != 10 || opts.roundsS4 != 20 {
		t.Fatalf("defaults drifted: %+v", opts)
	}
	if opts.maxErrorRate != 0.05 {
		t.Fatalf("max error rate = %v, want 0.05", opts.maxErrorRate)
	}

	bad := []struct {
		name string
		args []string
		env  []string
	}{
		{"no out", args[:len(args)-4], env},
		{"bad target", []string{"--target", "go", "--token-file", "/tmp/token", "--out", "/tmp/rows.jsonl"}, env},
		{"bad name", []string{"--target", "GO=http://x", "--token-file", "/tmp/token", "--out", "/tmp/rows.jsonl"}, env},
		{"no selector", args, []string{"BENCH_DS_UID=loki-demo"}},
		{"S2 without filter", []string{"--target", "go=http://x", "--token-file", "/tmp/token", "--out", "/tmp/rows.jsonl", "--scenarios", "S2"},
			[]string{"BENCH_DS_UID=loki-demo", "BENCH_SELECTOR={app=\"demo\"}"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOptions(tc.args, tc.env); err == nil {
				t.Fatalf("parseOptions must fail for %s", tc.name)
			}
		})
	}
}
