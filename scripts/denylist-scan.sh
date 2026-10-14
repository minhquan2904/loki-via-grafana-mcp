#!/usr/bin/env bash
# denylist-scan.sh — refuse a repository that carries internal identifiers.
#
# Thin wrapper around cmd/denyscan, which does the matching in Go with the same
# RE2 patterns as internal/redact (POSIX grep does not understand \d or \S, so a
# grep-based scanner silently misses addresses and bearer tokens). It scans the
# working tree, including untracked files that are not ignored, and every added
# line, author, committer and message in the history of every ref.
#
# USAGE
#   scripts/denylist-scan.sh [--require-local] [--self-test]
#
#   --require-local  fail (exit 2) when .denylist.local is missing; use it
#                    before a publish, where the local patterns must be in play
#   --self-test      seed canaries into a throwaway repository and require the
#                    scanner to catch every one, then exit
#
# The output names a place and a pattern, never the matching text. The local
# patterns are printed when they match, so do not run this in a public CI log.
#
# EXIT  0 clean, 1 something matched, 2 a precondition failed.
set -euo pipefail

cd "$(dirname "$0")/.."

args=()
for arg in "$@"; do
  case "$arg" in
    --require-local) args+=(-require-local) ;;
    --self-test) args+=(-self-test) ;;
    *)
      echo "denylist-scan: unknown flag $arg" >&2
      exit 2
      ;;
  esac
done

exec go run ./cmd/denyscan "${args[@]}"
