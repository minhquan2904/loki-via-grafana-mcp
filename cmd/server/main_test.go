package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testGrafanaToken = "sa-token-abc123"
	testMCPToken     = "mcp-token-abc123"
)

// logFrameJSON is hand-written synthetic data: no response from a real Grafana
// is ever copied into this repository.
const logFrameJSON = `{"schema":{"fields":[{"name":"labels","type":"other"},{"name":"Time","type":"time"},` +
	`{"name":"Line","type":"string"}]},"data":{"values":[[{"app":"demo"}],[1759800000000],` +
	`["level=error msg=\"boom\""]]}}`

// bearerTransport adds the token the server expects to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// fakeGrafana serves the two endpoints the server uses.
func fakeGrafana(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/datasources":
			_, _ = w.Write([]byte(`[{"uid":"loki-demo","name":"demo","type":"loki","isDefault":true}]`))
		case "/api/ds/query":
			_, _ = w.Write([]byte(`{"results":{"A":{"frames":[` + logFrameJSON + `]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseEnv(grafanaURL string) []string {
	return []string{
		"GRAFANA_URL=" + grafanaURL,
		"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + testGrafanaToken,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func waitForHealth(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never became ready on %s", url)
}

func textOf(res *mcp.CallToolResult) string {
	var text strings.Builder
	for _, content := range res.Content {
		if part, ok := content.(*mcp.TextContent); ok {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func TestRun_VersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"--version"}, nil, strings.NewReader(""), &stdout, &stderr)

	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got, want := stdout.String(), "loki-via-grafana-mcp "+version+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// TestRun_ConfigErrorExit2NoSecret: a missing variable is named, and no token
// value is ever printed.
func TestRun_ConfigErrorExit2NoSecret(t *testing.T) {
	env := []string{
		"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + testGrafanaToken,
		"MCP_TRANSPORT=http",
		"MCP_TOKEN=" + testMCPToken,
	}
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), nil, env, strings.NewReader(""), &stdout, &stderr)

	if code != exitConfig {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "GRAFANA_URL is required") {
		t.Fatalf("stderr does not name the variable: %q", stderr.String())
	}
	for _, secret := range []string{testGrafanaToken, testMCPToken} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("stderr leaked a secret: %q", stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}
}

// TestHealthcheckFlag covers the probe the distroless image uses: no Grafana
// configuration is involved.
func TestHealthcheckFlag(t *testing.T) {
	newHealthy := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("healthy", func(t *testing.T) {
		srv := newHealthy(t)
		addr := strings.TrimPrefix(srv.URL, "http://")
		var stdout, stderr bytes.Buffer

		code := run(context.Background(), []string{"--healthcheck"}, []string{"MCP_LISTEN=" + addr},
			strings.NewReader(""), &stdout, &stderr)

		if code != exitOK {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
		}
	})

	t.Run("wildcard listen address", func(t *testing.T) {
		srv := newHealthy(t)
		_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		var stdout, stderr bytes.Buffer

		code := run(context.Background(), []string{"--healthcheck"}, []string{"MCP_LISTEN=0.0.0.0:" + port},
			strings.NewReader(""), &stdout, &stderr)

		if code != exitOK {
			t.Fatalf("exit = %d, want 0 over the loopback interface (stderr %q)", code, stderr.String())
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		code := run(context.Background(), []string{"--healthcheck"},
			[]string{"MCP_LISTEN=127.0.0.1:" + strconv.Itoa(freePort(t))}, strings.NewReader(""), &stdout, &stderr)

		if code != exitRuntime {
			t.Fatalf("exit = %d, want 1", code)
		}
		if stderr.Len() == 0 {
			t.Fatalf("a failed probe must say why")
		}
	})

	t.Run("bad address", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		code := run(context.Background(), []string{"--healthcheck"}, []string{"MCP_LISTEN=not-a-host-port"},
			strings.NewReader(""), &stdout, &stderr)

		if code != exitRuntime {
			t.Fatalf("exit = %d, want 1", code)
		}
	})
}

// TestRun_HTTPEndToEnd drives the whole server: config, tool registration,
// middleware chain, MCP tool call and graceful shutdown.
func TestRun_HTTPEndToEnd(t *testing.T) {
	grafanaSrv := fakeGrafana(t)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	env := append(baseEnv(grafanaSrv.URL),
		"MCP_TRANSPORT=http",
		"MCP_TOKEN="+testMCPToken,
		"MCP_LISTEN=127.0.0.1:"+strconv.Itoa(port),
		"MCP_ALLOWED_HOSTS=127.0.0.1:*",
		"LOG_LEVEL=warn",
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, nil, env, strings.NewReader(""), io.Discard, io.Discard)
	}()

	waitForHealth(t, port)

	t.Run("tools list and call", func(t *testing.T) {
		client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             base + "/mcp",
			HTTPClient:           &http.Client{Transport: bearerTransport{token: testMCPToken}},
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer session.Close()

		list, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if len(list.Tools) != 2 {
			t.Fatalf("exposed %d tools, want 2", len(list.Tools))
		}

		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "query_logs",
			Arguments: map[string]any{"datasource_uid": "loki-demo", "logql": `{app="demo"}`},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("tool error: %s", textOf(res))
		}
		if got := textOf(res); !strings.Contains(got, `level=error msg=\"boom\"`) {
			t.Fatalf("result = %s", got)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		res, err := http.Post(base+"/mcp", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
	})

	t.Run("unexpected host", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Host = "evil.example.test"
		req.Header.Set("Authorization", "Bearer "+testMCPToken)
		req.Header.Set("Content-Type", "application/json")

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("status = %d, want 421", res.StatusCode)
		}
	})

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit = %d after cancellation, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("run did not return after the context was cancelled")
	}
}

