package tools

import (
	"context"
	"encoding/json"
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
