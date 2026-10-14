#!/usr/bin/env bash
# denylist-scan.sh — refuse a repository that carries internal identifiers.
#
# WHY
#   The repository is public, so every file AND every commit is a publication.
#   The patterns come from one source: `go run ./cmd/benchreport --print-denylist`
#   (the built-ins of internal/redact) plus this machine's gitignored
#   .denylist.local file, which holds the names that must never be written down
#   in the repository itself.
#
# USAGE
#   scripts/denylist-scan.sh [--require-local] [--self-test]
#
#   --require-local  fail (exit 2) when .denylist.local is missing; used before a
#                    publish, where the local patterns must be in play
#   --self-test      prove the scanner reacts to a canary outside git, then exit
#
# EXIT  0 clean, 1 something matched, 2 a precondition failed.
set -uo pipefail

ALLOWLIST=("internal/redact/redact.go" "scripts/denylist-scan.sh")
REQUIRE_LOCAL=0
SELF_TEST=0

for arg in "$@"; do
  case "$arg" in
    --require-local) REQUIRE_LOCAL=1 ;;
    --self-test) SELF_TEST=1 ;;
    *)
      echo "denylist-scan: unknown flag $arg" >&2
      exit 2
      ;;
  esac
done

cd "$(dirname "$0")/.." || exit 2

if [ ! -f .denylist.local ]; then
  if [ "$REQUIRE_LOCAL" -eq 1 ]; then
    echo "denylist-scan: .denylist.local is required for this run" >&2
    exit 2
  fi
  echo "denylist-scan: no .denylist.local; scanning with the built-in patterns only" >&2
fi

PATTERNS="$(mktemp)"
CANARY=""
cleanup() {
  rm -f "$PATTERNS"
  [ -n "$CANARY" ] && rm -f "$CANARY"
}
trap cleanup EXIT

if ! go run ./cmd/benchreport --print-denylist >"$PATTERNS"; then
  echo "denylist-scan: cannot read the built-in patterns" >&2
  exit 2
fi
if [ -f .denylist.local ]; then
  grep -v '^[[:space:]]*#' .denylist.local | grep -v '^[[:space:]]*$' >>"$PATTERNS"
fi

if [ "$SELF_TEST" -eq 1 ]; then
  CANARY="$(mktemp)"
  # Assembled at run time so this file itself stays clean of the pattern.
  printf 'glsa_%s\n' "canary0123456789" >"$CANARY"

  found=0
  while IFS= read -r pattern; do
    [ -z "$pattern" ] && continue
    if grep -qIiE -e "$pattern" "$CANARY"; then
      found=1
      break
    fi
  done <"$PATTERNS"

  if [ "$found" -ne 1 ]; then
    echo "self-test: the canary was NOT detected" >&2
    exit 1
  fi

  if [ -f .denylist.local ]; then
    local_pattern="$(grep -v '^[[:space:]]*#' .denylist.local | grep -v '^[[:space:]]*$' | head -1)"
    if [ -n "$local_pattern" ]; then
      printf '%s\n' "$local_pattern" >"$CANARY"
      if ! grep -qIiE -e "$local_pattern" "$CANARY"; then
        echo "self-test: the local canary was NOT detected" >&2
        exit 1
      fi
    fi
  fi

  echo "self-test: canaries detected"
  exit 0
fi

# allowed_path reports whether a path may legitimately contain deny patterns.
allowed_path() {
  local candidate="$1"
  local allowed
  for allowed in "${ALLOWLIST[@]}"; do
    [ "$candidate" = "$allowed" ] && return 0
  done
  return 1
}

hits=0

while IFS= read -r pattern; do
  [ -z "$pattern" ] && continue

  # Working tree: only the paths outside the allowlist are scanned.
  mapfile -t paths < <(git ls-files)
  scanned=()
  for path in "${paths[@]}"; do
    allowed_path "$path" || scanned+=("$path")
  done

  if [ "${#scanned[@]}" -gt 0 ]; then
    while IFS= read -r hit; do
      [ -z "$hit" ] && continue
      echo "${hit%%:*}: pattern ${pattern}"
      hits=1
    done < <(git grep -nIiE -e "$pattern" -- "${scanned[@]}" 2>/dev/null || true)
  fi
done <"$PATTERNS"

# History: the allowlist applies here too, otherwise every commit that touched
# redact.go would fail the scan forever.
if git rev-parse --git-dir >/dev/null 2>&1 && [ -n "$(git rev-list --all --max-count=1)" ]; then
  while IFS= read -r pattern; do
    [ -z "$pattern" ] && continue
    while IFS= read -r hit; do
      [ -z "$hit" ] && continue
      ref="${hit%%:*}"
      rest="${hit#*:}"
      path="${rest%%:*}"
      line="${rest#*:}"
      allowed_path "$path" && continue
      echo "${ref}:${path}:${line%%:*}: pattern ${pattern}"
      hits=1
    done < <(git grep -nIiE -e "$pattern" $(git rev-list --all) 2>/dev/null | head -50 || true)
  done <"$PATTERNS"
fi

if [ "$hits" -ne 0 ]; then
  echo "denylist-scan: deny patterns matched; refusing" >&2
  exit 1
fi

echo "denylist-scan: clean"
exit 0
