#!/usr/bin/env bash
#
# test-local.sh — Reliable dual-PDS local test harness for ATChess
#
# Boots two PDS instances, creates one account on each, and optionally
# runs E2E tests. Tears down cleanly on exit (including Ctrl-C).
#
# Usage:
#   ./scripts/test-local.sh              # Start harness, run e2e tests, tear down
#   ./scripts/test-local.sh --up         # Start harness only (leave running)
#   ./scripts/test-local.sh --down       # Tear down only
#   ./scripts/test-local.sh --no-test    # Start harness, skip tests, tear down
#
# Environment variables:
#   PDS_PORT_ALICE  Port for Alice's PDS (default: 2583)
#   PDS_PORT_BOB    Port for Bob's PDS   (default: 2584)
#   KEEP_UP         Set to 1 to skip teardown after tests

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$PROJECT_ROOT/docker-compose.dual-pds.yml"

PORT_ALICE="${PDS_PORT_ALICE:-2583}"
PORT_BOB="${PDS_PORT_BOB:-2584}"

ALICE_HANDLE="alice.test"
ALICE_PASS="alice-test-pass"
ALICE_EMAIL="alice@chess.test"

BOB_HANDLE="bob.test"
BOB_PASS="bob-test-pass"
BOB_EMAIL="bob@chess.test"

# --- helpers ----------------------------------------------------------------

log()  { printf '[test-local] %s\n' "$*"; }
die()  { log "FATAL: $*" >&2; exit 1; }

compose() {
  docker compose -f "$COMPOSE_FILE" "$@"
}

wait_for_healthy() {
  local name=$1 url=$2 max_wait=${3:-60}
  local elapsed=0
  log "Waiting for $name at $url (up to ${max_wait}s)..."
  while ! curl -sf "$url/xrpc/_health" >/dev/null 2>&1; do
    sleep 2
    elapsed=$((elapsed + 2))
    if [ "$elapsed" -ge "$max_wait" ]; then
      die "$name did not become healthy within ${max_wait}s"
    fi
  done
  log "$name is healthy (${elapsed}s)"
}

create_account() {
  local pds_url=$1 email=$2 handle=$3 password=$4 label=$5

  local response http_code body
  response=$(curl -s -w '\n%{http_code}' -X POST \
    "$pds_url/xrpc/com.atproto.server.createAccount" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$email\",\"handle\":\"$handle\",\"password\":\"$password\"}")

  http_code=$(echo "$response" | tail -1)
  body=$(echo "$response" | sed '$d')

  case "$http_code" in
    200)
      local did
      did=$(echo "$body" | grep -o '"did":"[^"]*"' | head -1 | cut -d'"' -f4)
      log "Created $label ($handle) -> $did"
      echo "$did"
      ;;
    400)
      if echo "$body" | grep -q "Handle already taken"; then
        # Account exists — fetch DID via session
        local sess
        sess=$(curl -sf -X POST "$pds_url/xrpc/com.atproto.server.createSession" \
          -H "Content-Type: application/json" \
          -d "{\"identifier\":\"$handle\",\"password\":\"$password\"}")
        local did
        did=$(echo "$sess" | grep -o '"did":"[^"]*"' | head -1 | cut -d'"' -f4)
        log "$label already exists ($handle) -> $did"
        echo "$did"
      else
        die "Failed to create $label (HTTP $http_code): $body"
      fi
      ;;
    *)
      die "Failed to create $label (HTTP $http_code): $body"
      ;;
  esac
}

do_up() {
  log "Starting dual-PDS containers..."
  compose up -d --wait --wait-timeout 60

  # Belt-and-suspenders: also poll from the host side
  wait_for_healthy "pds-alice" "http://localhost:$PORT_ALICE"
  wait_for_healthy "pds-bob"   "http://localhost:$PORT_BOB"

  log "Creating test accounts..."
  ALICE_DID=$(create_account "http://localhost:$PORT_ALICE" \
    "$ALICE_EMAIL" "$ALICE_HANDLE" "$ALICE_PASS" "Alice")
  BOB_DID=$(create_account "http://localhost:$PORT_BOB" \
    "$BOB_EMAIL" "$BOB_HANDLE" "$BOB_PASS" "Bob")

  log "Harness ready."
  log "  Alice: $ALICE_HANDLE ($ALICE_DID) @ http://localhost:$PORT_ALICE"
  log "  Bob:   $BOB_HANDLE ($BOB_DID) @ http://localhost:$PORT_BOB"
}

do_down() {
  log "Tearing down..."
  compose down -v --remove-orphans 2>/dev/null || true
  log "Done."
}

do_test() {
  log "Running E2E tests..."
  (
    cd "$PROJECT_ROOT"
    export ATCHESS_TEST_PDS_ALICE="http://localhost:$PORT_ALICE"
    export ATCHESS_TEST_PDS_BOB="http://localhost:$PORT_BOB"
    export ATCHESS_TEST_ALICE_HANDLE="$ALICE_HANDLE"
    export ATCHESS_TEST_ALICE_PASS="$ALICE_PASS"
    export ATCHESS_TEST_BOB_HANDLE="$BOB_HANDLE"
    export ATCHESS_TEST_BOB_PASS="$BOB_PASS"
    go test -v -count=1 -timeout 120s ./test/e2e/...
  )
}

# --- main -------------------------------------------------------------------

cd "$PROJECT_ROOT"

case "${1:-}" in
  --up)
    do_up
    log "Harness left running. Tear down with: $0 --down"
    exit 0
    ;;
  --down)
    do_down
    exit 0
    ;;
  --no-test)
    trap do_down EXIT
    do_up
    exit 0
    ;;
  ""|--test)
    if [ "${KEEP_UP:-0}" = "1" ]; then
      do_up
      do_test
    else
      trap do_down EXIT
      do_up
      do_test
    fi
    ;;
  *)
    echo "Usage: $0 [--up|--down|--no-test|--test]" >&2
    exit 1
    ;;
esac
