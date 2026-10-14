// Command bench measures both MCP servers — this Go build and the Python
// reference implementation — over the same Loki datasource. Every call goes
// through the MCP tool, so the numbers include MCP overhead rather than talking
// to Grafana directly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// restBetweenTargets is the pause contract C8 asks for between the two targets
// of S4, so the upstream does not see one continuous burst.
const restBetweenTargets = 30 * time.Second

type target struct {
	name string
	url  string
}

type options struct {
	targets       []target
	tokenFile     string
	scenarios     []string
	warmup        int
	iterations    int
	concurrencyS4 int
	roundsS4      int
	fakeGrafana   string
	maxErrorRate  float64
	out           string

	dsUID    string
	selector string
	filter   string
}

var targetNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// targetList collects the repeated --target flag.
type targetList []target

func (l *targetList) String() string {
	parts := make([]string, 0, len(*l))
	for _, tgt := range *l {
		parts = append(parts, tgt.name+"="+tgt.url)
	}
	return strings.Join(parts, ",")
}

func (l *targetList) Set(value string) error {
	name, url, ok := strings.Cut(value, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("--target wants name=URL, got %q", value)
	}
	if !targetNameRE.MatchString(name) {
		return fmt.Errorf("--target name must match [a-z0-9-]+, got %q", name)
	}
	*l = append(*l, target{name: name, url: strings.TrimRight(url, "/")})
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(args []string, environ []string) error {
	opts, err := parseOptions(args, environ)
	if err != nil {
		return err
	}
	if opts.fakeGrafana != "" {
		return serveFakeGrafana(opts.fakeGrafana)
	}

	token, err := readToken(opts.tokenFile)
	if err != nil {
		return err
	}

	ctx := context.Background()
	var rows []Row
	for _, scenario := range opts.scenarios {
		var measured []Row
		if scenario == "S4" {
			measured, err = measureConcurrent(ctx, opts, token)
		} else {
			measured, err = measureSequential(ctx, opts, scenario, token)
		}
		if err != nil {
			return err
		}
		rows = append(rows, measured...)
	}
	return writeRows(opts.out, rows)
}

func parseOptions(args, environ []string) (options, error) {
	env := map[string]string{}
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}

	var opts options
	var targets targetList
	scenarios := "S1,S2,S3,S4"

	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Var(&targets, "target", "name=URL of an MCP server; repeat for each target (names go, python)")
	flags.StringVar(&opts.tokenFile, "token-file", "", "file holding the bearer token every target shares")
	flags.StringVar(&scenarios, "scenarios", scenarios, "comma separated S0|S1|S2|S3|S4 (S0 runs against --fake-grafana)")
	flags.IntVar(&opts.warmup, "warmup", 3, "warm-up calls per target, dropped from the statistics")
	flags.IntVar(&opts.iterations, "iterations", 30, "measured calls per target per scenario")
	flags.IntVar(&opts.concurrencyS4, "concurrency-s4", 10, "parallel clients in S4")
	flags.IntVar(&opts.roundsS4, "rounds-s4", 20, "rounds per client in S4")
	flags.StringVar(&opts.fakeGrafana, "fake-grafana", "", "serve a fake Grafana on this address instead of benchmarking")
	flags.Float64Var(&opts.maxErrorRate, "max-error-rate", 0.05, "stop with exit 1 above this error rate")
	flags.StringVar(&opts.out, "out", "", "path of the rows.jsonl to write")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}

	opts.targets = targets
	opts.dsUID = strings.TrimSpace(env["BENCH_DS_UID"])
	opts.selector = strings.TrimSpace(env["BENCH_SELECTOR"])
	opts.filter = strings.TrimSpace(env["BENCH_FILTER"])
	if opts.out == "" {
		return options{}, errors.New("--out is required")
	}
	if opts.tokenFile == "" {
		return options{}, errors.New("--token-file is required")
	}
	if len(opts.targets) == 0 {
		return options{}, errors.New("at least one --target is required")
	}
	if opts.dsUID == "" || opts.selector == "" {
		return options{}, errors.New("BENCH_DS_UID and BENCH_SELECTOR must be set")
	}

	parsed, err := parseScenarios(scenarios)
	if err != nil {
		return options{}, err
	}
	opts.scenarios = parsed
	for _, scenario := range parsed {
		if scenario == "S2" && opts.filter == "" {
			return options{}, errors.New("BENCH_FILTER must be set for S2")
		}
	}
	return opts, nil
}

