package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// maxBodyBytes caps what a client may send to /mcp.
	maxBodyBytes = 1 << 20
	// defaultMaxConcurrent applies when Options.MaxConcurrent is unset.
	defaultMaxConcurrent = 16
)

// Options configures New.
type Options struct {
	Addr           string
	MCP            http.Handler // served at /mcp behind the middleware chain
	Token          string       // bearer token for /mcp
	AllowedHosts   []string     // Host patterns, see HostCheck
	AllowedOrigins []string     // exact Origin values; empty refuses every browser origin
	UserHeader     string       // header carrying the caller identity for the access log
	Log            *slog.Logger
	MaxConcurrent  int // concurrent /mcp requests; 0 means 16
}

// New assembles the HTTP server:
//
//	/mcp     AccessLog -> HostCheck -> OriginCheck -> BearerAuth -> BodyLimit(1MB) -> Limit -> MCP
//
// BodyLimit sits behind auth on purpose: a client without the token must not be
// able to make the server read, or wait for, its body.
//
//	/healthz GET plain "ok", no middleware and no token: a probe must work
//	         while the Grafana side is unreachable.
func New(o Options) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/mcp", AccessLog(o.Log, o.UserHeader,
		HostCheck(o.AllowedHosts,
			OriginCheck(o.AllowedOrigins,
				BearerAuth(o.Token,
					BodyLimit(maxBodyBytes,
						Limit(o.MaxConcurrent, o.MCP)))))))
	mux.HandleFunc("/healthz", healthz)

	return &http.Server{
		Addr:    o.Addr,
		Handler: mux,
		// Responses are plain JSON (no SSE), so a request must arrive in full
		// within ReadTimeout and a query fits in WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// healthz is deliberately indifferent: it reports that the process is up, not
// whether Grafana answers.
func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}
