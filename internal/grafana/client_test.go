package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testToken deliberately avoids the built-in denylist shapes (glsa_/glc_): it
// only has to be a value the client must never echo.
const testToken = "sa-token-abc123"

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(server.URL, testToken, 5*time.Second, nil)
}

// TestDSQuery_HTTPAndLokiErrors ports test_loi_http_va_loi_loki_thanh_tool_error:
// a non-200 response and a Loki error inside a 200 both become errors.
func TestDSQuery_HTTPAndLokiErrors(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
		})

		_, err := client.DSQuery(context.Background(), map[string]any{})
		if err == nil {
			t.Fatalf("expected an error")
		}
		if got := err.Error(); got != "Grafana /api/ds/query 403: Forbidden" {
			t.Fatalf("error = %q", got)
		}
	})

	t.Run("loki error on 200", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"results":{"A":{"error":"parse error at line 1"}}}`))
		})

		_, err := client.DSQuery(context.Background(), map[string]any{})
		if err == nil {
			t.Fatalf("expected an error")
		}
		if got := err.Error(); got != "Loki error: parse error at line 1" {
			t.Fatalf("error = %q", got)
		}
	})
}

// TestDSQuery_ReturnsFrames: results.A.frames passes through, and a result with
// no frames is empty rather than nil.
func TestDSQuery_ReturnsFrames(t *testing.T) {
	t.Run("frames", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"results":{"A":{"frames":[{"schema":{"fields":[]},"data":{"values":[]}}]}}}`))
		})

		frames, err := client.DSQuery(context.Background(), map[string]any{})
		if err != nil {
			t.Fatalf("DSQuery: %v", err)
		}
		if len(frames) != 1 {
			t.Fatalf("frames = %v, want 1", frames)
		}
	})

	t.Run("no frames", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"results":{"A":{}}}`))
		})

		frames, err := client.DSQuery(context.Background(), map[string]any{})
		if err != nil {
			t.Fatalf("DSQuery: %v", err)
		}
		if frames == nil || len(frames) != 0 {
			t.Fatalf("frames = %#v, want an empty slice", frames)
		}
	})
}

// TestClient_ErrorNeverLeaksToken feeds the token back inside an error body: the
// client must scrub it before rendering the message.
func TestClient_ErrorNeverLeaksToken(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		payload, _ := json.Marshal(map[string]string{"message": r.Header.Get("Authorization")})
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(payload)
	})

	_, err := client.DSQuery(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error does not mark the redaction: %q", err.Error())
	}
}

// TestClient_RefusesRedirect: a redirect must not be followed, and the message
// must name the host only (contract C2.4, RT-2).
func TestClient_RefusesRedirect(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://sso.example.com/login?next=/api/ds/query")
		w.WriteHeader(http.StatusFound)
	})

	_, err := client.DSQuery(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	want := "Grafana /api/ds/query 302: redirect to sso.example.com refused (check GRAFANA_URL / proxy auth)"
	if got := err.Error(); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// TestClient_RequestHeaders pins the header set of every Grafana call.
func TestClient_RequestHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = w.Write([]byte(`{"results":{"A":{"frames":[]}}}`))
	})

	if _, err := client.DSQuery(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("DSQuery: %v", err)
	}

	header := <-seen
	if got := header.Get("User-Agent"); got != "loki-via-grafana-mcp/"+Version {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// TestClient_ResponseTooLarge stops reading at the cap instead of buffering.
func TestClient_ResponseTooLarge(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseBytes+1))
	})

	_, err := client.DSQuery(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := err.Error(); got != "Grafana response exceeds 32 MiB" {
		t.Fatalf("error = %q", got)
	}
}

// TestListDatasources_FiltersLoki keeps only loki entries, ordered by name, and
// drops the type field from the output shape.
func TestListDatasources_FiltersLoki(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"uid":"loki-b","name":"zeta","type":"loki","isDefault":false},
			{"uid":"prom-1","name":"aaa","type":"prometheus","isDefault":false},
			{"uid":"loki-a","name":"alpha","type":"loki","isDefault":true},
			{"uid":"graph-1","name":"bbb","type":"graphite","isDefault":false}
		]`))
	})

	got, err := client.ListDatasources(context.Background())
	if err != nil {
		t.Fatalf("ListDatasources: %v", err)
	}
	want := []Datasource{
		{UID: "loki-a", Name: "alpha", IsDefault: true},
		{UID: "loki-b", Name: "zeta"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("datasources = %#v, want %#v", got, want)
	}
}

func TestListDatasources_HTTPError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})

	_, err := client.ListDatasources(context.Background())
	if err == nil {
		t.Fatalf("expected an error")
	}
	if got := err.Error(); got != "Grafana /api/datasources 500: boom" {
		t.Fatalf("error = %q", got)
	}
}

// TestClient_ExactlyTwoMethods is the structural read-only lock: a broadly
// scoped service account token must not be usable through a generic request
// method, so the exported API stays at two calls.
func TestClient_ExactlyTwoMethods(t *testing.T) {
	typ := reflect.TypeOf(&Client{})
	if typ.NumMethod() != 2 {
		t.Fatalf("Client exposes %d methods, want exactly 2", typ.NumMethod())
	}
	names := make(map[string]bool, typ.NumMethod())
	for i := range typ.NumMethod() {
		names[typ.Method(i).Name] = true
	}
	want := map[string]bool{"DSQuery": true, "ListDatasources": true}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("methods = %v, want %v", names, want)
	}
}

// TestNewClient_SetsCheckRedirect covers both construction paths: a caller
// supplied client gets the redirect policy forced on it.
func TestNewClient_SetsCheckRedirect(t *testing.T) {
	supplied := &http.Client{}
	client := NewClient("https://grafana.test", testToken, time.Second, supplied)
	if supplied.CheckRedirect == nil {
		t.Fatalf("supplied client kept a nil CheckRedirect")
	}
	if client.httpClient != supplied {
		t.Fatalf("supplied client was not used")
	}

	fresh := NewClient("https://grafana.test/", testToken, time.Second, nil)
	if fresh.httpClient.CheckRedirect == nil {
		t.Fatalf("new client has no redirect policy")
	}
	if fresh.baseURL != "https://grafana.test" {
		t.Fatalf("baseURL = %q, want the trailing slash trimmed", fresh.baseURL)
	}
}
