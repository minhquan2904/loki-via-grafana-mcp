package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "mcp-token-abc123"

// okHandler stands in for the MCP handler and counts how often it was reached.
type okHandler struct {
	mu    sync.Mutex
	calls int
}

func (h *okHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("mcp"))
}

func (h *okHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

var defaultHosts = []string{"127.0.0.1:*", "localhost:*", "[::1]:*"}

func newTestServer(t *testing.T, o Options) *httptest.Server {
	t.Helper()
	if o.MCP == nil {
		o.MCP = &okHandler{}
	}
	if o.Token == "" {
		o.Token = testToken
	}
	if o.AllowedHosts == nil {
		o.AllowedHosts = defaultHosts
	}
	srv := httptest.NewServer(New(o).Handler)
	t.Cleanup(srv.Close)
	return srv
}

func bearerRequest(t *testing.T, method, url, token string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// assertRefusal checks the shape every rejected request shares: the JSON body,
// the exact message and Connection: close.
func assertRefusal(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Connection"); got != "close" {
		t.Fatalf("Connection = %q, want close", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if body["error"] != msg {
		t.Fatalf("error = %q, want %q", body["error"], msg)
	}
}

// TestHostCheck covers every pattern form in contract C4.
func TestHostCheck(t *testing.T) {
	patterns := append(append([]string{}, defaultHosts...), "internal.example.test")

	cases := []struct {
		host string
		want bool
	}{
		{"localhost:8000", true},
		{"LOCALHOST:8000", true},
		{"localhost", false}, // the pattern demands a port
		{"127.0.0.1:1", true},
		{"127.0.0.2:1", false},
		{"[::1]:8000", true},
		{"[::2]:8000", false},
		{"internal.example.test", true},
		{"internal.example.test:8000", false}, // a bare pattern means no port
		{"evil.example.test", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := hostAllowed(patterns, tc.host); got != tc.want {
				t.Fatalf("hostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}

	t.Run("middleware stops the request", func(t *testing.T) {
		next := &okHandler{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = "evil.example.test"

		HostCheck(patterns, next).ServeHTTP(rec, req)

		assertRefusal(t, rec, http.StatusMisdirectedRequest, "host not allowed")
		if next.count() != 0 {
			t.Fatalf("the handler ran behind a refused host")
		}
	})
}

// TestRefuse_JSON pins the rejection contract, including the bearer challenge.
func TestRefuse_JSON(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong token", "Bearer " + "mcp-token-wrong"},
		{"wrong scheme", "Basic " + testToken},
		{"token only", testToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := &okHandler{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			BearerAuth(testToken, next).ServeHTTP(rec, req)

			assertRefusal(t, rec, http.StatusUnauthorized, "unauthorized")
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			if next.count() != 0 {
				t.Fatalf("the handler ran without a valid token")
			}
		})
	}
}

// TestBearerAuth_ConstantTime: the comparison hashes both sides, so a token of a
// different length is refused like any other mismatch.
func TestBearerAuth_ConstantTime(t *testing.T) {
	next := &okHandler{}
	rec := httptest.NewRecorder()
	req := bearerRequest(t, http.MethodPost, "/mcp", testToken, nil)

	BearerAuth(testToken, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || next.count() != 1 {
		t.Fatalf("valid token refused: status %d, handler calls %d", rec.Code, next.count())
	}
}

// TestOriginCheck: a browser origin must be allow-listed, a request without one
// passes.
func TestOriginCheck(t *testing.T) {
	allowed := []string{"https://internal.example.test"}

	cases := []struct {
		name    string
		origins []string
		want    int
	}{
		{"absent", nil, http.StatusOK},
		{"allowed", []string{"https://internal.example.test"}, http.StatusOK},
		{"not allowed", []string{"https://evil.example.test"}, http.StatusForbidden},
		{"two headers", []string{"https://internal.example.test", "https://evil.example.test"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := &okHandler{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			for _, origin := range tc.origins {
				req.Header.Add("Origin", origin)
			}

			OriginCheck(allowed, next).ServeHTTP(rec, req)

			if tc.want == http.StatusOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				return
			}
			assertRefusal(t, rec, tc.want, "origin not allowed")
			if next.count() != 0 {
				t.Fatalf("the handler ran behind a refused origin")
			}
		})
	}
}

func TestBodyLimit(t *testing.T) {
	t.Run("declared too large", func(t *testing.T) {
		next := &okHandler{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", 64)))

		BodyLimit(16, next).ServeHTTP(rec, req)

		assertRefusal(t, rec, http.StatusRequestEntityTooLarge, "request body too large")
		if next.count() != 0 {
			t.Fatalf("the handler ran on an oversized body")
		}
	})

	t.Run("within the limit", func(t *testing.T) {
		next := &okHandler{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))

		BodyLimit(16, next).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || next.count() != 1 {
			t.Fatalf("status = %d, handler calls = %d", rec.Code, next.count())
		}
	})
}

// TestLimit_ServerBusy: the second concurrent request waits, then gets 503 with
// Retry-After instead of piling onto Grafana.
func TestLimit_ServerBusy(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	done := make(chan struct{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	limited := limit(1, 20*time.Millisecond, inner)

	go func() {
		defer close(done)
		limited.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	}()
	<-entered

	rec := httptest.NewRecorder()
	limited.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	assertRefusal(t, rec, http.StatusServiceUnavailable, "server busy")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}

	close(release)
	<-done
}

// TestHealthz_NoAuth: the probe works without a token, and unknown paths 404.
func TestHealthz_NoAuth(t *testing.T) {
	mcp := &okHandler{}
	srv := newTestServer(t, Options{MCP: mcp, UserHeader: "X-Mcp-Gateway-User"})

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if string(body) != "ok\n" {
		t.Fatalf("body = %q, want %q", body, "ok\n")
	}

	t.Run("mcp still needs the token", func(t *testing.T) {
		res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
		if mcp.count() != 0 {
			t.Fatalf("the MCP handler ran without a token")
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		res, err := http.Get(srv.URL + "/nope")
		if err != nil {
			t.Fatalf("GET /nope: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
	})
}

// TestNew_Timeouts pins the server timeouts from contract C4.
func TestNew_Timeouts(t *testing.T) {
	srv := New(Options{Addr: "127.0.0.1:0"})

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 120*time.Second {
		t.Fatalf("WriteTimeout = %s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout = %s", srv.IdleTimeout)
	}
}

// TestAccessLog_NoAuthHeader: the log carries method, path, status, duration,
// remote and user, and never the credential.
func TestAccessLog_NoAuthHeader(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := newTestServer(t, Options{MCP: &okHandler{}, UserHeader: "X-Mcp-Gateway-User", Log: log})

	req := bearerRequest(t, http.MethodPost, srv.URL+"/mcp", testToken, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mcp-Gateway-User", "demo-user")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	entry := lastLogLine(t, &buf)
	if entry["msg"] != "http" {
		t.Fatalf("msg = %v, want http", entry["msg"])
	}
	if entry["method"] != "POST" || entry["path"] != "/mcp" {
		t.Fatalf("method/path = %v/%v", entry["method"], entry["path"])
	}
	if entry["status"] != float64(http.StatusOK) {
		t.Fatalf("status = %v, want 200", entry["status"])
	}
	if entry["user"] != "demo-user" {
		t.Fatalf("user = %v, want demo-user", entry["user"])
	}
	for _, key := range []string{"duration_ms", "remote"} {
		if _, ok := entry[key]; !ok {
			t.Fatalf("attribute %s is missing from %v", key, entry)
		}
	}
	if strings.Contains(buf.String(), testToken) {
		t.Fatalf("the access log leaked the bearer token")
	}
}

// TestAccessLog_DeniedIsWarn: a refusal is logged at warn level under msg=denied.
func TestAccessLog_DeniedIsWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := newTestServer(t, Options{MCP: &okHandler{}, UserHeader: "X-Mcp-Gateway-User", Log: log})

	res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer res.Body.Close()

	entry := lastLogLine(t, &buf)
	if entry["msg"] != "denied" {
		t.Fatalf("msg = %v, want denied", entry["msg"])
	}
	if entry["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", entry["level"])
	}
	if entry["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("status = %v, want 401", entry["status"])
	}
	if entry["user"] != "-" {
		t.Fatalf("user = %v, want - when the gateway sent none", entry["user"])
	}
}

func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no log line was written")
	}
	raw := lines[len(lines)-1]
	var entry map[string]any
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("log line %q is not JSON: %v", raw, err)
	}
	return entry
}
