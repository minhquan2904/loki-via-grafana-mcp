// Package config loads and validates the server configuration from environment
// variables. It is deliberately dependency free: Load takes the environment as
// a []string so tests can drive it without touching the real process env.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Transport selects how the MCP server talks to its client.
type Transport string

const (
	// TransportStdio serves MCP over stdin/stdout.
	TransportStdio Transport = "stdio"
	// TransportHTTP serves MCP over streamable HTTP.
	TransportHTTP Transport = "http"
)

// Default values for optional environment variables.
const (
	defaultListenAddr    = "127.0.0.1:8000"
	defaultAllowedHosts  = "127.0.0.1:*,localhost:*,[::1]:*"
	defaultUserHeader    = "X-MCP-Gateway-User"
	defaultLogLevel      = "info"
	defaultTimeout       = 60 * time.Second
	defaultMaxConcurrent = 16

	minTimeout = time.Millisecond
	maxTimeout = 5 * time.Minute

	minTokenLength = 16
	minConcurrent  = 1
	maxConcurrent  = 256
)

// Config is the validated runtime configuration.
type Config struct {
	GrafanaURL     string // no trailing slash
	GrafanaToken   string // service account token; never printed
	Transport      Transport
	ListenAddr     string // http only
	AuthToken      string // http only; never printed
	AllowedHosts   []string
	AllowedOrigins []string // derived from AllowedHosts
	UserHeader     string   // canonical form, e.g. "X-Mcp-Gateway-User"
	LogLevel       slog.Level
	HTTPTimeout    time.Duration
	AllowInsecure  bool
	MaxConcurrent  int
}

// headerNameRE matches a valid HTTP header field name.
var headerNameRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Load reads and validates the configuration from environ (entries of the form
// "KEY=value"). The last occurrence of a key wins.
func Load(environ []string) (Config, error) {
	env := parseEnviron(environ)
	var c Config

	rawURL := strings.TrimSpace(env.get("GRAFANA_URL"))
	if rawURL == "" {
		return Config{}, fmt.Errorf("GRAFANA_URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Config{}, fmt.Errorf("GRAFANA_URL must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		// Credentials in the URL would end up in logs; the token goes in its own variable.
		return Config{}, fmt.Errorf("GRAFANA_URL must not contain credentials; use GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE")
	}
	c.GrafanaURL = strings.TrimRight(rawURL, "/")

	token, err := resolveSecret(env, "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE", "GRAFANA_SERVICE_ACCOUNT_TOKEN")
	if err != nil {
		return Config{}, err
	}
	if token == "" {
		return Config{}, fmt.Errorf("Grafana service account token is required: set GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE or GRAFANA_SERVICE_ACCOUNT_TOKEN")
	}
	c.GrafanaToken = token

	transport := strings.TrimSpace(env.get("MCP_TRANSPORT"))
	if transport == "" {
		transport = string(TransportStdio)
	}
	switch Transport(transport) {
	case TransportStdio:
		c.Transport = TransportStdio
	case TransportHTTP:
		c.Transport = TransportHTTP
	default:
		return Config{}, fmt.Errorf("MCP_TRANSPORT must be %q or %q, got %q", TransportStdio, TransportHTTP, transport)
	}

	listen := strings.TrimSpace(env.get("MCP_LISTEN"))
	if listen == "" {
		listen = defaultListenAddr
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return Config{}, fmt.Errorf("MCP_LISTEN must be host:port, got %q", listen)
	}
	c.ListenAddr = listen

	if c.Transport == TransportHTTP {
		authToken, err := resolveSecret(env, "MCP_TOKEN_FILE", "MCP_TOKEN")
		if err != nil {
			return Config{}, err
		}
		if authToken == "" {
			return Config{}, fmt.Errorf("MCP_TOKEN_FILE or MCP_TOKEN is required when MCP_TRANSPORT=http")
		}
		if len([]rune(authToken)) < minTokenLength {
			return Config{}, fmt.Errorf("MCP_TOKEN must be at least %d characters", minTokenLength)
		}
		c.AuthToken = authToken
	}

	hosts := splitCSV(env.get("MCP_ALLOWED_HOSTS"))
	if len(hosts) == 0 {
		hosts = splitCSV(defaultAllowedHosts)
	}
	c.AllowedHosts = hosts
	c.AllowedOrigins = deriveOrigins(hosts)

	userHeader := strings.TrimSpace(env.get("MCP_USER_HEADER"))
	if userHeader == "" {
		userHeader = defaultUserHeader
	}
	if !headerNameRE.MatchString(userHeader) {
		return Config{}, fmt.Errorf("MCP_USER_HEADER is not a valid header name: %q", userHeader)
	}
	c.UserHeader = http.CanonicalHeaderKey(userHeader)

	level := strings.TrimSpace(env.get("LOG_LEVEL"))
	if level == "" {
		level = defaultLogLevel
	}
	parsedLevel, err := parseLogLevel(level)
	if err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", level)
	}
	c.LogLevel = parsedLevel

	timeout := strings.TrimSpace(env.get("GRAFANA_TIMEOUT"))
	if timeout == "" {
		c.HTTPTimeout = defaultTimeout
	} else {
		d, err := time.ParseDuration(timeout)
		if err != nil || d < minTimeout || d > maxTimeout {
			return Config{}, fmt.Errorf("GRAFANA_TIMEOUT must be a duration between 1ms and 5m, got %q", timeout)
		}
		c.HTTPTimeout = d
	}

	insecure := strings.TrimSpace(env.get("GRAFANA_ALLOW_INSECURE"))
	switch {
	case insecure == "":
		c.AllowInsecure = false
	case strings.EqualFold(insecure, "true"):
		c.AllowInsecure = true
	case strings.EqualFold(insecure, "false"):
		c.AllowInsecure = false
	default:
		return Config{}, fmt.Errorf("GRAFANA_ALLOW_INSECURE must be \"true\" or \"false\", got %q", insecure)
	}
	if parsed.Scheme == "http" && !c.AllowInsecure && !isLoopbackHost(parsed.Hostname()) {
		return Config{}, fmt.Errorf("GRAFANA_URL uses http; set GRAFANA_ALLOW_INSECURE=true to send the token unencrypted")
	}

	concurrent := strings.TrimSpace(env.get("MCP_MAX_CONCURRENT"))
	if concurrent == "" {
		c.MaxConcurrent = defaultMaxConcurrent
	} else {
		n, err := strconv.Atoi(concurrent)
		if err != nil || n < minConcurrent || n > maxConcurrent {
			return Config{}, fmt.Errorf("MCP_MAX_CONCURRENT must be an integer between %d and %d, got %q", minConcurrent, maxConcurrent, concurrent)
		}
		c.MaxConcurrent = n
	}

	return c, nil
}

// String renders the configuration with secrets redacted.
func (c Config) String() string {
	return fmt.Sprintf(
		"transport=%s grafana_url=%s grafana_token=%s listen_addr=%s auth_token=%s allowed_hosts=%s allowed_origins=%s user_header=%s log_level=%s http_timeout=%s allow_insecure=%t max_concurrent=%d",
		c.Transport, c.GrafanaURL, redacted, c.ListenAddr, redacted,
		strings.Join(c.AllowedHosts, ","), strings.Join(c.AllowedOrigins, ","),
		c.UserHeader, c.LogLevel, c.HTTPTimeout, c.AllowInsecure, c.MaxConcurrent,
	)
}

// GoString keeps %#v from printing the tokens.
func (c Config) GoString() string { return "config.Config{" + c.String() + "}" }

// LogValue renders the configuration as a slog group with secrets redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("transport", string(c.Transport)),
		slog.String("grafana_url", c.GrafanaURL),
		slog.String("grafana_token", redacted),
		slog.String("listen_addr", c.ListenAddr),
		slog.String("auth_token", redacted),
		slog.Any("allowed_hosts", c.AllowedHosts),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.String("user_header", c.UserHeader),
		slog.String("log_level", c.LogLevel.String()),
		slog.Duration("http_timeout", c.HTTPTimeout),
		slog.Bool("allow_insecure", c.AllowInsecure),
		slog.Int("max_concurrent", c.MaxConcurrent),
	)
}

