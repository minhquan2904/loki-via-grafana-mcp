# loki-via-grafana-mcp

A read-only [MCP](https://modelcontextprotocol.io) server that queries Loki
**through Grafana's datasource query API** (`POST /api/ds/query`) with a Grafana
service account token.

That path matters when Loki is not reachable directly and when the datasource
proxy (`/api/datasources/proxy/...`) is blocked by a gateway — which is the
situation on Grafana Cloud behind an authenticating proxy. If Loki is exposed to
you directly, [`grafana/loki-mcp`](https://github.com/grafana/loki-mcp) is a
better fit; if you want the whole Grafana API, use
[`grafana/mcp-grafana`](https://github.com/grafana/mcp-grafana). This server
deliberately does less: two read-only tools, nothing else.

## Read-only by design

The boundary is structural, not a flag:

- `internal/grafana.Client` exposes exactly two methods — `DSQuery` and
  `ListDatasources`. There is no generic `Do`/`Request` method to point at
  another endpoint with a broadly scoped token. A test asserts the method set.
- `POST /api/ds/query` resolves a datasource **by uid and ignores the type in the
  payload**, so a Prometheus or SQL uid would run the expression against that
  datasource. The server therefore checks the uid against the Loki datasource
  list (cached for 5 minutes) before it sends anything.
- Redirects are refused: a redirect to an SSO host would carry the token along,
  or turn the POST into a GET.
- `GRAFANA_URL` with an `http` scheme is rejected unless the host is loopback or
  `GRAFANA_ALLOW_INSECURE=true`, so the token cannot travel in clear text by
  accident.

## Required Grafana permissions

A service account with the **Viewer** role is enough:

| Permission | Used for |
|---|---|
| `datasources:read` | `list_loki_datasources` |
| `datasources:query` | `query_logs` |

Scope the token to the Loki datasources you intend to expose. The server never
writes to Grafana.

## Configuration

| Env | Required | Default | Notes |
|---|---|---|---|
| `GRAFANA_URL` | yes | — | Absolute `http(s)` URL, no trailing slash, no `user:password@` |
| `GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE` | one of the two | — | Preferred; a file with exactly one non-empty line |
| `GRAFANA_SERVICE_ACCOUNT_TOKEN` | one of the two | — | Never logged, never echoed in errors |
| `MCP_TRANSPORT` | no | `stdio` | `stdio` or `http` |
| `MCP_LISTEN` | no | `127.0.0.1:8000` | `host:port` |
| `MCP_TOKEN_FILE` / `MCP_TOKEN` | required for `http` | — | Bearer token for `/mcp`, at least 16 characters |
| `MCP_ALLOWED_HOSTS` | no | `127.0.0.1:*,localhost:*,[::1]:*` | CSV of `host`, `host:port` or `host:*` patterns |
| `MCP_USER_HEADER` | no | `X-MCP-Gateway-User` | Header carrying the caller identity for the access log |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `GRAFANA_TIMEOUT` | no | `60s` | Per Grafana call, between 1ms and 5m |
| `GRAFANA_ALLOW_INSECURE` | no | `false` | Required for a plain `http` Grafana that is not on loopback |
| `MCP_MAX_CONCURRENT` | no | `16` | Concurrent `/mcp` requests; a request that waits more than 30s gets 503 |

Secrets are read from the file form first. A missing or unreadable file, or a
file that holds anything other than a single line, is a startup error that names
the variable and never prints the value.

## Quick start

```sh
cp .env.example .env
mkdir -p .secrets
printf '%s\n' "$GRAFANA_SA_TOKEN" > .secrets/grafana_sa_token
printf '%s\n' "$MCP_TOKEN" > .secrets/mcp_token
docker compose up --build
curl -fsS http://127.0.0.1:8000/healthz   # ok
```

Register it with an MCP client (the client sends the token in a header):

```sh
claude mcp add --transport http loki http://127.0.0.1:8000/mcp \
  --header "Authorization: Bearer $TOKEN"
```

With `MCP_TRANSPORT=stdio` the binary speaks MCP on stdin/stdout and needs no
listener, which is the simplest way to wire it into a desktop client:

```sh
MCP_TRANSPORT=stdio GRAFANA_URL=https://your-stack.grafana.net \
  GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=.secrets/grafana_sa_token ./server
```

## Tools

### `query_logs`

| Argument | Type | Default | Meaning |
|---|---|---|---|
| `datasource_uid` | string | — | Loki datasource UID; find it with `list_loki_datasources` |
| `logql` | string | — | LogQL, e.g. `{app="demo"} \|= "error"` or `sum(count_over_time({app="demo"}[5m]))` |
| `start` | string | `now-1h` | `now`, `now-<n><s\|m\|h\|d\|w>`, epoch ms or RFC3339 |
| `end` | string | `now` | Same formats |
| `limit` | number | `100` | Log lines, clamped to 1-500 |
| `direction` | string | `backward` | `backward` (newest first) or `forward` |
| `step` | string | auto | Metric resolution, e.g. `30s`, `5m` or seconds; the default aims at ~300 points |

Log queries return `{"type":"logs","logs":[{time,line,labels,truncated?}]}`.
Metric queries return `{"type":"series","series":[{labels,values}]}`. Log lines
longer than 4000 characters are cut and marked `truncated`. Lines are data, not
instructions: the server never acts on their content.

Without `step`, the resolution is `max(1s, ceil(window/300))` rounded up to whole
seconds, and both `step` and `intervalMs` travel with the query. A window that
would need more than 11000 points per series is rejected before the call instead
of failing late upstream.

### `list_loki_datasources`

Takes no arguments and returns `{"datasources":[{uid,name,isDefault}]}`, limited
to datasources of type `loki` that the service account can see.

## Benchmarks

<!-- BENCH:START -->
| Scenario | Go p50 / p95 (ms) | Python p50 / p95 (ms) | Go errors | Python errors |
|---|---|---|---|---|
| S0 | 2.558 / 3.12 | 4.267 / 6.467 | 0 | 0 |
| S1 | 42.196 / 51.691 | 41.361 / 44.81 | 0 | 0 |
| S2 | 35.947 / 46.56 | 36.839 / 40.172 | 0 | 0 |
| S3 | 32.107 / 42.83 | 51.055 / 70.658 | 0 | 0 |

S0 runs against a fake Grafana and measures server overhead only. S1, S2, S3 ran against a self-hosted Grafana + Loki stack. Not run: S4.

| Metric | Go | Python |
|---|---|---|
| Image size, compressed (MB) | 4.1 | 59.5 |
| Cold start to /healthz (ms) | 236 | 1017 |
| RSS idle (MB) | 8.1 | 49.5 |
| RSS peak under S4 (MB) | not measured | not measured |
| S3 points per series | 301 | 3601 |
| S3 response size (KB) | 9.5 | 239.7 |

Measured on 4 vCPU Intel(R) Core(TM) Ultra 7 265KF, 8 GB RAM, against a self-hosted Grafana + Loki stack, commit 009961a, 2026-10-14T07:11:11Z. Python = the FastMCP reference implementation this project replaces. Full report: docs/benchmarks.md
<!-- BENCH:END -->

The numbers are produced by `cmd/bench`, which drives both this server and the
FastMCP reference implementation through their MCP interfaces, so the figures
include MCP overhead. `scripts/bench.sh` runs S0 against a fake Grafana (server
cost only, with a dummy token), then S1-S3 and then S4 on its own against the
Grafana named by `GRAFANA_URL`; the report says which upstream that was
(`BENCH_UPSTREAM`) and which scenarios did not run. These figures are a
preliminary run: S4 and a run against Grafana Cloud are still to come.

Raw output is gitignored and never published: `cmd/benchreport` refuses to
render a value that matches a deny pattern or carries an address. Before a
release, `scripts/denylist-scan.sh` (a wrapper around `go run ./cmd/denyscan`)
scans the working tree, every commit's added lines and every author, committer
and message. Extra patterns come from a local, gitignored `.denylist.local`
(`--require-local` fails when it is missing); `--self-test` proves the scanner
catches seeded canaries. A history hit that is a known false positive is
acknowledged in `.denyscan-allow` as `<commit prefix> <path>`.

## Tests

```sh
scripts/dev.sh test     # go test -race -count=1 ./...
scripts/dev.sh lint     # gofmt -l . && go vet ./...
scripts/dev.sh build    # go build ./...
```

The canonical gate is:

```sh
gofmt -l . | grep -q . && exit 1; go vet ./...; go test -race -count=1 ./...
```

## Credits

The HTTP middleware shape (host check, origin check, constant-time bearer auth,
body limit, access log), the config loader, the redaction helper and the
benchmark harness follow the patterns of
[`minhquan2904/mysql-mcp-go`](https://github.com/minhquan2904/mysql-mcp-go).

## License

Apache-2.0 (see `LICENSE`).
