#!/usr/bin/env bash
# start.sh — Launch gemini-stealth-bridge (gsb) on port 20133 with isolated
# account storage in ~/.gemini-stealth-bridge/accounts/*.json.

set -euo pipefail
cd "$(dirname "$0")"

DIR="${BRIDGE_HOME:-$HOME/.gemini-stealth-bridge}"
ACCOUNTS_DIR="$DIR/accounts"
mkdir -p "$ACCOUNTS_DIR"
export BRIDGE_HOME="$DIR"
export PORT="${PORT:-20133}"

pairs=""
for f in "$ACCOUNTS_DIR"/*.json; do
  [ -e "$f" ] || continue
  id="$(basename "$f" .json)"
  [ -n "$pairs" ] && pairs="$pairs,"
  pairs="${pairs}gsb-${id}:${id},${id}:${id}"
done

if [ -n "$pairs" ]; then
  export BRIDGE_ACCOUNT_TOKENS="$pairs"
fi

if [ ! -x "./gsb" ] || [ -n "$(find cmd internal -name '*.go' -newer ./gsb -print -quit 2>/dev/null)" ]; then
  echo "building gsb..." >&2
  go build -o gsb ./cmd/gsb
fi

echo "starting gemini-stealth-bridge (gsb) on :$PORT (data: $DIR)"
exec ./gsb "$@"
