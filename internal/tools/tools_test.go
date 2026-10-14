package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/grafana"
)

// The frames below are hand-written synthetic data: no response from a real
// Grafana is ever copied into this repository (contract C0).
const (
	logFrameJSON = `{"schema":{"fields":[{"name":"labels","type":"other"},{"name":"Time","type":"time"},` +
		`{"name":"Line","type":"string"}]},"data":{"values":[[{"app":"demo"}],[1759800000000],` +
		`["level=error msg=\"boom\""]]}}`
	metricFrameJSON = `{"schema":{"fields":[{"name":"Time","type":"time"},` +
		`{"name":"Value","type":"number","labels":{"app":"demo"}}]},"data":{"values":[[1759800000000],[3]]}}`
)

// testNow is 2026-10-07T12:00:00Z; the default window is now-1h .. now.
var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

const (
	testNowMS  = "1791374400000"
	testFromMS = "1791370800000"
)

func lokiList() []grafana.Datasource {
	return []grafana.Datasource{
		{UID: "loki-demo", Name: "demo"},
		{UID: "loki-test", Name: "test", IsDefault: true},
	}
}

// fakeGrafana records what the handlers asked for.
type fakeGrafana struct {
	mu        sync.Mutex
	queries   []map[string]any
	listCalls int
	frames    []json.RawMessage
	queryErr  error
	listErr   error
	list      []grafana.Datasource
}

func (f *fakeGrafana) DSQuery(_ context.Context, payload map[string]any) ([]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, payload)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.frames, nil
}

func (f *fakeGrafana) ListDatasources(_ context.Context) ([]grafana.Datasource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeGrafana) queryCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeGrafana) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeGrafana) lastQuery(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatalf("no query reached Grafana")
	}
	return f.queries[len(f.queries)-1]
}

// firstQueryOf returns queries[0] of the recorded payload.
func firstQueryOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	queries, ok := payload["queries"].([]any)
	if !ok || len(queries) != 1 {
		t.Fatalf("queries = %v, want one entry", payload["queries"])
	}
	query, ok := queries[0].(map[string]any)
	if !ok {
		t.Fatalf("query = %v, want an object", queries[0])
	}
	return query
}

func newSession(t *testing.T, d Deps) *mcp.ClientSession {
	t.Helper()
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "loki-via-grafana-mcp", Version: "test"}, nil)
	Register(srv, d)

	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): protocol error: %v", name, err)
	}
	return res
}

func toolErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an IsError result, got %+v", res)
	}
	var text strings.Builder
	for _, content := range res.Content {
		if part, ok := content.(*mcp.TextContent); ok {
			text.WriteString(part.Text)
		}
	}
	if text.Len() == 0 {
		t.Fatalf("IsError result carried no text: %+v", res)
	}
	return text.String()
}

// structured decodes the structured content of a successful result.
func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", toolErrorText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return out
}

// TestQueryLogs_InvalidInputNoGrafanaCall ports
// test_input_sai_bi_chan_truoc_khi_goi_grafana: every rejection happens before
// the first Grafana call.
func TestQueryLogs_InvalidInputNoGrafanaCall(t *testing.T) {
	cases := []struct {
		name         string
		args         map[string]any
		want         string // exact message our own validation produces
		wantContains string // substring for rejections go-sdk's schema makes first
	}{
		{
			name:         "no uid",
			args:         map[string]any{"logql": `{app="demo"}`},
			wantContains: "datasource_uid",
		},
		{
			name: "blank uid",
			args: map[string]any{"datasource_uid": "   ", "logql": `{app="demo"}`},
			want: "datasource_uid and logql are required",
		},
		{
			name: "blank logql",
			args: map[string]any{"datasource_uid": "loki-demo", "logql": "   "},
			want: "datasource_uid and logql are required",
		},
		{
			name:         "no logql",
			args:         map[string]any{"datasource_uid": "loki-demo"},
			wantContains: "logql",
		},
		{
			name: "bad direction",
			args: map[string]any{"datasource_uid": "loki-demo", "logql": "{}", "direction": "sideways"},
			want: `direction must be "backward" or "forward"`,
		},
		{
			name: "bad step",
			args: map[string]any{"datasource_uid": "loki-demo", "logql": "{}", "step": "nope"},
			want: `step must be a whole number of seconds between 1s and 24h (e.g. 30s, 5m, 60), got "nope"`,
		},
		{
			name: "start after end",
			args: map[string]any{
				"datasource_uid": "loki-demo", "logql": "{}",
				"start": "2026-10-07T12:00:00Z", "end": "2026-10-07T11:00:00Z",
			},
			want: "start must be before end",
		},
		{
			name: "window too wide",
			args: map[string]any{
				"datasource_uid": "loki-demo", "logql": "{}",
				"start": "2026-08-01T00:00:00Z", "end": "2026-10-07T12:00:00Z",
			},
			want: "time window must not exceed 31 days",
		},
		{
			name: "step too small",
			args: map[string]any{
				"datasource_uid": "loki-demo", "logql": "{}",
				"start": "now-20d", "end": "now", "step": "1s",
			},
			want: "step too small for this window: at most 11000 points per series",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGrafana{list: lokiList()}
			cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

			res := callTool(t, cs, "query_logs", tc.args)
			got := toolErrorText(t, res)
			switch {
			case tc.want != "":
				if got != tc.want {
					t.Fatalf("error = %q, want %q", got, tc.want)
				}
			case tc.wantContains != "":
				if !strings.Contains(got, tc.wantContains) {
					t.Fatalf("error = %q, want it to mention %q", got, tc.wantContains)
				}
			default:
				t.Fatalf("case %s asserts nothing", tc.name)
			}
			if calls := fake.queryCalls(); calls != 0 {
				t.Fatalf("Grafana was queried %d times", calls)
			}
			if calls := fake.listCallCount(); calls != 0 {
				t.Fatalf("the datasource list was read %d times", calls)
			}
		})
	}
}

