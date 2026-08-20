#!/bin/bash

# End-to-end test runner for ATChess.
#
# atchess-1c9.26: the legacy single-PDS e2e suite (localhost:3000,
# player1.test/player2.test) was retired. The remaining test/e2e/*.go tests
# (ownership_test.go, federation_test.go, challenge_delivery_test.go) build
# on test/harness, which spins up its own protocol-service instances per
# test and talks to the dual-PDS local harness stack
# (alice.test@https://alice.pds.test, bob.test@https://bob.pds.test)
# started via 'make test-federation-up'/'make test-federation-up-ci'. This
# script only preflights that stack; it does not start or stop it.

set -e

echo "🧪 Running ATChess End-to-End Tests"
echo "=================================="

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

ACCOUNTS_FILE="$REPO_ROOT/test/.harness-accounts.json"
PDS_A_URL="${PDS_A_URL:-https://alice.pds.test}"
PDS_B_URL="${PDS_B_URL:-https://bob.pds.test}"
DUAL_PDS_CACERT="${DUAL_PDS_CACERT:-$REPO_ROOT/certs/dual-pds/ca.pem}"

fail_with_remedy() {
    echo "❌ $1" >&2
    echo "   Start the dual-PDS harness stack first:" >&2
    echo "   make test-federation-up-ci" >&2
    exit 1
}

CURL_CA_ARGS=()
if [ -f "$DUAL_PDS_CACERT" ]; then
    CURL_CA_ARGS=(--cacert "$DUAL_PDS_CACERT")
fi

echo "🔍 Checking dual-PDS harness preconditions..."

if [ ! -f "$ACCOUNTS_FILE" ]; then
    fail_with_remedy "Harness account data not found at $ACCOUNTS_FILE."
fi
echo "✅ Harness account data found"

if ! curl -f -s "${CURL_CA_ARGS[@]}" "$PDS_A_URL/xrpc/com.atproto.server.describeServer" >/dev/null 2>&1; then
    fail_with_remedy "$PDS_A_URL is not answering."
fi
echo "✅ $PDS_A_URL is up"

if ! curl -f -s "${CURL_CA_ARGS[@]}" "$PDS_B_URL/xrpc/com.atproto.server.describeServer" >/dev/null 2>&1; then
    fail_with_remedy "$PDS_B_URL is not answering."
fi
echo "✅ $PDS_B_URL is up"

# Run the e2e tests
echo ""
echo "🚀 Running end-to-end tests..."
echo ""

# Run with verbose output and race detection. Federated tests poll across
# two PDSes and spin up protocol-service instances per test, so this needs
# considerably more headroom than a typical unit test timeout.
go test -v -race -tags=e2e -timeout 20m ./test/e2e/...

echo ""
echo "🎉 All end-to-end tests completed successfully!"