func TestRun_StdioCleanStop(t *testing.T) {
	grafanaSrv := fakeGrafana(t)
	env := baseEnv(grafanaSrv.URL)

	t.Run("client closes stdin", func(t *testing.T) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		var stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- run(context.Background(), nil, env, inR, outW, &stderr)
			_ = outW.Close()
		}()

		init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
		if _, err := io.WriteString(inW, init); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(outR).ReadString('\n')
		if err != nil || !strings.Contains(line, `"id":1`) {
			t.Fatalf("initialize response = %q, %v", line, err)
		}
		_ = inW.Close() // the client goes away
		go func() { _, _ = io.Copy(io.Discard, outR) }()

		select {
		case code := <-done:
			if code != exitOK {
				t.Fatalf("exit = %d after stdin closed, want 0 (stderr %s)", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("stdio server did not stop after stdin closed")
		}
	})

	t.Run("signal while running", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- run(ctx, nil, env, pr, io.Discard, io.Discard) }()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Fatalf("exit = %d after a signal, want 0", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("stdio server did not stop")
		}
	})
}

func TestRun_NoGrafanaCallAtStartup(t *testing.T) {
	var hits atomic.Int32
	grafanaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(grafanaSrv.Close)
	port := freePort(t)
	env := append(baseEnv(grafanaSrv.URL),
		"MCP_TRANSPORT=http", "MCP_TOKEN="+testMCPToken, "MCP_LISTEN=127.0.0.1:"+strconv.Itoa(port))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, nil, env, strings.NewReader(""), io.Discard, io.Discard) }()
	waitForHealth(t, port)
	cancel()
	<-done
	if n := hits.Load(); n != 0 {
		t.Fatalf("Grafana was called %d times before the first tool call", n)
	}
}

func TestRun_BindFailureNotReady(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	env := append(baseEnv("https://grafana.test"),
		"MCP_TRANSPORT=http", "MCP_TOKEN="+testMCPToken, "MCP_LISTEN="+busy.Addr().String())

	var stderr bytes.Buffer
	code := run(context.Background(), nil, env, strings.NewReader(""), io.Discard, &stderr)
	if code != exitRuntime {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), `"msg":"ready"`) || !strings.Contains(stderr.String(), "cannot listen") {
		t.Fatalf("stderr = %s, want a bind error and no ready line", stderr.String())
	}
}

// proxyTransport makes requests look like they came through a reverse proxy:
// another Host header and a browser Origin for that host.
type proxyTransport struct {
	host, origin string
}

func (t proxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Host = t.host
	clone.Header.Set("Origin", t.origin)
	return http.DefaultTransport.RoundTrip(clone)
}

// TestRun_BehindProxyHost proves the SDK adds no host or origin check of its own
// on top of HostCheck/OriginCheck: a server bound to loopback must still serve a
// proxy that forwards an allowed, non-localhost Host and Origin.
func TestRun_BehindProxyHost(t *testing.T) {
	grafanaSrv := fakeGrafana(t)
	port := freePort(t)

	env := append(baseEnv(grafanaSrv.URL),
		"MCP_TRANSPORT=http",
		"MCP_TOKEN="+testMCPToken,
		"MCP_LISTEN=127.0.0.1:"+strconv.Itoa(port),
		"MCP_ALLOWED_HOSTS=127.0.0.1:*,mcp.test",
		"LOG_LEVEL=warn",
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, nil, env, strings.NewReader(""), io.Discard, io.Discard)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitForHealth(t, port)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		HTTPClient: &http.Client{Transport: bearerTransport{
			token: testMCPToken,
			base:  proxyTransport{host: "mcp.test", origin: "https://mcp.test"},
		}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect through the proxied host: %v", err)
	}
	defer session.Close()
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools through the proxied host: %v", err)
	}
}