// TestQueryLogs_LimitClamped ports test_limit_bi_kep.
func TestQueryLogs_LimitClamped(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want int
	}{
		{"absent", map[string]any{}, 100},
		{"zero", map[string]any{"limit": 0}, 1},
		{"negative", map[string]any{"limit": -3}, 1},
		{"at the ceiling", map[string]any{"limit": 500}, 500},
		{"above the ceiling", map[string]any{"limit": 9999}, 500},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGrafana{list: lokiList()}
			cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

			args := map[string]any{"datasource_uid": "loki-demo", "logql": `{app="demo"}`}
			for key, value := range tc.args {
				args[key] = value
			}
			callTool(t, cs, "query_logs", args)

			query := firstQueryOf(t, fake.lastQuery(t))
			got, ok := query["maxLines"].(int)
			if !ok {
				t.Fatalf("maxLines = %v (%T)", query["maxLines"], query["maxLines"])
			}
			if got != tc.want {
				t.Fatalf("maxLines = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestQueryLogs_Success asserts the default window, the payload and the shape of
// the result.
func TestQueryLogs_Success(t *testing.T) {
	fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(logFrameJSON)}}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "query_logs", map[string]any{
		"datasource_uid": "loki-demo",
		"logql":          `{app="demo"}`,
	})
	out := structured[grafana.Result](t, res)

	if out.Type != "logs" || len(out.Logs) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.Logs[0].Line != `level=error msg="boom"` {
		t.Fatalf("line = %q", out.Logs[0].Line)
	}
	if out.Logs[0].Labels["app"] != "demo" {
		t.Fatalf("labels = %v", out.Logs[0].Labels)
	}

	payload := fake.lastQuery(t)
	if payload["from"] != testFromMS {
		t.Fatalf("from = %v, want the default now-1h (%s)", payload["from"], testFromMS)
	}
	if payload["to"] != testNowMS {
		t.Fatalf("to = %v, want the default now (%s)", payload["to"], testNowMS)
	}
	query := firstQueryOf(t, payload)
	if query["direction"] != "backward" {
		t.Fatalf("direction = %v, want the default backward", query["direction"])
	}
	if query["expr"] != `{app="demo"}` {
		t.Fatalf("expr = %v", query["expr"])
	}
}

// TestQueryLogs_MetricSeries covers the series result type and the step path.
func TestQueryLogs_MetricSeries(t *testing.T) {
	fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(metricFrameJSON)}}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "query_logs", map[string]any{
		"datasource_uid": "loki-demo",
		"logql":          `sum(count_over_time({app="demo"}[5m]))`,
		"start":          "now-15m",
	})
	out := structured[grafana.Result](t, res)

	if out.Type != "series" || len(out.Series) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.Series[0].Labels["app"] != "demo" {
		t.Fatalf("series labels = %v", out.Series[0].Labels)
	}
	query := firstQueryOf(t, fake.lastQuery(t))
	if query["step"] != "3s" {
		t.Fatalf("step = %v, want the auto step for a 15 minute window (3s)", query["step"])
	}
}

