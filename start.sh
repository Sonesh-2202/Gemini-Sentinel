#!/usr/bin/env bash
# start.sh — Launch hearth on port 20133 with isolated
# account storage in ~/.hearth/accounts/*.json.
#
# Upgrading from gemini-stealth-bridge? Just run this script: the hearth
# binary copies your existing accounts from ~/.gemini-stealth-bridge on
# first start (source left untouched) and still accepts gsb-<id> tokens.

set -euo pipefail
cd "$(dirname "$0")"

DIR="${HEARTH_HOME:-${BRIDGE_HOME:-$HOME/.hearth}}"
ACCOUNTS_DIR="$DIR/accounts"
mkdir -p "$ACCOUNTS_DIR"
export HEARTH_HOME="$DIR"
export HEARTH_PORT="${HEARTH_PORT:-${BRIDGE_PORT:-${PORT:-20133}}}"

pairs=""
for f in "$ACCOUNTS_DIR"/*.json; do
  [ -e "$f" ] || continue
  id="$(basename "$f" .json)"
  [ -n "$pairs" ] && pairs="$pairs,"
  pairs="${pairs}hearth-${id}:${id},${id}:${id}"
done

if [ -n "$pairs" ]; then
  export HEARTH_ACCOUNT_TOKENS="$pairs"
fi

if [ ! -x "./hearth" ] || [ -n "$(find cmd internal -name '*.go' -newer ./hearth -print -quit 2>/dev/null)" ]; then
  echo "building hearth..." >&2
  go build -o hearth ./cmd/hearth
fi

echo "starting hearth on :$HEARTH_PORT (data: $DIR)"
exec ./hearth "$@"
