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

	t.Run("same host listed twice", func(t *testing.T) {
		multi := []struct {
			patterns []string
			host     string
		}{
			{[]string{"example.test:8000", "example.test:8443"}, "example.test:8443"},
			{[]string{"localhost:*", "localhost"}, "localhost"},
			{[]string{"localhost", "localhost:*"}, "localhost:8000"},
		}
		for _, tc := range multi {
			if !hostAllowed(tc.patterns, tc.host) {
				t.Errorf("hostAllowed(%v, %q) = false, want true", tc.patterns, tc.host)
			}
		}
	})

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

	for _, bad := range []string{testToken + "x", testToken[:len(testToken)-1], strings.ToUpper(testToken)} {
		rec := httptest.NewRecorder()
		BearerAuth(testToken, next).ServeHTTP(rec, bearerRequest(t, http.MethodPost, "/mcp", bad, nil))
		assertRefusal(t, rec, http.StatusUnauthorized, "unauthorized")
	}
	if next.count() != 1 {
		t.Fatalf("the handler ran for a wrong token")
	}
}

func TestBearerAuth_EmptyConfiguredTokenRefusesAll(t *testing.T) {
	next := &okHandler{}
	for _, header := range []string{"Bearer ", "Bearer", "", "bearer  "} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		BearerAuth("", next).ServeHTTP(rec, req)
		assertRefusal(t, rec, http.StatusUnauthorized, "unauthorized")
	}
	if next.count() != 0 {
		t.Fatalf("an empty configured token let a request through")
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

	t.Run("chunked too large", func(t *testing.T) {
		next := &okHandler{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(strings.NewReader(strings.Repeat("x", 17))))
		req.ContentLength = -1

		BodyLimit(16, next).ServeHTTP(rec, req)

		assertRefusal(t, rec, http.StatusRequestEntityTooLarge, "request body too large")
		if next.count() != 0 {
			t.Fatalf("the handler ran on an oversized chunked body")
		}
	})

	t.Run("chunked within the limit is replayed intact", func(t *testing.T) {
		var got string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(strings.NewReader(strings.Repeat("y", 16))))
		req.ContentLength = -1

		BodyLimit(16, next).ServeHTTP(httptest.NewRecorder(), req)

		if got != strings.Repeat("y", 16) {
			t.Fatalf("handler read %q", got)
		}
	})

	t.Run("chunked over the wire through New", func(t *testing.T) {
		srv := newTestServer(t, Options{})
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(bytes.Repeat([]byte("z"), maxBodyBytes+1))
			_ = pw.Close()
		}()
		req := bearerRequest(t, http.MethodPost, srv.URL+"/mcp", testToken, pr) // pipe -> chunked
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), `"request body too large"`) {
			t.Fatalf("status %d body %q, want our JSON 413", res.StatusCode, body)
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

	t.Run("healthz is GET only", func(t *testing.T) {
		res, err := http.Post(srv.URL+"/healthz", "text/plain", nil)
		if err != nil {
			t.Fatalf("POST /healthz: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", res.StatusCode)
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

// TestNew_ChainOrder pins which check answers when several would fail, so a
// reordering of the middleware chain breaks a test.
func TestNew_ChainOrder(t *testing.T) {
	srv := newTestServer(t, Options{AllowedOrigins: []string{"http://localhost:8000"}})
	big := strings.Repeat("x", maxBodyBytes+1)

	cases := []struct {
		name   string
		host   string
		origin string
		token  string
		body   string
		status int
	}{
		{"bad host beats bad token", "evil.example.test", "", "wrong-token-0000000", "{}", http.StatusMisdirectedRequest},
		{"bad origin beats bad token", "", "https://evil.example.test", "wrong-token-0000000", "{}", http.StatusForbidden},
		{"no token beats a large body", "", "", "", big, http.StatusUnauthorized},
		{"large body with token", "", "", testToken, big, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := bearerRequest(t, http.MethodPost, srv.URL+"/mcp", tc.token, strings.NewReader(tc.body))
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.status)
			}
		})
	}
}

// TestNew_MaxConcurrent checks that Options.MaxConcurrent reaches the limiter.
func TestNew_MaxConcurrent(t *testing.T) {
	old := limitWait
	limitWait = 20 * time.Millisecond
	t.Cleanup(func() { limitWait = old })

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blocking := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
	})
	srv := newTestServer(t, Options{MCP: blocking, MaxConcurrent: 1})

	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, srv.URL+"/mcp", testToken, strings.NewReader("{}")))
		if err == nil {
			res.Body.Close()
		}
	}()
	<-entered

	res, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, srv.URL+"/mcp", testToken, strings.NewReader("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	close(release)
	<-done
}

func TestAccessLog_CustomUserHeader(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedBuffer{b: &buf, mu: &mu}, nil))
	srv := newTestServer(t, Options{Log: log, UserHeader: "X-Remote-User"})

	req := bearerRequest(t, http.MethodPost, srv.URL+"/mcp", testToken, strings.NewReader("{}"))
	req.Header.Set("X-Remote-User", "alice")
	req.Header.Set("X-Mcp-Gateway-User", "mallory")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), `"user":"alice"`) || strings.Contains(buf.String(), "mallory") {
		t.Fatalf("access log = %s, want the user from X-Remote-User only", buf.String())
	}
}

type lockedBuffer struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
