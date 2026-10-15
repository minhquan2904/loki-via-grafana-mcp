package grafana

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// frameStart is the first timestamp of every fixture: 2025-10-07T01:20:00Z.
const frameStart = 1759800000000

func loadFrame(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read frame %s: %v", name, err)
	}
	return json.RawMessage(raw)
}

func parseOne(t *testing.T, name string) Result {
	t.Helper()
	result, err := ParseFrames([]json.RawMessage{loadFrame(t, name)})
	if err != nil {
		t.Fatalf("ParseFrames(%s): %v", name, err)
	}
	return result
}

// TestFinish_BackwardNewestFirst ports test_log_frame_backward_moi_nhat_truoc.
func TestFinish_BackwardNewestFirst(t *testing.T) {
	result := parseOne(t, "frame_log_basic.json")
	result.Finish("backward", 100)

	lines := make([]string, 0, len(result.Logs))
	for _, entry := range result.Logs {
		lines = append(lines, entry.Line)
	}
	want := []string{`level=info msg="second"`, `level=error msg="boom"`}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	if result.Type != "logs" {
		t.Fatalf("type = %q", result.Type)
	}
	if len(result.Series) != 0 {
		t.Fatalf("a log frame must not yield series: %v", result.Series)
	}
	if got := result.Logs[0].Labels; !reflect.DeepEqual(got, map[string]string{"app": "test", "env": "test"}) {
		t.Fatalf("string-encoded labels decoded wrong: %v", got)
	}
	if got := result.Logs[1].Labels; !reflect.DeepEqual(got, map[string]string{"app": "demo"}) {
		t.Fatalf("object labels decoded wrong: %v", got)
	}
	if result.Logs[1].Time != "2025-10-07T01:20:00.000Z" {
		t.Fatalf("time = %q", result.Logs[1].Time)
	}
}

// TestParseFrames_DataplaneTimestampBody ports
// test_log_frame_dataplane_timestamp_body.
func TestParseFrames_DataplaneTimestampBody(t *testing.T) {
	result := parseOne(t, "frame_log_dataplane.json")
	result.Finish("forward", 1)

	if len(result.Logs) != 1 || result.Logs[0].Line != `level=error msg="boom"` {
		t.Fatalf("logs = %v, want the oldest line only", result.Logs)
	}
	if result.Logs[0].Time != "2025-10-07T01:20:00.000Z" {
		t.Fatalf("time = %q", result.Logs[0].Time)
	}
}

// TestParseFrames_MetricSeries ports test_metric_frame_tra_series, plus the nil
// value and missing-labels cases.
func TestParseFrames_MetricSeries(t *testing.T) {
	result := parseOne(t, "frame_metric.json")

	if result.Type != "series" {
		t.Fatalf("type = %q, want series", result.Type)
	}
	if len(result.Logs) != 0 {
		t.Fatalf("a metric frame must not yield logs: %v", result.Logs)
	}
	want := []Series{
		{
			Labels: map[string]string{"app": "demo", "env": "test"},
			Values: [][2]any{
				{"2025-10-07T01:20:00.000Z", float64(3)},
				{"2025-10-07T01:20:01.000Z", nil},
			},
		},
		{
			Labels: map[string]string{},
			Values: [][2]any{
				{"2025-10-07T01:20:00.000Z", nil},
				{"2025-10-07T01:20:01.000Z", float64(7)},
			},
		},
	}
	if !reflect.DeepEqual(result.Series, want) {
		t.Fatalf("series = %#v", result.Series)
	}
}

// TestParseFrames_LongLineTruncated ports test_dong_qua_dai_bi_cat.
func TestParseFrames_LongLineTruncated(t *testing.T) {
	long := strings.Repeat("x", MaxLineChars+10)
	frame := fmt.Sprintf(`{"schema":{"fields":[{"name":"labels","type":"other"},{"name":"Time","type":"time"},{"name":"Line","type":"string"}]},`+
		`"data":{"values":[[{"app":"demo"}],[%d],["%s"]]}}`, frameStart, long)

	result, err := ParseFrames([]json.RawMessage{json.RawMessage(frame)})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("logs = %v", result.Logs)
	}
	entry := result.Logs[0]
	if !entry.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if got := len([]rune(entry.Line)); got != MaxLineChars {
		t.Fatalf("kept %d runes, want %d", got, MaxLineChars)
	}
}