func parseScenarios(value string) ([]string, error) {
	var scenarios []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch part {
		case "S0", "S1", "S2", "S3", "S4":
			scenarios = append(scenarios, part)
		default:
			return nil, fmt.Errorf("unknown scenario %q", part)
		}
	}
	if len(scenarios) == 0 {
		return nil, errors.New("--scenarios selected nothing")
	}
	return scenarios, nil
}

func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the token file is empty")
	}
	return token, nil
}

// measureSequential runs S0-S3: every round calls the targets in alternating
// order, so caching and load drift hit both sides equally.
func measureSequential(ctx context.Context, opts options, scenario, token string) ([]Row, error) {
	args := scenarioArgs(scenario, opts)
	workers := make(map[string]*worker, len(opts.targets))
	for _, tgt := range opts.targets {
		w, err := newWorker(ctx, tgt, token)
		if err != nil {
			return nil, err
		}
		defer w.close()
		workers[tgt.name] = w
	}

	samples := make(map[string][]sample, len(opts.targets))
	total := opts.warmup + opts.iterations
	for round := range total {
		order := opts.targets
		if round%2 == 1 {
			order = reversed(opts.targets)
		}
		for _, tgt := range order {
			got, err := workers[tgt.name].call(ctx, args)
			if err != nil {
				return nil, err
			}
			if round < opts.warmup {
				continue
			}
			samples[tgt.name] = append(samples[tgt.name], got)
		}
	}

	rows := make([]Row, 0, len(opts.targets))
	for _, tgt := range opts.targets {
		row := buildRow(tgt.name, scenario, 1, samples[tgt.name])
		if err := checkErrorRate(row, opts.maxErrorRate); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// measureConcurrent runs S4: one group of clients per target, with a rest
// between the targets so the upstream does not see one continuous burst.
func measureConcurrent(ctx context.Context, opts options, token string) ([]Row, error) {
	args := scenarioArgs("S4", opts)

	var rows []Row
	for i, tgt := range opts.targets {
		if i > 0 {
			time.Sleep(restBetweenTargets)
		}

		perWorker := make([][]sample, opts.concurrencyS4)
		var wg sync.WaitGroup
		var firstErr error
		var errOnce sync.Once

		for workerIndex := range opts.concurrencyS4 {
			wg.Add(1)
			go func(workerIndex int) {
				defer wg.Done()
				w, err := newWorker(ctx, tgt, token)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				defer w.close()

				for range opts.roundsS4 {
					got, err := w.call(ctx, args)
					if err != nil {
						errOnce.Do(func() { firstErr = err })
						return
					}
					perWorker[workerIndex] = append(perWorker[workerIndex], got)
				}
			}(workerIndex)
		}
		wg.Wait()
		if firstErr != nil {
			return nil, firstErr
		}

		var samples []sample
		for _, got := range perWorker {
			samples = append(samples, got...)
		}
		row := buildRow(tgt.name, "S4", opts.concurrencyS4, samples)
		if err := checkErrorRate(row, opts.maxErrorRate); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// sample is one measured MCP tool call.
type sample struct {
	latencyMS float64
	respBytes int
	lines     int
	points    int
	throttled bool
	failed    bool
}

func buildRow(target, scenario string, concurrency int, samples []sample) Row {
	latencies := make([]float64, 0, len(samples))
	sizes := make([]float64, 0, len(samples))
	lineCounts := make([]float64, 0, len(samples))
	var failures, throttled, points int
	var elapsed float64

	for _, got := range samples {
		latencies = append(latencies, got.latencyMS)
		sizes = append(sizes, float64(got.respBytes))
		lineCounts = append(lineCounts, float64(got.lines))
		elapsed += got.latencyMS
		if got.failed {
			failures++
		}
		if got.throttled {
			throttled++
		}
		if got.points > points {
			points = got.points
		}
	}

	row := Row{
		Target:             target,
		Scenario:           scenario,
		Concurrency:        concurrency,
		N:                  len(samples),
		Errors:             failures,
		Throttled:          throttled,
		P50MS:              round3(nearestRank(latencies, 0.50)),
		P95MS:              round3(nearestRank(latencies, 0.95)),
		MaxMS:              round3(maxOf(latencies)),
		RespBytesP50:       nearestRank(sizes, 0.50),
		PointsPerSeriesMax: points,
		LinesP50:           nearestRank(lineCounts, 0.50),
	}
	if elapsed > 0 {
		row.RPS = round3(float64(len(samples)) / (elapsed / 1000))
	}
	return row
}

func checkErrorRate(row Row, maxRate float64) error {
	if row.N == 0 {
		return fmt.Errorf("%s/%s measured nothing", row.Target, row.Scenario)
	}
	if rate := float64(row.Errors) / float64(row.N); rate > maxRate {
		return fmt.Errorf("%s/%s error rate %.2f exceeds the limit %.2f", row.Target, row.Scenario, rate, maxRate)
	}
	return nil
}

// worker keeps one MCP session so a round does not pay for the handshake.
type worker struct {
	session *mcp.ClientSession
	name    string
}

func newWorker(ctx context.Context, tgt target, token string) (*worker, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "loki-via-grafana-mcp-bench", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             tgt.url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: connect: %w", tgt.name, err)
	}
	return &worker{session: session, name: tgt.name}, nil
}

func (w *worker) close() {
	_ = w.session.Close()
}

func (w *worker) call(ctx context.Context, args map[string]any) (sample, error) {
	start := time.Now()
	res, err := w.session.CallTool(ctx, &mcp.CallToolParams{Name: "query_logs", Arguments: args})
	elapsed := time.Since(start)
	if err != nil {
		return sample{}, fmt.Errorf("%s: call: %w", w.name, err)
	}

	text := textOf(res)
	got := sample{
		latencyMS: float64(elapsed.Microseconds()) / 1000,
		respBytes: len(text),
	}
	if res.IsError {
		got.failed = true
		got.throttled = strings.Contains(text, "429")
		return got, nil
	}
	got.lines, got.points = decodeCounts(text)
	return got, nil
}

type toolResult struct {
	Type   string     `json:"type"`
	Logs   []struct{} `json:"logs"`
	Series []struct {
		Values [][2]any `json:"values"`
	} `json:"series"`
}

// decodeCounts reads the line count and the widest series out of the tool
// result, so the report can show what a query actually returned.
func decodeCounts(text string) (lines, points int) {
	var parsed toolResult
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return 0, 0
	}
	lines = len(parsed.Logs)
	for _, series := range parsed.Series {
		if len(series.Values) > points {
			points = len(series.Values)
		}
	}
	return lines, points
}

func textOf(res *mcp.CallToolResult) string {
	var text strings.Builder
	for _, content := range res.Content {
		if part, ok := content.(*mcp.TextContent); ok {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func scenarioArgs(scenario string, opts options) map[string]any {
	uid := opts.dsUID
	if scenario == "S0" {
		// S0 runs against the fake Grafana, whose only datasource is fixed.
		uid = "loki-demo"
	}
	switch scenario {
	case "S0", "S1", "S4":
		return map[string]any{
			"datasource_uid": uid,
			"logql":          opts.selector,
			"start":          "now-15m",
			"limit":          100,
		}
	case "S2":
		return map[string]any{
			"datasource_uid": uid,
			"logql":          opts.selector + ` |= "` + opts.filter + `"`,
			"start":          "now-1h",
			"limit":          500,
		}
	case "S3":
		return map[string]any{
			"datasource_uid": uid,
			"logql":          "sum(count_over_time(" + opts.selector + "[5m]))",
			"start":          "now-1h",
		}
	}
	return nil
}

func writeRows(path string, rows []Row) error {
	var out strings.Builder
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		out.Write(raw)
		out.WriteString("\n")
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

func reversed(targets []target) []target {
	flipped := make([]target, len(targets))
	for i, tgt := range targets {
		flipped[len(targets)-1-i] = tgt
	}
	return flipped
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}

// bearerTransport adds the shared bearer token to every MCP request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
