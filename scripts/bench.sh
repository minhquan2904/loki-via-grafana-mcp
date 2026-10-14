#!/usr/bin/env bash
# bench.sh — measure this server and the FastMCP reference implementation over
# the same Loki datasource, then leave rows.jsonl, meta.json and extra.json for
# cmd/benchreport.
#
# WHY TWO PHASES
#   A  a fake Grafana: S0 isolates the MCP server overhead (no network, no Loki).
#      The servers get a dummy service-account token here, never the real one.
#   B  the real Grafana named by GRAFANA_URL: S1-S3 with the two targets
#      interleaved so caching and load drift hit both sides equally, then S4
#      (concurrent) in a run of its own, the only window the RSS peak covers.
#
# Only docker is required on the host: the benchmark client and the fake
# Grafana run in golang containers on a private docker network, so nothing but
# the two servers' 127.0.0.1 ports is published.
#
# USAGE
#   PY_IMAGE=<reference image> \
#   GRAFANA_URL=https://<stack>.grafana.net \
#   BENCH_UPSTREAM='Grafana Cloud' \
#   BENCH_DS_UID=<loki-ds> BENCH_SELECTOR='{app="<service>"}' BENCH_FILTER=error \
#   scripts/bench.sh
#
#   Secrets: .secrets/grafana_sa_token (a Viewer service account is enough) and
#   .secrets/mcp_token (16+ characters), one line each.
#   BENCH_UPSTREAM is printed in the report as given; keep it neutral.
#   BENCH_SKIP_S4=1 skips the concurrent scenario (the report then says so).
#
#   OUT_DIR defaults to bench-out/<UTC timestamp>. That output MUST NOT be
#   committed: it names a real datasource. cmd/benchreport refuses to publish a
#   value that matches a deny pattern.
set -euo pipefail

cd "$(dirname "$0")/.."

: "${PY_IMAGE:?PY_IMAGE (the reference implementation image) is required}"
: "${GRAFANA_URL:?GRAFANA_URL is required for phase B}"
: "${BENCH_UPSTREAM:?BENCH_UPSTREAM is required: how the report names the Grafana of phase B}"
: "${BENCH_DS_UID:?BENCH_DS_UID is required}"
: "${BENCH_SELECTOR:?BENCH_SELECTOR is required}"
export BENCH_DS_UID BENCH_SELECTOR
export BENCH_FILTER="${BENCH_FILTER:-error}"
GO_IMAGE="${GO_IMAGE:-loki-via-grafana-mcp:bench}"
GOLANG_IMAGE="${GOLANG_IMAGE:-golang:1.25}"
GO_PORT="${GO_PORT:-18101}"
PY_PORT="${PY_PORT:-18102}"
NET="${BENCH_NET:-loki-bench}"
GO_CONTAINER="${GO_CONTAINER:-loki-bench-go}"
PY_CONTAINER="${PY_CONTAINER:-loki-bench-python}"
FAKE_CONTAINER="${FAKE_CONTAINER:-loki-bench-fake-grafana}"
OUT_DIR="${OUT_DIR:-bench-out/$(date -u +%Y%m%dT%H%M%SZ)}"
SECRETS="$(pwd)/.secrets"

command -v docker >/dev/null || { echo "bench: docker is required" >&2; exit 2; }
for f in grafana_sa_token mcp_token; do
  [ -s "$SECRETS/$f" ] || { echo "bench: $SECRETS/$f is missing or empty" >&2; exit 2; }
done
mkdir -p "$OUT_DIR"
OUT_ABS="$(cd "$OUT_DIR" && pwd)"
echo "== bench: output in $OUT_DIR"

# ---------------------------------------------------------------- helpers

# now_ms works with the BSD date of macOS too, which has no %N.
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

image_mb() { # the size docker reports, i.e. the compressed layers
  docker image inspect "$1" --format '{{.Size}}' | awk '{printf "%.1f", $1/1048576}'
}

rss_mb() {
  docker stats --no-stream --format '{{.MemUsage}}' "$1" 2>/dev/null |
    awk -F'/' '{v=$1; gsub(/ /, "", v); n=v+0;
      if (v ~ /GiB$/) n*=1024; else if (v ~ /KiB$/) n/=1024; else if (v !~ /MiB$/) n/=1048576;
      printf "%.1f\n", n}'
}

cpu_model() {
  if [ -r /proc/cpuinfo ]; then awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo
  else sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown; fi
}
vcpu() { getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0; }
ram_gb() {
  if [ -r /proc/meminfo ]; then awk '/MemTotal/{printf "%.0f", $2/1048576}' /proc/meminfo; else echo 0; fi
}
os_name() { # the release name only, never the hostname
  if [ -r /etc/os-release ]; then (. /etc/os-release && printf '%s' "${PRETTY_NAME:-${ID:-linux}}"); else uname -s; fi
}

