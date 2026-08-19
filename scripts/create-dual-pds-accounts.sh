#!/bin/bash
# Idempotent account provisioning for the dual-PDS local test harness.
#
# Creates (or re-uses, if already present) one test account on each of two
# independently-running PDS instances started via docker-compose.dual-pds.yml:
#   - alice on PDS-A (default http://localhost:2583)
#   - bob   on PDS-B (default http://localhost:2584)
#
# Writes machine-readable account data (handle, did, pdsUrl, password) for
# both players to test/.harness-accounts.json, consumed by downstream test
# harness code (see atchess-1c9.2).
#
# Usage: ./scripts/create-dual-pds-accounts.sh
#
# Exit codes:
#   0  success — both accounts exist and are usable; JSON written
#   1  failure — see stderr for which PDS/account/step failed and why

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PDS_A_URL="${PDS_A_URL:-http://localhost:2583}"
PDS_B_URL="${PDS_B_URL:-http://localhost:2584}"

ALICE_HANDLE="alice.test"
ALICE_EMAIL="alice@chess.test"
ALICE_PASSWORD="alice-harness-pass-1"

BOB_HANDLE="bob.test"
BOB_EMAIL="bob@chess.test"
BOB_PASSWORD="bob-harness-pass-1"

OUTPUT_FILE="$REPO_ROOT/test/.harness-accounts.json"

HEALTH_TIMEOUT_SECS=60
HEALTH_POLL_INTERVAL_SECS=1

# ---------------------------------------------------------------------------
# Health gate: poll describeServer until 200 or deadline. No bare sleep loop
# without a bound; this always terminates and reports the last status seen.
# ---------------------------------------------------------------------------
wait_for_pds() {
    local url="$1"
    local name="$2"
    local deadline=$((SECONDS + HEALTH_TIMEOUT_SECS))
    local last_status="000"
    local curl_exit=0

    echo "Waiting for $name at $url (up to ${HEALTH_TIMEOUT_SECS}s)..." >&2

    while [ "$SECONDS" -lt "$deadline" ]; do
        last_status=$(curl -s -o /dev/null -w "%{http_code}" \
            --max-time 5 \
            "$url/xrpc/com.atproto.server.describeServer" 2>/dev/null) || curl_exit=$?
        if [ "$curl_exit" -eq 0 ] && [ "$last_status" = "200" ]; then
            echo "$name is up (HTTP 200)." >&2
            return 0
        fi
        curl_exit=0
        sleep "$HEALTH_POLL_INTERVAL_SECS"
    done

    echo "ERROR: $name at $url did not become healthy within ${HEALTH_TIMEOUT_SECS}s (last HTTP status: $last_status)" >&2
    return 1
}

# ---------------------------------------------------------------------------
# json_escape: minimal JSON string escaping without external deps for values
# we control (handles/urls/dids/passwords contain no control chars here, but
# escape backslash/quote defensively anyway).
# ---------------------------------------------------------------------------
json_escape() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    printf '%s' "$s"
}

