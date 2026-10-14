// Package tools registers the two read-only MCP tools. Handlers return a Go
// error on failure, which go-sdk turns into an IsError result; a successful
// handler never returns a nil slice.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/grafana"
)

const (
	// defaultDSCacheTTL bounds how long the datasource list is reused.
	defaultDSCacheTTL = 5 * time.Minute
	// maxToolErrorRunes bounds a message shown to the caller.
	maxToolErrorRunes = 500
	// minLimit and maxLimit clamp the requested log line count.
	minLimit = 1
	maxLimit = 500
	// defaultLimit applies when the caller sends no limit.
	defaultLimit = 100
)

// Grafana is the slice of the client the tools use.
type Grafana interface {
	DSQuery(ctx context.Context, payload map[string]any) ([]json.RawMessage, error)
	ListDatasources(ctx context.Context) ([]grafana.Datasource, error)
}

// Deps is what the handlers need.
type Deps struct {
	Grafana    Grafana
	Log        *slog.Logger
	Now        func() time.Time // nil -> time.Now
	DSCacheTTL time.Duration    // 0 -> 5m

	// ds is installed by Register so both tools share one cache. A Deps built
	// by hand without Register gets a fresh, empty cache on every call.
	ds *dsCache
}

func (d Deps) logger() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// datasourceCache returns the shared cache, or a fresh one for a Deps that did
// not go through Register.
func (d Deps) datasourceCache() *dsCache {
	if d.ds == nil {
		return &dsCache{ttl: defaultDSCacheTTL, now: d.now}
	}
	return d.ds
}

// readOnly returns the annotations shared by both tools. DestructiveHint and
// OpenWorldHint are *bool in go-sdk, so they point at a false value.
func readOnly() *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &f,
		IdempotentHint:  true,
		OpenWorldHint:   &f,
	}
}

// dsCache memoizes the Loki datasource list so a burst of queries does not turn
// into a burst of /api/datasources calls. Failures are never cached, so a
// transient error does not lock the server out. The lock is never held across
// the network call: one caller fetches while the others wait on inflight and
// can still give up when their own context ends.
type dsCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	loaded   bool
	at       time.Time
	list     []grafana.Datasource
	inflight chan struct{} // non-nil while a fetch runs; closed when it ends
}

func (c *dsCache) datasources(ctx context.Context, g Grafana) ([]grafana.Datasource, error) {
	for {
		c.mu.Lock()
		if c.loaded && c.now().Sub(c.at) < c.ttl {
			list := c.list
			c.mu.Unlock()
			return list, nil
		}
		if wait := c.inflight; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				continue // the fetch ended; use its result or try again
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.inflight = done
		c.mu.Unlock()

		list, err := g.ListDatasources(ctx)

		c.mu.Lock()
		if err == nil {
			c.list, c.at, c.loaded = list, c.now(), true
		}
		c.inflight = nil
		close(done)
		c.mu.Unlock()
		return list, err
	}
}

// requireLoki refuses a uid outside the Loki datasource list. /api/ds/query
// resolves a datasource by uid and ignores the type in the payload, so a
// Prometheus or SQL uid would run the expression against that datasource.
func (d Deps) requireLoki(ctx context.Context, uid string) error {
	if d.Grafana == nil {
		return errors.New("grafana client is not configured")
	}
	list, err := d.datasourceCache().datasources(ctx, d.Grafana)
	if err != nil {
		return err
	}
	for _, ds := range list {
		if ds.UID == uid {
			return nil
		}
	}
	return fmt.Errorf("datasource_uid %q is not a Loki datasource visible to this service account", uid)
}

// callInfo is what a handler reports for the per-call log line.
type callInfo struct {
	resultType string // "logs", "series" or "datasources"
	count      int    // lines, series or datasources
	logql      string // debug level only; never log content
}

// instrument adapts h to an mcp tool handler and logs one line per call. A
// non-nil error becomes an IsError result; a panic becomes "internal error".
func instrument[In any, Out any](d Deps, tool string, h func(ctx context.Context, in In) (Out, callInfo, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (_ *mcp.CallToolResult, out Out, err error) {
		start := time.Now()
		var info callInfo
		defer func() {
			if r := recover(); r != nil {
				d.logger().Error("tool panic", "tool", tool)
				var zero Out
				out, err, info = zero, errors.New("internal error"), callInfo{}
			}
			attrs := []any{"tool", tool, "duration_ms", time.Since(start).Milliseconds(), "ok", err == nil}
			switch info.resultType {
			case "logs":
				attrs = append(attrs, "result_type", "logs", "lines", info.count)
			case "series":
				attrs = append(attrs, "result_type", "series", "series", info.count)
			case "datasources":
				attrs = append(attrs, "result_type", "datasources", "datasources", info.count)
			}
			if err != nil {
				attrs = append(attrs, "error", truncateRunes(err.Error(), maxToolErrorRunes))
			}
			if info.logql != "" && d.logger().Enabled(ctx, slog.LevelDebug) {
				attrs = append(attrs, "logql", info.logql)
			}
			d.logger().Info("tool call", attrs...)
		}()

		var herr error
		out, info, herr = h(ctx, in)
		if herr != nil {
			var zero Out
			return nil, zero, errors.New(truncateRunes(herr.Error(), maxToolErrorRunes))
		}
		return nil, out, nil
	}
}

// clampLimit applies the default to an absent limit and keeps the result in
// [minLimit, maxLimit].
func clampLimit(p *int) int {
	if p == nil {
		return defaultLimit
	}
	return min(max(*p, minLimit), maxLimit)
}

// orDefault trims value and falls back to def when it is empty.
func orDefault(value, def string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return def
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
