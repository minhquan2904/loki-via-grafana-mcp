package grafana

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// MaxLineChars is the maximum number of runes kept from a single log line.
const MaxLineChars = 4000

// LogLine is a single log entry.
type LogLine struct {
	Time      string            `json:"time"`
	Line      string            `json:"line"`
	Labels    map[string]string `json:"labels"`
	Truncated bool              `json:"truncated,omitempty"`
}

// Series is a single metric series.
type Series struct {
	Labels map[string]string `json:"labels"`
	Values [][2]any          `json:"values"`
}

// Result is the tool output: either log lines or metric series.
type Result struct {
	Type   string    `json:"type"`
	Logs   []LogLine `json:"logs,omitempty"`
	Series []Series  `json:"series,omitempty"`
}

// MarshalJSON always emits the array belonging to Type, so an empty log result
// serialises as {"type":"logs","logs":[]} rather than {"type":"logs"}.
func (r Result) MarshalJSON() ([]byte, error) {
	if r.Type == "series" {
		series := r.Series
		if series == nil {
			series = []Series{}
		}
		return json.Marshal(struct {
			Type   string   `json:"type"`
			Series []Series `json:"series"`
		}{Type: "series", Series: series})
	}
	logs := r.Logs
	if logs == nil {
		logs = []LogLine{}
	}
	return json.Marshal(struct {
		Type string    `json:"type"`
		Logs []LogLine `json:"logs"`
	}{Type: "logs", Logs: logs})
}

// frameField is one entry of a dataframe schema.
type frameField struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Labels map[string]any `json:"labels"`
}

// frame is the subset of a Grafana dataframe this server understands.
type frame struct {
	Schema struct {
		Fields []frameField `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]json.RawMessage `json:"values"`
	} `json:"data"`
}

// ParseFrames converts the frames of results.A into a Result.
func ParseFrames(frames []json.RawMessage) (Result, error) {
	var result Result
	for _, raw := range frames {
		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			return Result{}, fmt.Errorf("unexpected Grafana frame: %w", err)
		}
		if logs, ok := parseLogFrame(f); ok {
			result.Logs = append(result.Logs, logs...)
			continue
		}
		result.Series = append(result.Series, parseMetricFrame(f)...)
	}

	if len(result.Series) > 0 && len(result.Logs) == 0 {
		result.Type = "series"
	} else {
		result.Type = "logs"
		if result.Logs == nil {
			result.Logs = []LogLine{}
		}
	}
	return result, nil
}

// parseLogFrame reads the log rows of f. ok is false when f has no line column,
// in which case ParseFrames treats the frame as a metric frame.
func parseLogFrame(f frame) ([]LogLine, bool) {
	lineIdx := fieldIndex(f, func(field frameField) bool {
		return field.Name == "Line" || field.Name == "body"
	})
	if lineIdx < 0 {
		return nil, false
	}
	timeIdx := fieldIndex(f, func(field frameField) bool { return field.Type == "time" })
	labelsIdx := fieldIndex(f, func(field frameField) bool { return field.Name == "labels" })

	rows := len(column(f, lineIdx))
	logs := make([]LogLine, 0, rows)
	for i := range rows {
		line := decodeString(column(f, lineIdx)[i])
		truncated := false
		if runes := []rune(line); len(runes) > MaxLineChars {
			line = string(runes[:MaxLineChars])
			truncated = true
		}
		time, _ := decodeTime(rawAt(f, timeIdx, i))
		logs = append(logs, LogLine{
			Time:      time,
			Line:      line,
			Labels:    parseLabels(rawAt(f, labelsIdx, i)),
			Truncated: truncated,
		})
	}
	return logs, true
}

// parseMetricFrame reads the numeric fields of f as series.
func parseMetricFrame(f frame) []Series {
	timeIdx := fieldIndex(f, func(field frameField) bool { return field.Type == "time" })
	if timeIdx < 0 {
		return nil
	}
	var series []Series
	for idx, field := range f.Schema.Fields {
		if field.Type != "number" {
			continue
		}
		// Columns may be ragged; pair rows only while both have a cell, and skip
		// rows whose time is not a number rather than inventing a 1970 point.
		col := column(f, idx)
		rows := min(len(col), len(column(f, timeIdx)))
		values := make([][2]any, 0, rows)
		for i := range rows {
			time, ok := decodeTime(rawAt(f, timeIdx, i))
			if !ok {
				continue
			}
			values = append(values, [2]any{time, decodeValue(col[i])})
		}
		series = append(series, Series{
			Labels: labelsToStrings(field.Labels),
			Values: values,
		})
	}
	return series
}

// Finish sorts logs by time (newest first for "backward", oldest first for
// "forward") and cuts the list to limit. Series are untouched.
func (r *Result) Finish(direction string, limit int) {
	sort.SliceStable(r.Logs, func(i, j int) bool {
		if direction == "forward" {
			return r.Logs[i].Time < r.Logs[j].Time
		}
		return r.Logs[i].Time > r.Logs[j].Time
	})
	if limit > 0 && len(r.Logs) > limit {
		r.Logs = r.Logs[:limit]
	}
}

// fieldIndex returns the index of the first field matching pred, or -1.
func fieldIndex(f frame, pred func(frameField) bool) int {
	for i, field := range f.Schema.Fields {
		if pred(field) {
			return i
		}
	}
	return -1
}

// column returns the data column for field index idx, tolerating a missing one.
func column(f frame, idx int) []json.RawMessage {
	if idx < 0 || idx >= len(f.Data.Values) {
		return nil
	}
	return f.Data.Values[idx]
}

// rawAt returns the raw value at column idx, row i, or nil when out of range.
func rawAt(f frame, idx, i int) json.RawMessage {
	col := column(f, idx)
	if i < 0 || i >= len(col) {
		return nil
	}
	return col[i]
}

// decodeString renders a raw value as a string; non-strings use fmt.Sprint and
// JSON null becomes the empty string.
func decodeString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// decodeTime renders a raw epoch-milliseconds cell with FormatTime. A missing
// or non-numeric cell yields "" and false.
func decodeTime(raw json.RawMessage) (string, bool) {
	if f, ok := decodeValue(raw).(float64); ok {
		return FormatTime(f), true
	}
	return "", false
}

// decodeValue renders a raw value as a float64 or nil.
func decodeValue(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	switch typed := v.(type) {
	case float64:
		return typed
	case string:
		// "NaN" and "Inf" parse but cannot be encoded as JSON numbers.
		if f, err := strconv.ParseFloat(typed, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		return nil
	default:
		return nil
	}
}

// parseLabels turns a labels cell into a label map. The cell may be a JSON
// object or a string containing one; anything else yields an empty map.
func parseLabels(raw json.RawMessage) map[string]string {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]string{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]string{}
	}
	if s, ok := v.(string); ok {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(s), &decoded); err != nil {
			return map[string]string{}
		}
		return labelsToStrings(decoded)
	}
	if obj, ok := v.(map[string]any); ok {
		return labelsToStrings(obj)
	}
	return map[string]string{}
}

// labelsToStrings renders a label object as map[string]string, using fmt.Sprint
// for non-string values. A nil object yields an empty map.
func labelsToStrings(labels map[string]any) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		if s, ok := value.(string); ok {
			out[key] = s
			continue
		}
		out[key] = fmt.Sprint(value)
	}
	return out
}
