package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxResponseBytes caps how much of a Grafana response body is read.
const maxResponseBytes = 32 << 20 // 32 MiB

// maxErrorRunes caps the length of any error text taken from Grafana.
const maxErrorRunes = 500

// Datasource is a Grafana datasource as returned by /api/datasources.
type Datasource struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

// Client is a read-only Grafana client. It deliberately exposes only the two
// calls the server needs: there is no generic request method to abuse with a
// broadly-scoped service account token.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient builds a Client. A nil hc is replaced with a new http.Client using
// timeout. Redirects are never followed.
func NewClient(baseURL, token string, timeout time.Duration, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	} else {
		copied := *hc // do not change the caller's client
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: hc,
	}
}

// DSQuery runs a datasource query and returns the raw frames of results.A.
func (c *Client) DSQuery(ctx context.Context, payload map[string]any) ([]json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("Grafana request failed: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/ds/query", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req, "/api/ds/query")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Grafana /api/ds/query %d: %s", resp.StatusCode, c.errorText(raw))
	}

	var parsed struct {
		Results map[string]struct {
			Error  string            `json:"error"`
			Frames []json.RawMessage `json:"frames"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("Grafana /api/ds/query %d: %s", resp.StatusCode, c.errorText(raw))
	}

	result := parsed.Results["A"]
	if result.Error != "" {
		return nil, fmt.Errorf("Loki error: %s", c.scrub(result.Error))
	}
	if result.Frames == nil {
		return []json.RawMessage{}, nil
	}
	return result.Frames, nil
}

// ListDatasources returns the Loki datasources visible to the token, sorted by
// name.
func (c *Client) ListDatasources(ctx context.Context) ([]Datasource, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/datasources", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req, "/api/datasources")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Grafana /api/datasources %d: %s", resp.StatusCode, c.errorText(raw))
	}

	var entries []datasourceEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("Grafana /api/datasources %d: %s", resp.StatusCode, c.errorText(raw))
	}
	loki := make([]Datasource, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "loki" {
			continue
		}
		loki = append(loki, Datasource{UID: entry.UID, Name: entry.Name, IsDefault: entry.IsDefault})
	}
	sort.Slice(loki, func(i, j int) bool {
		if loki[i].Name != loki[j].Name {
			return loki[i].Name < loki[j].Name
		}
		return loki[i].UID < loki[j].UID
	})
	return loki, nil
}

// newRequest builds a request with the common headers already set.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("Grafana request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "loki-via-grafana-mcp/"+Version)
	return req, nil
}

// do executes the request, turning transport errors and redirects into errors.
func (c *Client) do(req *http.Request, path string) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := redirectHost(resp.Header.Get("Location"))
		resp.Body.Close()
		return nil, fmt.Errorf("Grafana %s %d: redirect to %s refused (check GRAFANA_URL / proxy auth)", path, resp.StatusCode, location)
	}
	return resp, nil
}

// readBody reads at most maxResponseBytes from body.
func readBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Grafana request failed: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("Grafana response exceeds 32 MiB")
	}
	return raw, nil
}

// errorText extracts a human readable error from a Grafana response body and
// scrubs the service account token from it.
func (c *Client) errorText(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	var parsed struct {
		Results map[string]struct {
			Error string `json:"error"`
		} `json:"results"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if result, ok := parsed.Results["A"]; ok && result.Error != "" {
			text = result.Error
		} else if parsed.Message != "" {
			text = parsed.Message
		}
	}
	return c.scrub(text)
}

// scrub removes the service account token from text and cuts it to
// maxErrorRunes, so no upstream text can carry the token into an error.
func (c *Client) scrub(text string) string {
	if c.token != "" {
		text = strings.ReplaceAll(text, c.token, "[redacted]")
	}
	return truncateRunes(text, maxErrorRunes)
}

// transportError describes a failed round trip without the request URL or the
// dialed address, which *url.Error and *net.OpError would otherwise expose to
// the MCP caller.
func transportError(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errors.New("Grafana request failed: cannot resolve the Grafana host")
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	for {
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			break
		}
		err = opErr.Err
	}
	return fmt.Errorf("Grafana request failed: %w", err)
}

// redirectHost reduces a Location header to its host, so a redirect to an SSO
// endpoint never echoes a full URL into an error message.
func redirectHost(location string) string {
	if location == "" {
		return "(no Location header)"
	}
	parsed, err := url.Parse(location)
	if err != nil || parsed.Host == "" {
		return "(unparsable Location header)"
	}
	return parsed.Host
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// datasourceEntry is the full /api/datasources entry, including the type that
// Datasource deliberately does not expose.
type datasourceEntry struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	IsDefault bool   `json:"isDefault"`
}
