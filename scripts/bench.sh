#!/usr/bin/env bash
# bench.sh — measure both MCP servers over the same Loki datasource.
#
# WHY TWO PHASES
#   A  fake Grafana only: S0 isolates the MCP server overhead (no upstream, no
#      real Loki), which is the only way to see the server's own cost.
#   B  the real Grafana Cloud stack: S1-S4, with the two targets interleaved so
#      caching and load drift hit both sides equally.
#
# The Go container is served by the image built from this repository; the Python
# container is the FastMCP reference implementation this project replaces.
#
# USAGE
#   GO_URL=http://127.0.0.1:18101 PY_URL=http://127.0.0.1:18102 \
#   GO_CONTAINER=loki-bench-go PY_CONTAINER=loki-bench-python \
#   GO_IMAGE=loki-via-grafana-mcp:bench PY_IMAGE=loki-cloud-logs:bench \
#   TOKEN_FILE=.secrets/mcp_token GRAFANA_URL=https://<stack>.grafana.net \
#   BENCH_DS_UID=<loki-ds> BENCH_SELECTOR='{app="<service>"}' BENCH_FILTER=error \
#   scripts/bench.sh
#
#   OUT_DIR defaults to bench-out/<UTC timestamp> and receives rows.jsonl,
#   meta.json and extra.json. That output MUST NOT be committed: it names a real
#   datasource. cmd/benchreport refuses to publish it when it does.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# ---------------------------------------------------------------- helpers

image_mb() {
  docker image inspect "$1" --format '{{.Size}}' 2>/dev/null | awk '{printf "%.1f", $1/1048576}'
}

rss_mb() {
  docker stats --no-stream --format '{{.MemUsage}}' "$1" 2>/dev/null |
    awk -F'/' '{gsub(/[A-Za-z ]/, "", $1); printf "%.1f", $1+0}'
}

cpu_model() {
  if [ -r /proc/cpuinfo ]; then
    awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo
  else
    sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown
  fi
}

vcpu() {
  getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0
}

ram_gb() {
  if [ -r /proc/meminfo ]; then
    awk '/MemTotal/{printf "%.0f", $2/1048576}' /proc/meminfo
  else
    echo 0
  fi
}

os_name() {
  # The release name only: never the hostname.
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    printf '%s' "${PRETTY_NAME:-${ID:-linux}}"
  else
    uname -s
  fi
}

# cold_start_ms restarts the container five times and reports the median of the
# time until /healthz answers 200, polling every 50ms.
cold_start_ms() {
  local container="$1" port="${HEALTH_PORT:-8000}" start sample samples=()

  for _ in 1 2 3 4 5; do
    start=$(date +%s%3N)
    docker restart "$container" >/dev/null 2>&1 || return 1
    for _ in $(seq 1 400); do
      if curl -fsS -o /dev/null "http://127.0.0.1:${port}/healthz"; then
        break
      fi
      sleep 0.05
    done
    samples+=("$(( $(date +%s%3N) - start ))")
  done

  printf '%s\n' "${samples[@]}" | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'
}

# sample_rss writes one RSS reading per second until it is killed.
sample_rss() {
  local container="$1" out="$2"
  while true; do
    rss_mb "$container" >>"$out" 2>/dev/null || true
    sleep 1
  done
}

: "${GO_URL:?GO_URL is required}"
: "${PY_URL:?PY_URL is required}"
: "${GO_CONTAINER:?GO_CONTAINER is required}"
: "${PY_CONTAINER:?PY_CONTAINER is required}"
: "${GO_IMAGE:?GO_IMAGE is required}"
: "${PY_IMAGE:?PY_IMAGE is required}"
: "${TOKEN_FILE:?TOKEN_FILE is required}"
: "${BENCH_DS_UID:?BENCH_DS_UID is required}"
: "${BENCH_SELECTOR:?BENCH_SELECTOR is required}"
command -v docker >/dev/null || { echo "bench: docker is required" >&2; exit 2; }

FAKE_NAME="${FAKE_NAME:-fake-grafana}"
FAKE_PORT="${FAKE_PORT:-9999}"
FAKE_URL="http://${FAKE_NAME}:${FAKE_PORT}"
# The published ports must match the GO_URL / PY_URL the caller passes in.
GO_PORT="${GO_PORT:-18101}"
PY_PORT="${PY_PORT:-18102}"
OUT_DIR="${OUT_DIR:-bench-out/$(date -u +%Y%m%dT%H%M%SZ)}"
# The bench containers are not the hardened deployment: they run as the calling
# user so the 0600 secret files stay readable, and they need no healthcheck.
mkdir -p "$OUT_DIR"
echo "== bench: output in $OUT_DIR"

# ------------------------------------------------------- phase A: S0 (fake)

echo "== phase A: S0 against the fake Grafana (server overhead only)"

go run ./cmd/bench --fake-grafana "0.0.0.0:${FAKE_PORT}" >"$OUT_DIR/fake-grafana.log" 2>&1 &
FAKE_PID=$!
trap 'kill "$FAKE_PID" >/dev/null 2>&1 || true' EXIT