// TestQueryLogs_GrafanaErrorIsToolError asserts the message reaches the caller
// as a tool error and is cut to the contract limit.
func TestQueryLogs_GrafanaErrorIsToolError(t *testing.T) {
	fake := &fakeGrafana{list: lokiList(), queryErr: errors.New(strings.Repeat("x", 700))}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "query_logs", map[string]any{
		"datasource_uid": "loki-demo",
		"logql":          "{}",
	})
	got := toolErrorText(t, res)
	if len([]rune(got)) != maxToolErrorRunes {
		t.Fatalf("message is %d runes, want %d", len([]rune(got)), maxToolErrorRunes)
	}
}

// TestQueryLogs_NotALokiDatasource is the read-only scope lock: a uid that is
// not a Loki datasource must be refused before any query goes out.
func TestQueryLogs_NotALokiDatasource(t *testing.T) {
	fake := &fakeGrafana{list: []grafana.Datasource{{UID: "prom-1", Name: "metrics"}}}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "query_logs", map[string]any{
		"datasource_uid": "loki-demo",
		"logql":          "{}",
	})
	want := `datasource_uid "loki-demo" is not a Loki datasource visible to this service account`
	if got := toolErrorText(t, res); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if calls := fake.queryCalls(); calls != 0 {
		t.Fatalf("Grafana was queried %d times", calls)
	}
}

// TestQueryLogs_DatasourceCacheReused: the list is read once for a burst of
// calls.
func TestQueryLogs_DatasourceCacheReused(t *testing.T) {
	fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(logFrameJSON)}}
	cs := newSession(t, Deps{
		Grafana:    fake,
		Now:        func() time.Time { return testNow },
		DSCacheTTL: time.Minute,
	})

	for range 3 {
		callTool(t, cs, "query_logs", map[string]any{"datasource_uid": "loki-demo", "logql": "{}"})
	}
	if calls := fake.listCallCount(); calls != 1 {
		t.Fatalf("the datasource list was read %d times, want 1", calls)
	}
}

// TestQueryLogs_CacheErrorNotReused: a failed list must be retried, not cached.
func TestQueryLogs_CacheErrorNotReused(t *testing.T) {
	fake := &fakeGrafana{listErr: errors.New("Grafana /api/datasources 500: boom")}
	cs := newSession(t, Deps{
		Grafana:    fake,
		Now:        func() time.Time { return testNow },
		DSCacheTTL: time.Minute,
	})

	for range 2 {
		res := callTool(t, cs, "query_logs", map[string]any{"datasource_uid": "loki-demo", "logql": "{}"})
		if got := toolErrorText(t, res); got != "Grafana /api/datasources 500: boom" {
			t.Fatalf("error = %q", got)
		}
	}
	if calls := fake.listCallCount(); calls != 2 {
		t.Fatalf("the datasource list was read %d times, want 2 (errors are not cached)", calls)
	}
}

// TestListDatasources_ReturnsTheVisibleList covers the second tool.
func TestListDatasources_ReturnsTheVisibleList(t *testing.T) {
	fake := &fakeGrafana{list: lokiList()}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "list_loki_datasources", nil)
	out := structured[ListDatasourcesOut](t, res)

	if len(out.Datasources) != 2 {
		t.Fatalf("datasources = %+v", out.Datasources)
	}
	if out.Datasources[0].UID != "loki-demo" || !out.Datasources[1].IsDefault {
		t.Fatalf("datasources = %+v", out.Datasources)
	}
	if calls := fake.queryCalls(); calls != 0 {
		t.Fatalf("listing datasources queried Grafana %d times", calls)
	}
}

// TestRegister_ExactlyTwoReadOnlyTools pins the exposed surface: two tools, both
// annotated read-only, with the contract descriptions.
func TestRegister_ExactlyTwoReadOnlyTools(t *testing.T) {
	cs := newSession(t, Deps{Grafana: &fakeGrafana{list: lokiList()}, Now: func() time.Time { return testNow }})

	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != 2 {
		t.Fatalf("exposed %d tools, want 2: %+v", len(list.Tools), list.Tools)
	}

	byName := make(map[string]*mcp.Tool, len(list.Tools))
	for _, tool := range list.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"query_logs", "list_loki_datasources"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tool %s is missing", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %s is not annotated read-only: %+v", name, tool.Annotations)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatalf("tool %s must not be destructive", name)
		}
		if !tool.Annotations.IdempotentHint {
			t.Fatalf("tool %s must be idempotent", name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("tool %s must not be open world", name)
		}
	}

	if got := byName["query_logs"].Description; got != queryLogsDescription {
		t.Fatalf("query_logs description drifted:\n%q", got)
	}
	if got := byName["list_loki_datasources"].Description; got != listDatasourcesDescription {
		t.Fatalf("list_loki_datasources description drifted:\n%q", got)
	}
}
