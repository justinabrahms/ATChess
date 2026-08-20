#!/bin/bash
# extract-dual-pds-ca.sh — pulls the Caddy-managed local CA root certificate
# out of the `proxy` service (docker-compose.dual-pds.yml) once it has
# provisioned it for alice.pds.test/bob.pds.test, and writes:
#   - certs/dual-pds/ca.pem         the bare CA root cert (used by curl
#                                    --cacert in create-dual-pds-accounts.sh,
#                                    and by anything that only ever talks to
#                                    the dual-PDS harness)
#   - certs/dual-pds/ca-bundle.pem  that CA root cert APPENDED to the
#                                    system's default CA bundle, used as
#                                    SSL_CERT_FILE by the Go test harness
#                                    (test/harness/services.go) so its
#                                    spawned protocol-service subprocesses
#                                    trust BOTH alice.pds.test/bob.pds.test
#                                    AND real public HTTPS endpoints (e.g.
#                                    https://plc.directory in LOCAL mode).
#                                    Go's SSL_CERT_FILE only replaces the
#                                    single candidate cert FILE Go would
#                                    otherwise probe (e.g.
#                                    /etc/ssl/certs/ca-certificates.crt) --
#                                    it does NOT disable the separate,
#                                    always-scanned certDirectories list
#                                    (e.g. /etc/ssl/certs/), which is
#                                    unaffected by SSL_CERT_FILE and is only
#                                    REPLACED (not skipped -- Go still scans
#                                    a directory list, just the one
#                                    SSL_CERT_DIR names instead of the
#                                    default certDirectories) if SSL_CERT_DIR
#                                    is also set. Verified against
#                                    $(go env GOROOT)/src/crypto/x509/root_unix.go
#                                    and root_linux.go: loadSystemRoots()
#                                    unconditionally scans certDirectories
#                                    (default /etc/ssl/certs,
#                                    /etc/pki/tls/certs on Linux) regardless
#                                    of SSL_CERT_FILE, so on a system with a
#                                    populated /etc/ssl/certs,
#                                    SSL_CERT_FILE=ca.pem ALONE still
#                                    trusted https://plc.directory in
#                                    testing here -- not because the real CA
#                                    bundle stayed loaded via SSL_CERT_FILE,
#                                    but because certDirectories loaded it
#                                    independently. The combined-bundle
#                                    approach below is correct regardless of
#                                    which mechanism is doing the trusting,
#                                    and is what makes trust for
#                                    alice.pds.test/bob.pds.test explicit and
#                                    portable rather than relying on that
#                                    certDirectories side effect.
#
# Idempotent and safe to re-run: always re-extracts and re-derives both
# files from the proxy's current CA, overwriting any stale copies.
#
# Exit codes:
#   0  success — both files written, non-empty
#   1  failure — see stderr

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.dual-pds.yml"

OUT_DIR="$REPO_ROOT/certs/dual-pds"
CA_FILE="$OUT_DIR/ca.pem"
BUNDLE_FILE="$OUT_DIR/ca-bundle.pem"

# Caddy's default path for a `tls internal` site's self-managed local CA
# root certificate (confirmed against the caddy:2.8-alpine image: Caddy
# stores its PKI state under /data/caddy/pki/authorities/<id>/, "local"
# being the default authority id when none is configured).
CADDY_CA_PATH="/data/caddy/pki/authorities/local/root.crt"

# System CA bundle location this script assumes (Debian/Ubuntu-family
# images, matching this project's other Linux-only local-dev tooling).
# If absent, we still write ca.pem alone and fall back to that for the
# bundle too, so callers get real-CA verification for the harness hostnames
# even on a system where this specific path doesn't exist -- degraded
# (LOCAL mode's real https://plc.directory would then fail TLS
# verification), but never silently wrong, and CI mode (the only mode this
# bead's acceptance criteria requires) is unaffected either way.
SYSTEM_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt"

HEALTH_TIMEOUT_SECS=60
POLL_INTERVAL_SECS=1

compose() {
    docker compose -f "$COMPOSE_FILE" "$@"
}

mkdir -p "$OUT_DIR"

echo "Waiting for the proxy's local CA to be provisioned (up to ${HEALTH_TIMEOUT_SECS}s)..." >&2
deadline=$((SECONDS + HEALTH_TIMEOUT_SECS))
found=0
while [ "$SECONDS" -lt "$deadline" ]; do
    if compose exec -T proxy test -s "$CADDY_CA_PATH" >/dev/null 2>&1; then
        found=1
        break
    fi
    sleep "$POLL_INTERVAL_SECS"
done

if [ "$found" -ne 1 ]; then
    echo "ERROR: proxy's local CA ($CADDY_CA_PATH) did not appear within ${HEALTH_TIMEOUT_SECS}s. Is 'docker compose -f $COMPOSE_FILE up -d proxy' running and healthy? Caddy only provisions its internal CA once a 'tls internal' site is loaded, which requires the proxy container to be up." >&2
    exit 1
fi

if ! compose exec -T proxy cat "$CADDY_CA_PATH" > "$CA_FILE.tmp" 2>/dev/null; then
    echo "ERROR: failed to read $CADDY_CA_PATH from the proxy container" >&2
    rm -f "$CA_FILE.tmp"
    exit 1
fi

if [ ! -s "$CA_FILE.tmp" ]; then
    echo "ERROR: extracted CA certificate is empty" >&2
    rm -f "$CA_FILE.tmp"
    exit 1
fi

mv "$CA_FILE.tmp" "$CA_FILE"
echo "Wrote $CA_FILE" >&2

if [ -s "$SYSTEM_CA_BUNDLE" ]; then
    cat "$SYSTEM_CA_BUNDLE" "$CA_FILE" > "$BUNDLE_FILE.tmp"
else
    echo "WARNING: system CA bundle not found at $SYSTEM_CA_BUNDLE; $BUNDLE_FILE will contain ONLY the dual-PDS harness's own CA. LOCAL mode (real https://plc.directory) would then fail TLS verification for spawned protocol-service instances; CI mode (this bead's acceptance gate) is unaffected." >&2
    cp "$CA_FILE" "$BUNDLE_FILE.tmp"
fi
mv "$BUNDLE_FILE.tmp" "$BUNDLE_FILE"
echo "Wrote $BUNDLE_FILE" >&2