# go_in_docker runs a Go command of this repository on the private network.
go_in_docker() {
  docker run --rm --network "$NET" \
    -v "$(pwd)":/src -v loki-bench-gomod:/go/pkg/mod -v "$OUT_ABS":/out \
    -v "$SECRETS/mcp_token":/run/secrets/mcp_token:ro \
    --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache \
    -w /src -e GOFLAGS=-buildvcs=false \
    -e BENCH_DS_UID -e BENCH_SELECTOR -e BENCH_FILTER \
    "$GOLANG_IMAGE" go "$@"
}

wait_healthy() { # port, tries of 50ms
  local i
  for ((i = 0; i < $2; i++)); do
    curl -fsS -o /dev/null "http://127.0.0.1:$1/healthz" 2>/dev/null && return 0
    sleep 0.05
  done
  echo "bench: nothing answers /healthz on 127.0.0.1:$1" >&2
  return 1
}

# start_server runs one server on the network, published on 127.0.0.1:port.
# It runs as the calling user so the 0600 secret files stay readable.
start_server() { # container, image, port, grafana url, SA token file
  local container="$1" image="$2" port="$3" url="$4" sa="$5" insecure=""
  case "$url" in http://*) insecure=1 ;; esac
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker run -d --name "$container" --network "$NET" \
    --user "$(id -u):$(id -g)" \
    -p "127.0.0.1:$port:8000" \
    -e MCP_TRANSPORT=http -e MCP_LISTEN=0.0.0.0:8000 \
    -e FASTMCP_HOST=0.0.0.0 -e FASTMCP_PORT=8000 \
    -e "MCP_ALLOWED_HOSTS=$container:8000,127.0.0.1:*,localhost:*" \
    -e "GRAFANA_URL=$url" ${insecure:+-e GRAFANA_ALLOW_INSECURE=true} \
    -e GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana_sa_token \
    -e MCP_TOKEN_FILE=/run/secrets/mcp_token -e LOGS_MCP_TOKEN_FILE=/run/secrets/mcp_token \
    -v "$sa":/run/secrets/grafana_sa_token:ro \
    -v "$SECRETS/mcp_token":/run/secrets/mcp_token:ro \
    "$image" >/dev/null
  wait_healthy "$port" 400
}

# cold_start_ms restarts a container five times and prints the median time
# until its own published /healthz answers 200.
cold_start_ms() { # container, port
  local start samples=()
  for _ in 1 2 3 4 5; do
    start=$(now_ms)
    docker restart "$1" >/dev/null
    wait_healthy "$2" 400
    samples+=("$(($(now_ms) - start))")
  done
  printf '%s\n' "${samples[@]}" | sort -n | sed -n 3p
}

idle_mb() { # mean of 10 samples, 1s apart, with no load
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do rss_mb "$1"; sleep 1; done |
    awk '{s+=$1; n++} END{if (n>0) printf "%.1f", s/n; else print "null"}'
}

sample_rss() { while true; do rss_mb "$1" >>"$2" || true; sleep 1; done; }
peak_of() { # a JSON number, or null when nothing was sampled
  local p
  p=$(sort -n "$1" 2>/dev/null | tail -1)
  printf '%s' "${p:-null}"
}

bench() { # scenarios, out file, extra flags...
  local scenarios="$1" out="$2"
  shift 2
  go_in_docker run ./cmd/bench \
    --target "go=http://$GO_CONTAINER:8000" --target "python=http://$PY_CONTAINER:8000" \
    --token-file /run/secrets/mcp_token --scenarios "$scenarios" \
    --warmup "${BENCH_WARMUP:-3}" --iterations "${BENCH_ITERATIONS:-30}" \
    --max-error-rate "${BENCH_MAX_ERROR_RATE:-0.05}" --out "/out/$out" "$@"
}

SAMPLERS=""
DUMMY_SA="$(mktemp)"
cleanup() {
  [ -z "$SAMPLERS" ] || kill $SAMPLERS >/dev/null 2>&1 || true
  docker rm -f "$GO_CONTAINER" "$PY_CONTAINER" "$FAKE_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -f "$DUMMY_SA"
}
trap cleanup EXIT

docker network create "$NET" >/dev/null
docker build -q --build-arg "VERSION=$(git rev-parse --short=12 HEAD)" -t "$GO_IMAGE" . >/dev/null

# ------------------------------------------------------- phase A: S0 (fake)

echo "== phase A: S0 against the fake Grafana (server overhead only)"
echo "dummy-token-not-a-secret" >"$DUMMY_SA"
# The fake listens on the private network only; it is never published.
docker run -d --name "$FAKE_CONTAINER" --network "$NET" \
  -v "$(pwd)":/src -v loki-bench-gomod:/go/pkg/mod -w /src -e GOFLAGS=-buildvcs=false \
  "$GOLANG_IMAGE" go run ./cmd/bench --fake-grafana 0.0.0.0:9999 >/dev/null