start_container() { # container, grafana url, insecure flag, published port
  local container="$1" grafana_url="$2" insecure="$3" port="$4"
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker run -d --name "$container" \
    --user "$(id -u):$(id -g)" \
    -e MCP_TRANSPORT=http -e MCP_LISTEN=0.0.0.0:8000 \
    -e GRAFANA_URL="$grafana_url" \
    -e GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana_sa_token \
    -e MCP_TOKEN_FILE=/run/secrets/mcp_token \
    -v "$(pwd)/.secrets/grafana_sa_token:/run/secrets/grafana_sa_token:ro" \
    -v "$(pwd)/.secrets/mcp_token:/run/secrets/mcp_token:ro" \
    ${insecure:+-e GRAFANA_ALLOW_INSECURE=true} \
    --add-host "${FAKE_NAME}:host-gateway" \
    -p "127.0.0.1:${port}:8000" >/dev/null
}

start_container "$GO_CONTAINER" "$FAKE_URL" "insecure" "$GO_PORT"
start_container "$PY_CONTAINER" "$FAKE_URL" "insecure" "$PY_PORT"

go run ./cmd/bench \
  --target "go=$GO_URL" --target "python=$PY_URL" \
  --token-file "$TOKEN_FILE" \
  --scenarios S0 \
  --iterations "${BENCH_ITERATIONS:-30}" \
  --out "$OUT_DIR/rows-s0.jsonl"

# ------------------------------------------------- phase B: S1-S4 (real Loki)

echo "== phase B: S1-S4 against the real Grafana Cloud stack"
: "${GRAFANA_URL:?GRAFANA_URL is required for phase B}"

start_container "$GO_CONTAINER" "$GRAFANA_URL" "" "$GO_PORT"
start_container "$PY_CONTAINER" "$GRAFANA_URL" "" "$PY_PORT"

GO_RSS="$OUT_DIR/rss-go.txt"
PY_RSS="$OUT_DIR/rss-python.txt"
sample_rss "$GO_CONTAINER" "$GO_RSS" &
GO_SAMPLER=$!
sample_rss "$PY_CONTAINER" "$PY_RSS" &
PY_SAMPLER=$!

go run ./cmd/bench \
  --target "go=$GO_URL" --target "python=$PY_URL" \
  --token-file "$TOKEN_FILE" \
  --scenarios S1,S2,S3,S4 \
  --warmup "${BENCH_WARMUP:-3}" \
  --iterations "${BENCH_ITERATIONS:-30}" \
  --concurrency-s4 "${BENCH_CONCURRENCY_S4:-10}" \
  --rounds-s4 "${BENCH_ROUNDS_S4:-20}" \
  --max-error-rate "${BENCH_MAX_ERROR_RATE:-0.05}" \
  --out "$OUT_DIR/rows-s1-s4.jsonl"
BENCH_RC=$?

kill "$GO_SAMPLER" "$PY_SAMPLER" >/dev/null 2>&1 || true
if [ "$BENCH_RC" -ne 0 ]; then
  echo "bench: the measured run failed (exit $BENCH_RC); not rendering" >&2
  exit "$BENCH_RC"
fi

cat "$OUT_DIR"/rows-s0.jsonl "$OUT_DIR"/rows-s1-s4.jsonl >"$OUT_DIR/rows.jsonl"

peak_of() { sort -n "$1" 2>/dev/null | tail -1; }
idle_of() { awk '{sum+=$1; n++} END{if (n>0) printf "%.1f", sum/n; else print 0}' "$1"; }

cat >"$OUT_DIR/extra.json" <<JSON
{
  "image_size_mb": {
    "go": $(image_mb "$GO_IMAGE"),
    "python": $(image_mb "$PY_IMAGE")
  },
  "cold_start_ms": {
    "go": $(cold_start_ms "$GO_CONTAINER"),
    "python": $(cold_start_ms "$PY_CONTAINER")
  },
  "rss_mb": {
    "go": {"idle": $(idle_of "$GO_RSS"), "peak": $(peak_of "$GO_RSS")},
    "python": {"idle": $(idle_of "$PY_RSS"), "peak": $(peak_of "$PY_RSS")}
  }
}
JSON

cat >"$OUT_DIR/meta.json" <<JSON
{
  "cpu_model": "$(cpu_model)",
  "vcpu": $(vcpu),
  "ram_gb": $(ram_gb),
  "os": "$(os_name)",
  "docker_version": "$(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',')",
  "go_version": "$(go version | awk '{print $3}')",
  "gosdk_version": "$(grep -m1 'modelcontextprotocol/go-sdk' go.mod | awk '{print $2}')",
  "commit": "$(git rev-parse HEAD 2>/dev/null || echo unknown)",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON

echo "== bench: wrote $OUT_DIR/{rows.jsonl,meta.json,extra.json}"
echo "== bench: render with cmd/benchreport; never commit $OUT_DIR"