const redacted = "[redacted]"

// environ is a small lookup helper over a KEY=value list.
type environ map[string]string

func parseEnviron(entries []string) environ {
	env := make(environ, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env
}

func (e environ) get(key string) string { return e[key] }

// resolveSecret returns the secret from the *_FILE variable when set, otherwise
// from the plain variable. The file form wins. Errors never include file
// contents.
func resolveSecret(env environ, fileVar, valueVar string) (string, error) {
	if path := strings.TrimSpace(env.get(fileVar)); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s is missing or unreadable: %w", fileVar, err)
		}
		secret, err := normalizeSecretFile(string(data))
		if err != nil {
			return "", fmt.Errorf("%s must contain exactly one non-empty line", fileVar)
		}
		return secret, nil
	}
	return strings.TrimSpace(env.get(valueVar)), nil
}

// normalizeSecretFile strips exactly one trailing newline (and one trailing
// carriage return before it). Anything else that still looks like a line break
// is rejected.
func normalizeSecretFile(content string) (string, error) {
	s := content
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("secret must be exactly one non-empty line")
	}
	return s, nil
}

// splitCSV splits a comma separated list, trimming spaces and dropping empties.
func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// deriveOrigins expands host patterns into allowed Origin header values.
// Pattern entries ("host:*") have no fixed port, so they cannot produce an
// origin and are skipped.
func deriveOrigins(hosts []string) []string {
	origins := make([]string, 0, len(hosts)*2)
	for _, host := range hosts {
		if strings.Contains(host, ":*") {
			continue
		}
		// Browsers send the Origin host in lower case.
		host = strings.ToLower(host)
		origins = append(origins, "http://"+host, "https://"+host)
	}
	return origins
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level")
	}
}

// isLoopbackHost reports whether host is localhost, an IPv4 loopback address or
// IPv6 ::1.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
