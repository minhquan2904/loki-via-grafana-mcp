// Package httpserver holds the HTTP hardening in front of the MCP handler: host
// and origin checks, bearer auth, body limit, concurrency limit, access log and
// the unauthenticated /healthz probe.
package httpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// limitWait is how long a request may queue for a concurrency slot.
const limitWait = 30 * time.Second

// HostCheck refuses a request whose Host header matches none of patterns, which
// is the defence against DNS rebinding. Pattern forms (case-insensitive):
//
//	host:port   exact host and port
//	host:*      that host on any port
//	host        a Host header without a port
//	[::1]:*     the same, for an IPv6 literal
func HostCheck(patterns []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(patterns, r.Host) {
			refuse(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OriginCheck answers 403 to a request whose Origin header is present but not in
// allowed (exact match). Requests without Origin (non-browser clients) pass;
// with an empty list every browser Origin is refused.
func OriginCheck(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origins, ok := r.Header["Origin"]; ok {
			if len(origins) != 1 || !slices.Contains(allowed, origins[0]) {
				refuse(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// BearerAuth requires a bearer credential in the Authorization header (the
// scheme is case-insensitive) and answers 401 otherwise.
func BearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearer(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			refuse(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BodyLimit rejects a request that declares a body larger than n bytes with 413
// and caps what the handler can read at n bytes. It never reads the body itself,
// so a slow client cannot hold it up; it must run after authentication.
func BodyLimit(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			refuse(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}

// Limit lets at most n requests run next at once; a request that cannot get a
// slot within 30s (or whose client went away) is answered 503.
func Limit(n int, next http.Handler) http.Handler { return limit(n, limitWait, next) }

// AccessLog writes one line per request once it has been answered: msg="http",
// or msg="denied" at warn level for 401/403/421. The Authorization header is
// never logged.
func AccessLog(log *slog.Logger, userHeader string, next http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", remoteHost(r),
			"user", userOf(r, userHeader),
		}
		switch rec.status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusMisdirectedRequest:
			log.Warn("denied", attrs...)
		default:
			log.Info("http", attrs...)
		}
	})
}

func validBearer(r *http.Request, token string) bool {
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func limit(n int, wait time.Duration, next http.Handler) http.Handler {
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	sem := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
		default:
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case sem <- struct{}{}:
			case <-timer.C:
				busy(w)
				return
			case <-r.Context().Done():
				busy(w)
				return
			}
		}
		defer func() { <-sem }()
		next.ServeHTTP(w, r)
	})
}

// busy answers a request that could not get a slot. Retry-After keeps a client
// from hammering the server while it drains.
func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	refuse(w, "server busy", http.StatusServiceUnavailable)
}

// refuse answers a request that was turned away before its body was read.
// "Connection: close" matters here: without it net/http first tries to drain the
// unread body (up to 256KB), so a client that trickles a chunked body would hold
// the answer back until the read timeout.
func refuse(w http.ResponseWriter, msg string, code int) {
	body, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		body = []byte(`{"error":"refused"}`)
	}
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

// hostAllowed reports whether a Host header matches one of the patterns.
func hostAllowed(patterns []string, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	name, port, hasPort := splitHost(host)
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		wantName, wantPort, wantHasPort := splitHost(pattern)
		if wantName != name {
			continue
		}
		switch {
		case wantPort == "*":
			return hasPort
		case wantHasPort:
			return hasPort && wantPort == port
		default:
			return !hasPort
		}
	}
	return false
}

// splitHost splits "host", "host:port" or "[::1]:port" into a name, a port and
// whether a port was present.
func splitHost(value string) (name, port string, hasPort bool) {
	if strings.HasPrefix(value, "[") {
		if host, port, err := net.SplitHostPort(value); err == nil {
			return host, port, true
		}
		return strings.Trim(value, "[]"), "", false
	}
	if idx := strings.LastIndex(value, ":"); idx >= 0 && !strings.Contains(value[idx+1:], ":") {
		return value[:idx], value[idx+1:], true
	}
	return value, "", false
}

// remoteHost is the client address for the log line: the first X-Forwarded-For
// entry when a proxy set one, otherwise the peer address.
func remoteHost(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, ok := strings.Cut(forwarded, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// userOf reads the caller identity a gateway forwards; "-" when absent.
func userOf(r *http.Request, header string) string {
	if header == "" {
		return "-"
	}
	if user := strings.TrimSpace(r.Header.Get(header)); user != "" {
		return user
	}
	return "-"
}

// statusRecorder remembers the status code for the access log and keeps the
// optional interfaces the MCP handler may rely on.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