# ---------------------------------------------------------------------------
# ensure_account: idempotently create (or re-use) an account on a PDS.
# Prints the account's DID on stdout on success (and ONLY the DID — callers
# use $(...) capture). All diagnostics go to stderr. Returns nonzero on any
# failure, including a password mismatch on a pre-existing account.
# ---------------------------------------------------------------------------
ensure_account() {
    local pds_url="$1"
    local pds_label="$2"
    local handle="$3"
    local email="$4"
    local password="$5"

    local http_code body did curl_exit

    curl_exit=0
    body=$(curl -s -w '\n%{http_code}' --max-time 15 \
        -X POST "$pds_url/xrpc/com.atproto.server.createAccount" \
        -H "Content-Type: application/json" \
        -d "{\"email\":\"$(json_escape "$email")\",\"handle\":\"$(json_escape "$handle")\",\"password\":\"$(json_escape "$password")\"}") || curl_exit=$?

    if [ "$curl_exit" -ne 0 ]; then
        echo "ERROR: curl failed talking to $pds_label ($pds_url) during account creation for $handle (curl exit $curl_exit)" >&2
        return 1
    fi

    http_code="${body##*$'\n'}"
    body="${body%$'\n'*}"

    if [ "$http_code" = "200" ]; then
        did=$(printf '%s' "$body" | grep -o '"did":"[^"]*"' | head -1 | cut -d'"' -f4)
        if [ -z "$did" ]; then
            echo "ERROR: $pds_label returned HTTP 200 for createAccount($handle) but no did was found in the response: $body" >&2
            return 1
        fi
        echo "Created $handle on $pds_label (did: $did)" >&2
        printf '%s' "$did"
        return 0
    fi

    if [ "$http_code" = "400" ] && printf '%s' "$body" | grep -qi "Handle already taken"; then
        echo "$handle already exists on $pds_label; verifying credentials..." >&2

        curl_exit=0
        local session_body session_http
        session_body=$(curl -s -w '\n%{http_code}' --max-time 15 \
            -X POST "$pds_url/xrpc/com.atproto.server.createSession" \
            -H "Content-Type: application/json" \
            -d "{\"identifier\":\"$(json_escape "$handle")\",\"password\":\"$(json_escape "$password")\"}") || curl_exit=$?

        if [ "$curl_exit" -ne 0 ]; then
            echo "ERROR: curl failed talking to $pds_label ($pds_url) while verifying existing account $handle (curl exit $curl_exit)" >&2
            return 1
        fi

        session_http="${session_body##*$'\n'}"
        session_body="${session_body%$'\n'*}"

        if [ "$session_http" = "200" ]; then
            did=$(printf '%s' "$session_body" | grep -o '"did":"[^"]*"' | head -1 | cut -d'"' -f4)
            if [ -z "$did" ]; then
                echo "ERROR: $pds_label createSession for $handle returned HTTP 200 but no did was found in the response: $session_body" >&2
                return 1
            fi
            echo "Re-using existing $handle on $pds_label (did: $did)" >&2
            printf '%s' "$did"
            return 0
        elif [ "$session_http" = "401" ]; then
            echo "ERROR: $handle already exists on $pds_label but the harness password does not match (HTTP 401 on createSession). The PDS volume likely has a stale account from an earlier run with a different password. Run 'make test-federation-down' to reset, then retry." >&2
            return 1
        else
            echo "ERROR: $pds_label createSession for $handle failed with HTTP $session_http: $session_body" >&2
            return 1
        fi
    fi

    echo "ERROR: $pds_label createAccount for $handle failed with HTTP $http_code: $body" >&2
    return 1
}

main() {
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl is required but not found on PATH" >&2
        exit 1
    fi

    wait_for_pds "$PDS_A_URL" "PDS-A" || exit 1
    wait_for_pds "$PDS_B_URL" "PDS-B" || exit 1

    local alice_did bob_did

    if ! alice_did=$(ensure_account "$PDS_A_URL" "PDS-A" "$ALICE_HANDLE" "$ALICE_EMAIL" "$ALICE_PASSWORD"); then
        echo "ERROR: failed to provision alice on PDS-A" >&2
        exit 1
    fi

    if ! bob_did=$(ensure_account "$PDS_B_URL" "PDS-B" "$BOB_HANDLE" "$BOB_EMAIL" "$BOB_PASSWORD"); then
        echo "ERROR: failed to provision bob on PDS-B" >&2
        exit 1
    fi

    if [ -z "$alice_did" ] || [ -z "$bob_did" ]; then
        echo "ERROR: refusing to write harness accounts file with an empty did (alice='$alice_did' bob='$bob_did')" >&2
        exit 1
    fi

    mkdir -p "$(dirname "$OUTPUT_FILE")"
    cat > "$OUTPUT_FILE" <<EOF
{
  "alice": {
    "handle": "$(json_escape "$ALICE_HANDLE")",
    "did": "$(json_escape "$alice_did")",
    "pdsUrl": "$(json_escape "$PDS_A_URL")",
    "password": "$(json_escape "$ALICE_PASSWORD")"
  },
  "bob": {
    "handle": "$(json_escape "$BOB_HANDLE")",
    "did": "$(json_escape "$bob_did")",
    "pdsUrl": "$(json_escape "$PDS_B_URL")",
    "password": "$(json_escape "$BOB_PASSWORD")"
  }
}
EOF

    echo "Wrote harness account data to $OUTPUT_FILE" >&2
    echo "  alice: $ALICE_HANDLE ($alice_did) on $PDS_A_URL" >&2
    echo "  bob:   $BOB_HANDLE ($bob_did) on $PDS_B_URL" >&2
}

main "$@"
