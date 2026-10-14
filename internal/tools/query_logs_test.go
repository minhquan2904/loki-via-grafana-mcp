package tools

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/grafana"
)

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

func TestQueryLogs_StepIsTrimmed(t *testing.T) {
	fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(metricFrameJSON)}}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "query_logs", map[string]any{"datasource_uid": "loki-demo", "logql": "{}", "step": " 30s "})
	if res.IsError {
		t.Fatalf("tool error: %s", toolErrorText(t, res))
	}
	if got := firstQueryOf(t, fake.lastQuery(t))["step"]; got != "30s" {
		t.Fatalf("step = %v, want 30s", got)
	}
}

func TestQueryLogs_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		// 11000 x 1s fits, 11001 x 1s does not.
		{"11000 points", map[string]any{"start": "now-11000s", "step": "1s"}, ""},
		{"11001 points", map[string]any{"start": "now-11001s", "step": "1s"}, "step too small for this window: at most 11000 points per series"},
		// 31 days is allowed, one millisecond more is not.
		{"31 days", map[string]any{"start": "now-31d", "step": "1h"}, ""},
		{"31 days + 1ms", map[string]any{"start": "1788695999999", "end": "1791374400000", "step": "1h"}, "time window must not exceed 31 days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(metricFrameJSON)}}
			cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})
			args := map[string]any{"datasource_uid": "loki-demo", "logql": "{}"}
			for k, v := range tc.args {
				args[k] = v
			}
			res := callTool(t, cs, "query_logs", args)
			if tc.wantErr == "" {
				if res.IsError {
					t.Fatalf("tool error: %s", toolErrorText(t, res))
				}
				return
			}
			if got := toolErrorText(t, res); got != tc.wantErr {
				t.Fatalf("error = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestQueryLogs_DebugLogCarriesLogQLOnFailure(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fake := &fakeGrafana{list: lokiList(), queryErr: errors.New("Loki error: boom")}
	cs := newSession(t, Deps{Grafana: fake, Log: log, Now: func() time.Time { return testNow }})

	callTool(t, cs, "query_logs", map[string]any{"datasource_uid": "loki-demo", "logql": `{app="demo"}`})

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if n := strings.Count(out, `"msg":"tool call"`); n != 1 {
		t.Fatalf("got %d tool call records, want 1: %s", n, out)
	}
	if !strings.Contains(out, `"logql":"{app=\"demo\"}"`) || !strings.Contains(out, `"ok":false`) {
		t.Fatalf("failed call does not carry logql: %s", out)
	}
}

func TestQueryLogs_InfoLogOmitsLogQL(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	fake := &fakeGrafana{list: lokiList(), frames: []json.RawMessage{json.RawMessage(logFrameJSON)}}
	cs := newSession(t, Deps{Grafana: fake, Log: log, Now: func() time.Time { return testNow }})

	callTool(t, cs, "query_logs", map[string]any{"datasource_uid": "loki-demo", "logql": `{app="demo"}`})

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "logql") || strings.Contains(buf.String(), "boom") {
		t.Fatalf("info log carries the query or log content: %s", buf.String())
	}
}

// lockedWriter serializes writes from the server goroutine and the test.
type lockedWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
