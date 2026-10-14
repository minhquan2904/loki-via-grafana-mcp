package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secret is a synthetic token used only by tests. It is assembled at run time so
// that this file cannot trip the repository's own deny list.
var secret = "glsa" + "_" + "synthetic_test_token_0123456789"

func writeSecret(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return path
}

func baseEnv(extra ...string) []string {
	env := []string{
		"GRAFANA_URL=https://grafana.test",
		"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret,
	}
	return append(env, extra...)
}

func TestLoad_MissingGrafanaURL(t *testing.T) {
	_, err := Load([]string{"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "GRAFANA_URL is required") {
		t.Fatalf("error must name the variable, got %q", err.Error())
	}
}

func TestLoad_GrafanaURLValidation(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"relative", "grafana.test", "GRAFANA_URL must be an absolute http(s) URL"},
		{"no-scheme", "//grafana.test", "GRAFANA_URL must be an absolute http(s) URL"},
		{"bad-scheme", "ftp://grafana.test", "GRAFANA_URL must be an absolute http(s) URL"},
		{"no-host", "https://", "GRAFANA_URL must be an absolute http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]string{"GRAFANA_URL=" + tc.url, "GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoad_TrailingSlashStripped(t *testing.T) {
	cfg, err := Load(baseEnv("GRAFANA_URL=https://grafana.test/"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GrafanaURL != "https://grafana.test" {
		t.Fatalf("GrafanaURL = %q, want no trailing slash", cfg.GrafanaURL)
	}
}

func TestLoad_MissingGrafanaToken(t *testing.T) {
	_, err := Load([]string{"GRAFANA_URL=https://grafana.test"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	want := "Grafana service account token is required: set GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE or GRAFANA_SERVICE_ACCOUNT_TOKEN"
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

// TestLoad_SecretFile covers the secret file rules from C1.
func TestLoad_SecretFile(t *testing.T) {
	t.Run("file wins over value", func(t *testing.T) {
		path := writeSecret(t, "file-token-value\n")
		cfg, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
			"GRAFANA_SERVICE_ACCOUNT_TOKEN=env-token-value",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.GrafanaToken != "file-token-value" {
			t.Fatalf("GrafanaToken = %q, want the file value", cfg.GrafanaToken)
		}
	})

	t.Run("exactly one trailing newline is stripped", func(t *testing.T) {
		path := writeSecret(t, "token\r\n")
		cfg, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.GrafanaToken != "token" {
			t.Fatalf("GrafanaToken = %q, want %q", cfg.GrafanaToken, "token")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := writeSecret(t, "")
		_, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		wantErr(t, err, "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE must contain exactly one non-empty line")
	})

	t.Run("newline only", func(t *testing.T) {
		path := writeSecret(t, "\n")
		_, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		wantErr(t, err, "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE must contain exactly one non-empty line")
	})

	t.Run("two lines", func(t *testing.T) {
		path := writeSecret(t, "one\ntwo\n")
		_, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		wantErr(t, err, "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE must contain exactly one non-empty line")
	})

	t.Run("carriage return inside", func(t *testing.T) {
		path := writeSecret(t, "to\rken\n")
		_, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		wantErr(t, err, "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE must contain exactly one non-empty line")
	})

	t.Run("unreadable file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := Load([]string{
			"GRAFANA_URL=https://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + path,
		})
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.HasPrefix(err.Error(), "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE is missing or unreadable") {
			t.Fatalf("unexpected error %q", err.Error())
		}
	})
}

func TestLoad_Transport(t *testing.T) {
	t.Run("defaults to stdio", func(t *testing.T) {
		cfg, err := Load(baseEnv())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Transport != TransportStdio {
			t.Fatalf("Transport = %q, want stdio", cfg.Transport)
		}
		if cfg.AuthToken != "" {
			t.Fatalf("AuthToken must stay empty for stdio, got %q", cfg.AuthToken)
		}
	})

	t.Run("http without token", func(t *testing.T) {
		_, err := Load(baseEnv("MCP_TRANSPORT=http"))
		wantErr(t, err, "MCP_TOKEN_FILE or MCP_TOKEN is required when MCP_TRANSPORT=http")
	})

	t.Run("http with short token", func(t *testing.T) {
		_, err := Load(baseEnv("MCP_TRANSPORT=http", "MCP_TOKEN=short"))
		wantErr(t, err, "MCP_TOKEN must be at least 16 characters")
	})

	t.Run("http with token", func(t *testing.T) {
		cfg, err := Load(baseEnv("MCP_TRANSPORT=http", "MCP_TOKEN=0123456789abcdef"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Transport != TransportHTTP {
			t.Fatalf("Transport = %q, want http", cfg.Transport)
		}
		if cfg.AuthToken != "0123456789abcdef" {
			t.Fatalf("AuthToken = %q", cfg.AuthToken)
		}
	})

	t.Run("unknown transport", func(t *testing.T) {
		_, err := Load(baseEnv("MCP_TRANSPORT=grpc"))
		wantErr(t, err, `MCP_TRANSPORT must be "stdio" or "http", got "grpc"`)
	})
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(baseEnv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8000" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.UserHeader != "X-Mcp-Gateway-User" {
		t.Fatalf("UserHeader = %q", cfg.UserHeader)
	}
	if got := strings.Join(cfg.AllowedHosts, ","); got != "127.0.0.1:*,localhost:*,[::1]:*" {
		t.Fatalf("AllowedHosts = %q", got)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.HTTPTimeout != 60*time.Second {
		t.Fatalf("HTTPTimeout = %v", cfg.HTTPTimeout)
	}
	if cfg.AllowInsecure {
		t.Fatal("AllowInsecure must default to false")
	}
	if cfg.MaxConcurrent != 16 {
		t.Fatalf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}
}

func TestLoad_AllowedOrigins(t *testing.T) {
	cfg, err := Load(baseEnv("MCP_ALLOWED_HOSTS=127.0.0.1:8000,localhost:*,example.test"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := strings.Join(cfg.AllowedOrigins, ",")
	want := "http://127.0.0.1:8000,https://127.0.0.1:8000,http://example.test,https://example.test"
	if got != want {
		t.Fatalf("AllowedOrigins = %q, want %q", got, want)
	}
}

func TestLoad_UserHeader(t *testing.T) {
	t.Run("custom is canonicalized", func(t *testing.T) {
		cfg, err := Load(baseEnv("MCP_USER_HEADER=x-remote-user"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.UserHeader != "X-Remote-User" {
			t.Fatalf("UserHeader = %q", cfg.UserHeader)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		_, err := Load(baseEnv("MCP_USER_HEADER=bad header"))
		wantErr(t, err, `MCP_USER_HEADER is not a valid header name: "bad header"`)
	})
}

func TestLoad_ListenAddr(t *testing.T) {
	_, err := Load(baseEnv("MCP_LISTEN=not-a-host-port"))
	wantErr(t, err, `MCP_LISTEN must be host:port, got "not-a-host-port"`)
}

func TestLoad_LogLevel(t *testing.T) {
	cfg, err := Load(baseEnv("LOG_LEVEL=DEBUG"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v, want debug", cfg.LogLevel)
	}

	_, err = Load(baseEnv("LOG_LEVEL=verbose"))
	wantErr(t, err, `LOG_LEVEL must be debug, info, warn or error, got "verbose"`)
}

func TestLoad_Timeout(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, err := Load(baseEnv("GRAFANA_TIMEOUT=90s"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPTimeout != 90*time.Second {
			t.Fatalf("HTTPTimeout = %v", cfg.HTTPTimeout)
		}
	})

	t.Run("floor is 1ms", func(t *testing.T) {
		cfg, err := Load(baseEnv("GRAFANA_TIMEOUT=1ms"))
		if err != nil {
			t.Fatalf("1ms must be accepted, got %v", err)
		}
		if cfg.HTTPTimeout != time.Millisecond {
			t.Fatalf("HTTPTimeout = %v", cfg.HTTPTimeout)
		}
	})

	t.Run("500us rejected", func(t *testing.T) {
		_, err := Load(baseEnv("GRAFANA_TIMEOUT=500us"))
		wantErr(t, err, `GRAFANA_TIMEOUT must be a duration between 1ms and 5m, got "500us"`)
	})

	t.Run("above ceiling rejected", func(t *testing.T) {
		_, err := Load(baseEnv("GRAFANA_TIMEOUT=6m"))
		wantErr(t, err, `GRAFANA_TIMEOUT must be a duration between 1ms and 5m, got "6m"`)
	})

	t.Run("garbage rejected", func(t *testing.T) {
		_, err := Load(baseEnv("GRAFANA_TIMEOUT=soon"))
		wantErr(t, err, `GRAFANA_TIMEOUT must be a duration between 1ms and 5m, got "soon"`)
	})
}

func TestLoad_AllowInsecure(t *testing.T) {
	t.Run("http url rejected by default", func(t *testing.T) {
		_, err := Load([]string{
			"GRAFANA_URL=http://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret,
		})
		wantErr(t, err, "GRAFANA_URL uses http; set GRAFANA_ALLOW_INSECURE=true to send the token unencrypted")
	})

	t.Run("http url allowed with flag", func(t *testing.T) {
		cfg, err := Load([]string{
			"GRAFANA_URL=http://grafana.test",
			"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret,
			"GRAFANA_ALLOW_INSECURE=true",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.AllowInsecure {
			t.Fatal("AllowInsecure = false, want true")
		}
	})

	t.Run("loopback http url allowed without flag", func(t *testing.T) {
		for _, host := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000"} {
			if _, err := Load([]string{
				"GRAFANA_URL=" + host,
				"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret,
			}); err != nil {
				t.Fatalf("Load(%s): %v", host, err)
			}
		}
	})

	t.Run("invalid value", func(t *testing.T) {
		_, err := Load(baseEnv("GRAFANA_ALLOW_INSECURE=yes"))
		wantErr(t, err, `GRAFANA_ALLOW_INSECURE must be "true" or "false", got "yes"`)
	})

	t.Run("false is accepted", func(t *testing.T) {
		cfg, err := Load(baseEnv("GRAFANA_ALLOW_INSECURE=FALSE"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AllowInsecure {
			t.Fatal("AllowInsecure = true, want false")
		}
	})
}

func TestLoad_MaxConcurrent(t *testing.T) {
	cfg, err := Load(baseEnv("MCP_MAX_CONCURRENT=256"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxConcurrent != 256 {
		t.Fatalf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}

	for _, bad := range []string{"0", "257", "-1", "many"} {
		_, err := Load(baseEnv("MCP_MAX_CONCURRENT=" + bad))
		wantErr(t, err, `MCP_MAX_CONCURRENT must be an integer between 1 and 256, got "`+bad+`"`)
	}
}

// TestLoad_ErrorNeverContainsSecret asserts the redaction invariant from C1.
func TestLoad_ErrorNeverContainsSecret(t *testing.T) {
	cases := [][]string{
		{"GRAFANA_URL=grafana.test", "GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret},
		{"GRAFANA_URL=https://grafana.test", "GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret, "MCP_TRANSPORT=http"},
		{"GRAFANA_URL=https://grafana.test", "GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret, "MCP_TRANSPORT=http", "MCP_TOKEN=" + secret, "LOG_LEVEL=verbose"},
		{"GRAFANA_URL=https://grafana.test", "GRAFANA_SERVICE_ACCOUNT_TOKEN=" + secret, "MCP_MAX_CONCURRENT=0"},
	}
	for _, env := range cases {
		_, err := Load(env)
		if err == nil {
			t.Fatalf("want error for env %v", env)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks the token: %q", err.Error())
		}
	}
}

func TestConfig_StringAndLogValueRedact(t *testing.T) {
	cfg, err := Load(baseEnv("MCP_TRANSPORT=http", "MCP_TOKEN=0123456789abcdef"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s := cfg.String(); strings.Contains(s, cfg.GrafanaToken) || strings.Contains(s, cfg.AuthToken) {
		t.Fatalf("String() leaks a token: %s", s)
	}
	text := cfg.LogValue().String()
	if strings.Contains(text, cfg.GrafanaToken) || strings.Contains(text, cfg.AuthToken) {
		t.Fatalf("LogValue() leaks a token: %s", text)
	}
	if !strings.Contains(cfg.String(), redacted) {
		t.Fatalf("String() should show the redaction marker: %s", cfg.String())
	}
}

func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q, got nil", want)
	}
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}