for _ in $(seq 1 600); do
  docker exec "$FAKE_CONTAINER" curl -s -o /dev/null http://127.0.0.1:9999/ 2>/dev/null && break
  sleep 0.2
done
start_server "$GO_CONTAINER" "$GO_IMAGE" "$GO_PORT" "http://$FAKE_CONTAINER:9999" "$DUMMY_SA"
start_server "$PY_CONTAINER" "$PY_IMAGE" "$PY_PORT" "http://$FAKE_CONTAINER:9999" "$DUMMY_SA"
bench S0 rows-s0.jsonl
docker rm -f "$FAKE_CONTAINER" >/dev/null

# ------------------------------------------------- phase B: S1-S4 (real Grafana)

echo "== phase B: S1-S3 against $BENCH_UPSTREAM"
start_server "$GO_CONTAINER" "$GO_IMAGE" "$GO_PORT" "$GRAFANA_URL" "$SECRETS/grafana_sa_token"
start_server "$PY_CONTAINER" "$PY_IMAGE" "$PY_PORT" "$GRAFANA_URL" "$SECRETS/grafana_sa_token"
sleep 5
GO_IDLE=$(idle_mb "$GO_CONTAINER")
PY_IDLE=$(idle_mb "$PY_CONTAINER")
bench S1,S2,S3 rows-s1-s3.jsonl

GO_PEAK=null
PY_PEAK=null
: >"$OUT_DIR/rows-s4.jsonl"
if [ "${BENCH_SKIP_S4:-0}" != 1 ]; then
  echo "== phase B: S4 (concurrent); RSS is sampled during S4 only"
  : >"$OUT_DIR/rss-go.txt"
  : >"$OUT_DIR/rss-python.txt"
  sample_rss "$GO_CONTAINER" "$OUT_DIR/rss-go.txt" &
  SAMPLERS="$!"
  sample_rss "$PY_CONTAINER" "$OUT_DIR/rss-python.txt" &
  SAMPLERS="$SAMPLERS $!"
  bench S4 rows-s4.jsonl \
    --concurrency-s4 "${BENCH_CONCURRENCY_S4:-10}" --rounds-s4 "${BENCH_ROUNDS_S4:-20}"
  kill $SAMPLERS >/dev/null 2>&1 || true
  SAMPLERS=""
  GO_PEAK=$(peak_of "$OUT_DIR/rss-go.txt")
  PY_PEAK=$(peak_of "$OUT_DIR/rss-python.txt")
fi

GO_COLD=$(cold_start_ms "$GO_CONTAINER" "$GO_PORT")
PY_COLD=$(cold_start_ms "$PY_CONTAINER" "$PY_PORT")

cat "$OUT_DIR"/rows-s0.jsonl "$OUT_DIR"/rows-s1-s3.jsonl "$OUT_DIR"/rows-s4.jsonl >"$OUT_DIR/rows.jsonl"

cat >"$OUT_DIR/extra.json" <<JSON
{
  "image_size_mb": {"go": $(image_mb "$GO_IMAGE"), "python": $(image_mb "$PY_IMAGE")},
  "cold_start_ms": {"go": $GO_COLD, "python": $PY_COLD},
  "rss_mb": {
    "go": {"idle": $GO_IDLE, "peak": $GO_PEAK},
    "python": {"idle": $PY_IDLE, "peak": $PY_PEAK}
  }
}
JSON

cat >"$OUT_DIR/meta.json" <<JSON
{
  "cpu_model": "$(cpu_model)",
  "vcpu": $(vcpu),
  "ram_gb": $(ram_gb),
  "os": "$(os_name)",
  "docker_version": "$(docker version --format '{{.Server.Version}}')",
  "go_version": "$(docker run --rm "$GOLANG_IMAGE" go env GOVERSION)",
  "gosdk_version": "$(awk '/modelcontextprotocol\/go-sdk/{print $2; exit}' go.mod)",
  "commit": "$(git rev-parse HEAD)",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "upstream": "$BENCH_UPSTREAM"
}
JSON

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "bench: WARNING: uncommitted changes; meta.commit does not describe them" >&2
fi
echo "== bench: wrote $OUT_DIR/{rows.jsonl,meta.json,extra.json}; never commit them"
echo "== render: go run ./cmd/benchreport --rows $OUT_DIR/rows.jsonl --meta $OUT_DIR/meta.json \\"
echo "     --extra $OUT_DIR/extra.json --readme README.md --doc docs/benchmarks.md --redact-file .denylist.local"
