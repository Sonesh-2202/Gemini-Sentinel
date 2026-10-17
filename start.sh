#!/usr/bin/env bash
# start.sh — Launch Gemini-Sentinel on port 20133 with isolated
# account storage in ~/.gemini-sentinel/accounts/*.json.

set -euo pipefail
cd "$(dirname "$0")"

DIR="${SENTINEL_HOME:-$HOME/.gemini-sentinel}"
ACCOUNTS_DIR="$DIR/accounts"
mkdir -p "$ACCOUNTS_DIR"
export SENTINEL_HOME="$DIR"
export SENTINEL_PORT="${SENTINEL_PORT:-${PORT:-20133}}"

pairs=""
for f in "$ACCOUNTS_DIR"/*.json; do
  [ -e "$f" ] || continue
  id="$(basename "$f" .json)"
  [ -n "$pairs" ] && pairs="$pairs,"
  pairs="${pairs}sentinel-${id}:${id},${id}:${id}"
done

if [ -n "$pairs" ]; then
  export SENTINEL_ACCOUNT_TOKENS="$pairs"
fi

if [ ! -x "./sentinel" ] || [ -n "$(find cmd internal -name '*.go' -newer ./sentinel -print -quit 2>/dev/null)" ]; then
  echo "building sentinel..." >&2
  go build -o sentinel ./cmd/sentinel
fi

echo "starting sentinel on :$SENTINEL_PORT (data: $DIR)"
exec ./sentinel "$@"
