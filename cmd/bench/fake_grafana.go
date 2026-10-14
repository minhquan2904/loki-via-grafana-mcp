package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// fakeLogRows is how many log lines the fake Grafana returns; S0 measures the
// server overhead, so the payload must be identical on every call.
const fakeLogRows = 100

// fakeGrafanaHandler answers the two endpoints the MCP server uses. It is the S0
// baseline from contract C8: no upstream latency, no Loki, no credentials.
func fakeGrafanaHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"uid":"loki-demo","name":"demo","type":"loki","isDefault":true}]`))
	})
	mux.HandleFunc("/api/ds/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeQueryResponse())
	})
	return mux
}

// serveFakeGrafana runs the fake Grafana until the process is stopped.
func serveFakeGrafana(addr string) error {
	return http.ListenAndServe(addr, fakeGrafanaHandler())
}

// fakeQueryResponse builds one synthetic log frame of fakeLogRows rows. The data
// is generated here, never copied from a real Grafana response.
func fakeQueryResponse() []byte {
	labels := make([]any, 0, fakeLogRows)
	times := make([]any, 0, fakeLogRows)
	lines := make([]any, 0, fakeLogRows)

	for i := range fakeLogRows {
		labels = append(labels, `{"app":"demo","env":"test"}`)
		times = append(times, 1759800000000+i*1000)
		lines = append(lines, `level=error msg="boom" n=`+strconv.Itoa(i))
	}

	frame := map[string]any{
		"schema": map[string]any{
			"fields": []any{
				map[string]any{"name": "labels", "type": "other"},
				map[string]any{"name": "Time", "type": "time"},
				map[string]any{"name": "Line", "type": "string"},
			},
		},
		"data": map[string]any{"values": []any{labels, times, lines}},
	}
	body := map[string]any{
		"results": map[string]any{"A": map[string]any{"frames": []any{frame}}},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return []byte(`{"results":{"A":{"frames":[]}}}`)
	}
	return raw
}