// TestParseFrames_EmptyLogs pins the empty shape: {"type":"logs","logs":[]}.
func TestParseFrames_EmptyLogs(t *testing.T) {
	result, err := ParseFrames(nil)
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	if result.Type != "logs" || result.Logs == nil || len(result.Logs) != 0 {
		t.Fatalf("result = %#v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"type":"logs","logs":[]}` {
		t.Fatalf("json = %s", raw)
	}
}

// TestParseFrames_BrokenFrame fails loudly instead of guessing.
func TestParseFrames_BrokenFrame(t *testing.T) {
	_, err := ParseFrames([]json.RawMessage{json.RawMessage(`{"schema":`)})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !strings.HasPrefix(err.Error(), "unexpected Grafana frame: ") {
		t.Fatalf("error = %q", err.Error())
	}
}

// TestParseFrames_LabelForms covers the encodings a labels cell can use.
func TestParseFrames_LabelForms(t *testing.T) {
	cases := []struct {
		name string
		cell string
		want map[string]string
	}{
		{"object", `{"app":"demo"}`, map[string]string{"app": "demo"}},
		{"json string", `"{\"app\":\"demo\"}"`, map[string]string{"app": "demo"}},
		{"broken string", `"not json"`, map[string]string{}},
		{"string array", `"[1,2]"`, map[string]string{}},
		{"null", `null`, map[string]string{}},
		{"non string values", `{"app":5,"env":true}`, map[string]string{"app": "5", "env": "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := fmt.Sprintf(`{"schema":{"fields":[{"name":"labels","type":"other"},{"name":"Time","type":"time"},{"name":"Line","type":"string"}]},`+
				`"data":{"values":[[%s],[%d],["level=error msg=\"boom\""]]}}`, tc.cell, frameStart)

			result, err := ParseFrames([]json.RawMessage{json.RawMessage(frame)})
			if err != nil {
				t.Fatalf("ParseFrames: %v", err)
			}
			if len(result.Logs) != 1 {
				t.Fatalf("logs = %v", result.Logs)
			}
			if got := result.Logs[0].Labels; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFinish_ForwardAndLimit pins ordering and the cut.
func TestFinish_ForwardAndLimit(t *testing.T) {
	result := parseOne(t, "frame_log_basic.json")
	result.Finish("forward", 100)
	if result.Logs[0].Line != `level=error msg="boom"` {
		t.Fatalf("forward must be oldest first, got %v", result.Logs[0])
	}
	result.Finish("forward", 1)
	if len(result.Logs) != 1 {
		t.Fatalf("limit not applied: %v", result.Logs)
	}
}

// TestFinish_SeriesUntouched: Finish orders and cuts logs only (contract C2.3).
func TestFinish_SeriesUntouched(t *testing.T) {
	result := parseOne(t, "frame_metric.json")
	before := fmt.Sprintf("%#v", result.Series)
	result.Finish("backward", 1)
	if after := fmt.Sprintf("%#v", result.Series); after != before {
		t.Fatalf("Finish mutated series: %s", after)
	}
}

// rawFrame builds a frame from field specs and columns for the ragged cases.
func rawFrame(t *testing.T, fields []map[string]any, values [][]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": map[string]any{"fields": fields},
		"data":   map[string]any{"values": values},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseFrames_LogFrameWithoutTimeColumn(t *testing.T) {
	frame := rawFrame(t,
		[]map[string]any{{"name": "Line", "type": "string"}},
		[][]any{{"a", "b"}},
	)
	result, err := ParseFrames([]json.RawMessage{frame})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Logs) != 2 || result.Logs[0].Time != "" || result.Logs[1].Line != "b" {
		t.Fatalf("logs = %+v, want 2 lines with empty time", result.Logs)
	}
}

func TestParseFrames_LogFrameShortTimeColumn(t *testing.T) {
	frame := rawFrame(t,
		[]map[string]any{{"name": "Time", "type": "time"}, {"name": "Line", "type": "string"}},
		[][]any{{frameStart}, {"a", "b", "c"}},
	)
	result, err := ParseFrames([]json.RawMessage{frame})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2025-10-07T01:20:00.000Z", "", ""}
	for i, entry := range result.Logs {
		if entry.Time != want[i] {
			t.Errorf("logs[%d].Time = %q, want %q", i, entry.Time, want[i])
		}
	}
	if len(result.Logs) != 3 {
		t.Fatalf("got %d logs, want 3", len(result.Logs))
	}
}

func TestParseFrames_MetricFrameRaggedColumns(t *testing.T) {
	frame := rawFrame(t,
		[]map[string]any{{"name": "Time", "type": "time"}, {"name": "Value", "type": "number"}},
		[][]any{{frameStart, "not-a-time"}, {1, 2, 3}},
	)
	result, err := ParseFrames([]json.RawMessage{frame})
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "series" || len(result.Series) != 1 {
		t.Fatalf("result = %+v, want one series", result)
	}
	values := result.Series[0].Values
	if len(values) != 1 || values[0][0] != "2025-10-07T01:20:00.000Z" || values[0][1] != 1.0 {
		t.Fatalf("values = %v, want only the first row", values)
	}
}

func TestParseFrames_NaNStringBecomesNull(t *testing.T) {
	frame := rawFrame(t,
		[]map[string]any{{"name": "Time", "type": "time"}, {"name": "Value", "type": "number"}},
		[][]any{{frameStart, frameStart + 1000}, {"NaN", "+Inf"}},
	)
	result, err := ParseFrames([]json.RawMessage{frame})
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range result.Series[0].Values {
		if point[1] != nil {
			t.Errorf("value = %v, want nil", point[1])
		}
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatalf("result does not encode: %v", err)
	}
}

// TestParseFrames_MultibyteTruncation cuts on rune boundaries: exactly
// MaxLineChars runes stay whole, one more is cut to valid UTF-8.
func TestParseFrames_MultibyteTruncation(t *testing.T) {
	for _, tc := range []struct {
		runes     int
		truncated bool
	}{
		{MaxLineChars, false},
		{MaxLineChars + 1, true},
	} {
		line := strings.Repeat("é", tc.runes)
		frame := fmt.Sprintf(`{"schema":{"fields":[{"name":"Time","type":"time"},{"name":"Line","type":"string"}]},`+
			`"data":{"values":[[%d],["%s"]]}}`, frameStart, line)
		result, err := ParseFrames([]json.RawMessage{json.RawMessage(frame)})
		if err != nil {
			t.Fatalf("ParseFrames: %v", err)
		}
		entry := result.Logs[0]
		if entry.Truncated != tc.truncated {
			t.Fatalf("%d runes: truncated = %v, want %v", tc.runes, entry.Truncated, tc.truncated)
		}
		if got := len([]rune(entry.Line)); got != MaxLineChars {
			t.Fatalf("%d runes: kept %d, want %d", tc.runes, got, MaxLineChars)
		}
		if !utf8.ValidString(entry.Line) {
			t.Fatalf("%d runes: the kept line is not valid UTF-8", tc.runes)
		}
	}
}

// TestParseFrames_MixedLogAndMetric keeps the logs and drops the series, as the
// reference implementation does.
func TestParseFrames_MixedLogAndMetric(t *testing.T) {
	logFrame := fmt.Sprintf(`{"schema":{"fields":[{"name":"Time","type":"time"},{"name":"Line","type":"string"}]},`+
		`"data":{"values":[[%d],["boom"]]}}`, frameStart)
	metricFrame := fmt.Sprintf(`{"schema":{"fields":[{"name":"Time","type":"time"},{"name":"Value","type":"number"}]},`+
		`"data":{"values":[[%d],[3]]}}`, frameStart)

	result, err := ParseFrames([]json.RawMessage{json.RawMessage(metricFrame), json.RawMessage(logFrame)})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	if result.Type != "logs" || len(result.Logs) != 1 {
		t.Fatalf("type = %q, logs = %v; want one log line", result.Type, result.Logs)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"series"`) {
		t.Fatalf("a log result must not carry series: %s", body)
	}
}
