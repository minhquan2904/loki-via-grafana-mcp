package grafana

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestBuildPayload_DSQueryLokiDatasource ports
// test_payload_dung_ds_query_va_datasource_loki: the body must address a loki
// datasource through /api/ds/query and pass the caller's query through.
func TestBuildPayload_DSQueryLokiDatasource(t *testing.T) {
	from := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	got := BuildPayload(QueryParams{
		DatasourceUID: "loki-demo",
		LogQL:         `{app="demo"} |= "error"`,
		From:          from,
		To:            from.Add(15 * time.Minute),
		Limit:         100,
		Direction:     "backward",
		Step:          3 * time.Second,
	})

	want := map[string]any{
		"from": "1791374400000",
		"to":   "1791375300000",
		"queries": []any{
			map[string]any{
				"refId":         "A",
				"datasource":    map[string]any{"type": "loki", "uid": "loki-demo"},
				"expr":          `{app="demo"} |= "error"`,
				"queryType":     "range",
				"maxLines":      float64(100),
				"direction":     "backward",
				"step":          "3s",
				"intervalMs":    float64(3000),
				"maxDataPoints": float64(300),
			},
		},
	}
	assertSameJSON(t, got, want)
	assertGoldenJSON(t, got, "testdata/payload_basic.json")
}

// TestBuildPayload_Step pins the resolution controls the Python version lacked:
// both "step" and "intervalMs" travel with the query, and maxDataPoints follows
// the requested window instead of Grafana's default.
func TestBuildPayload_Step(t *testing.T) {
	from := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	got := BuildPayload(QueryParams{
		DatasourceUID: "loki-demo",
		LogQL:         `sum(count_over_time({app="demo"}[5m]))`,
		From:          from,
		To:            from.Add(time.Hour),
		Limit:         500,
		Direction:     "forward",
		Step:          30 * time.Second,
	})

	query := firstQuery(t, got)
	if query["step"] != "30s" {
		t.Fatalf(`step = %v, want "30s"`, query["step"])
	}
	if query["intervalMs"] != float64(30000) {
		t.Fatalf("intervalMs = %v, want 30000", query["intervalMs"])
	}
	if query["maxDataPoints"] != float64(120) {
		t.Fatalf("maxDataPoints = %v, want 120", query["maxDataPoints"])
	}
	assertGoldenJSON(t, got, "testdata/payload_step_30s.json")
}

// jsonValue round-trips v through JSON so numbers compare as float64.
func jsonValue(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out
}

// assertSameJSON compares two values after a JSON round trip, matching the
// comparison contract C2.2 prescribes.
func assertSameJSON(t *testing.T, got, want any) {
	t.Helper()
	gotValue := jsonValue(t, got)
	wantValue := jsonValue(t, want)
	if !reflect.DeepEqual(gotValue, wantValue) {
		gotRaw, _ := json.Marshal(gotValue)
		wantRaw, _ := json.Marshal(wantValue)
		t.Fatalf("value mismatch\n got: %s\nwant: %s", gotRaw, wantRaw)
	}
}

func assertGoldenJSON(t *testing.T, got any, path string) {
	t.Helper()
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	var want any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatalf("golden %s is not JSON: %v", path, err)
	}
	assertSameJSON(t, got, want)
}

func firstQuery(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	value, ok := jsonValue(t, payload).(map[string]any)
	if !ok {
		t.Fatalf("payload is not a JSON object")
	}
	queries, ok := value["queries"].([]any)
	if !ok || len(queries) != 1 {
		t.Fatalf("queries = %v, want exactly one entry", value["queries"])
	}
	query, ok := queries[0].(map[string]any)
	if !ok {
		t.Fatalf("query is not a JSON object")
	}
	return query
}
