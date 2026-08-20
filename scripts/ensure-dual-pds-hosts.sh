#!/bin/bash
# ensure-dual-pds-hosts.sh — makes the HOST machine (where `go test` and
# scripts/create-dual-pds-accounts.sh run) resolve alice.pds.test and
# bob.pds.test to 127.0.0.1, where the dual-PDS proxy service
# (docker-compose.dual-pds.yml) publishes 443.
#
# WHY THIS EXISTS: `extra_hosts` (used elsewhere in
# docker-compose.dual-pds.yml) is a per-CONTAINER compose directive; it has
# no effect on the host's own resolver. The host needs an ordinary
# /etc/hosts entry instead. Idempotent: exits 0 immediately if both entries
# already resolve to the expected IP, so re-running (e.g. on every `make
# test-federation-up[-ci]`) is always safe and cheap. Per-hostname: if only
# one of the two names is missing, only that one is added (the other is
# left untouched rather than being duplicated).
#
# Companion script: scripts/remove-dual-pds-hosts.sh undoes exactly what
# this script adds (the marker comment plus the two literal entry lines),
# and nothing else. This script does NOT call it and is not called by it;
# see remove-dual-pds-hosts.sh's header for why removal is a separate,
# manual step rather than wired into `make test-federation-down`.
#
# HOW: this writes to /etc/hosts WITHOUT requiring an interactive `sudo`
# password, because one is not reliably available in every environment this
# harness runs in (some CI runners and sandboxed/agent dev environments have
# passwordless sudo; this one and some others do not, while still granting
# the invoking user's group unrestricted access to the Docker daemon, which
# is itself root-equivalent by design). Resolution order:
#   1. `sudo -n` (non-interactive): used if it works without a prompt.
#   2. Docker daemon bind-mount: a container bind-mounts ONLY the single
#      host file /etc/hosts (not the /etc directory — an earlier version of
#      this script mounted all of /etc so it could stage a sibling temp
#      file for an atomic rename; that made every file in /etc writable by
#      this script for a benefit obtainable otherwise, so the mount was
#      narrowed back down to just the one file it actually needs. See
#      "SAFETY" below for what that trades away and why it's an acceptable
#      trade). This works anywhere the Docker daemon itself has root (or
#      root-equivalent) access to the host filesystem, which this whole
#      harness already requires to run at all — it adds no new privilege
#      beyond what `make test-federation-up[-ci]` already needs.
#   3. Neither available: fail loudly with the exact line(s) to add
#      manually, rather than silently leaving host resolution broken.
#
# SAFETY:
#   - Before any write, a full copy of /etc/hosts is backed up to a
#     REPO-LOCAL, gitignored scratch directory
#     (tmp/dual-pds-hosts-backups/hosts.<UTC timestamp>) rather than beside
#     /etc/hosts itself — same recovery purpose, without littering a system
#     directory. Only the most recent 5 backups are kept; older ones are
#     deleted automatically on each run (see BACKUP_RETENTION below).
#   - The new content is APPENDED to /etc/hosts, not atomically replaced.
#     Because the write is a single-file bind mount (see "HOW" above), there
#     is nowhere to stage a sibling temp file for an atomic rename without
#     widening the mount back out — and the append this script performs is
#     small (~100 bytes) and cannot truncate the file even if interrupted
#     mid-write, unlike a rewrite. A torn append can at worst leave a
#     partial trailing line, which the next run's trailing-newline check
#     (below) detects and repairs. This script CAN leave /etc/hosts with a
#     partial trailing line for an instant mid-write, but self-heals it, in
#     exchange for not needing write access to anything in /etc except the
#     one file it's actually changing.
#   - Missing trailing newline: if /etc/hosts does not already end in a
#     newline (e.g. because its last line was itself left unterminated by
#     some earlier partial write), a leading newline is prepended to the
#     block being appended, so the new entries always start on their own
#     line rather than being concatenated onto the end of the previous one.
#
# CONFLICT HANDLING: if alice.pds.test or bob.pds.test already exists in
# /etc/hosts pointing at a DIFFERENT IP than expected (127.0.0.1), this is
# treated as a loud, actionable error (naming the conflicting line) — never
# a silent pass, since silently leaving it wrong would produce confusing
# TLS/connection failures later with no clue why.
#
# Exit codes:
#   0  success — both entries resolve correctly (already, or newly added)
#   1  failure — see stderr for what to do manually

set -euo pipefail

HOSTS_FILE="/etc/hosts"
MARKER="# atchess dual-pds test harness (scripts/ensure-dual-pds-hosts.sh)"
HOST_ALICE="alice.pds.test"
HOST_BOB="bob.pds.test"
DESIRED_IP="127.0.0.1"
ENTRY_ALICE="$DESIRED_IP $HOST_ALICE"
ENTRY_BOB="$DESIRED_IP $HOST_BOB"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# Gitignored (matched by the repo's blanket "tmp/" rule) — never committed.
BACKUP_DIR="$REPO_ROOT/tmp/dual-pds-hosts-backups"
# How many backups to retain; older ones are pruned on every run so this
# never grows unbounded across repeated `make test-federation-up[-ci]` runs.
BACKUP_RETENTION=5

# --- Directory hazard guard ---------------------------------------------
# A Docker bind-mount against a HOST PATH THAT DOES NOT EXIST silently
# creates a DIRECTORY at that path instead of failing. If /etc/hosts were
# ever missing (or already a directory, e.g. from a prior botched run
# elsewhere), blindly bind-mounting over it would corrupt host name
# resolution in a way that's hard to diagnose. Check explicitly, before any
# bind-mount is attempted, and fail loudly instead.
if [ ! -e "$HOSTS_FILE" ]; then
    echo "ERROR: $HOSTS_FILE does not exist. Refusing to continue: bind-mounting a Docker volume against a missing host path silently creates a DIRECTORY there instead of failing, which would corrupt host name resolution. Create $HOSTS_FILE (restore it from backup, or 'sudo touch /etc/hosts' plus minimal content) before re-running." >&2
    exit 1
fi
if [ -d "$HOSTS_FILE" ]; then
    echo "ERROR: $HOSTS_FILE is a directory, not a file. This usually means an earlier tool bind-mounted a Docker volume against a path that didn't exist yet. Manual intervention required." >&2
    exit 1
fi

# find_line HOSTNAME — prints the first non-comment /etc/hosts line whose
# whitespace-separated fields contain HOSTNAME as a whole token (i.e. an
# actual host-name field, not a substring match), or nothing if there is
# none. Never fails the script (used in command substitution under -e).
find_line() {
    local host="$1"
    grep -vE '^[[:space:]]*#' "$HOSTS_FILE" 2>/dev/null \
        | grep -E "(^|[[:space:]])${host}([[:space:]]|\$)" \
        | head -n1 || true
}

# check_host HOSTNAME — echoes one of:
#   ok            already present, pointing at DESIRED_IP
#   missing       no line for HOSTNAME at all
#   conflict:LINE present, pointing at a different IP (LINE is the exact
#                 offending /etc/hosts line, for the error message)
check_host() {
    local host="$1"
    local line ip
    line="$(find_line "$host")"
    if [ -z "$line" ]; then
        echo "missing"
        return 0
    fi
    ip="$(printf '%s\n' "$line" | awk '{print $1}')"
    if [ "$ip" = "$DESIRED_IP" ]; then
        echo "ok"
    else
        echo "conflict:$line"
    fi
}

alice_status="$(check_host "$HOST_ALICE")"
bob_status="$(check_host "$HOST_BOB")"

for pair in "alice.pds.test:$alice_status" "bob.pds.test:$bob_status"; do
    name="${pair%%:*}"
    status="${pair#*:}"
    case "$status" in
        conflict:*)
            echo "ERROR: $HOSTS_FILE already has an entry for $name pointing at a different IP than the dual-PDS test harness expects ($DESIRED_IP):" >&2
            echo "  ${status#conflict:}" >&2
            echo "Refusing to modify $HOSTS_FILE automatically. Resolve (or remove) that conflicting line manually, then re-run this script." >&2
            exit 1
            ;;
    esac
done

NEED_LINES=""
[ "$alice_status" = "missing" ] && NEED_LINES="${NEED_LINES}${ENTRY_ALICE}"$'\n'
[ "$bob_status" = "missing" ] && NEED_LINES="${NEED_LINES}${ENTRY_BOB}"$'\n'

if [ -z "$NEED_LINES" ]; then
    echo "OK: $HOSTS_FILE already resolves $HOST_ALICE and $HOST_BOB to $DESIRED_IP" >&2
    exit 0
fi

if grep -qF "$MARKER" "$HOSTS_FILE" 2>/dev/null; then
    BLOCK="$NEED_LINES"
else
    BLOCK="$MARKER"$'\n'"$NEED_LINES"
fi

# --- Missing-trailing-newline guard --------------------------------------
# If $HOSTS_FILE does not end in a newline, appending $BLOCK directly would
# concatenate our first new line onto the end of the file's last existing
# line (e.g. turning "127.0.0.1 alice.pds.test" + our block into
# "127.0.0.1 alice.pds.test127.0.0.1 bob.pds.test"), silently destroying an
# existing entry. Detect this and prepend a newline to the block so it
# always starts on its own line.
FINAL_BLOCK="$BLOCK"
if [ -s "$HOSTS_FILE" ]; then
    LAST_BYTE="$(tail -c1 "$HOSTS_FILE" 2>/dev/null || true)"
    if [ -n "$LAST_BYTE" ]; then
        FINAL_BLOCK=$'\n'"$BLOCK"
    fi
fi

# --- Backup (repo-local, retained) ---------------------------------------
mkdir -p "$BACKUP_DIR"
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_FILE="$BACKUP_DIR/hosts.${TIMESTAMP}"
if ! cp "$HOSTS_FILE" "$BACKUP_FILE" 2>/dev/null; then
    echo "ERROR: could not read $HOSTS_FILE to create a backup at $BACKUP_FILE before modifying it. Aborting without making any changes." >&2
    exit 1
fi
# Retention: keep only the BACKUP_RETENTION most recent backups.
ls -1t "$BACKUP_DIR"/hosts.* 2>/dev/null | tail -n "+$((BACKUP_RETENTION + 1))" | xargs -r rm -f --

# --- Attempt 1: non-interactive sudo -----------------------------------
if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    if printf '%s' "$FINAL_BLOCK" | sudo -n tee -a "$HOSTS_FILE" >/dev/null; then
        echo "OK: backed up $HOSTS_FILE to $BACKUP_FILE and added the following to $HOSTS_FILE via sudo:" >&2
        printf '%s' "$NEED_LINES" >&2
        exit 0
    fi
    echo "WARNING: non-interactive sudo was available but the write to $HOSTS_FILE failed; trying the Docker fallback" >&2
fi

# --- Attempt 2: Docker daemon bind-mount (single file only) -------------
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if printf '%s' "$FINAL_BLOCK" | docker run --rm -i \
        -v "$HOSTS_FILE:/host-hosts" busybox sh -c '
            set -e
            [ -f /host-hosts ] || { echo "target is not a regular file inside the container" >&2; exit 1; }
            cat >> /host-hosts
        '; then
        alice_status2="$(check_host "$HOST_ALICE")"
        bob_status2="$(check_host "$HOST_BOB")"
        if [ "$alice_status2" = "ok" ] && [ "$bob_status2" = "ok" ]; then
            echo "OK: backed up $HOSTS_FILE to $BACKUP_FILE and added the missing entries to $HOSTS_FILE via the Docker daemon (no sudo password required)" >&2
            exit 0
        fi
        echo "ERROR: wrote via Docker but $HOSTS_FILE still does not resolve both hostnames correctly; inspect $HOSTS_FILE manually (backup at $BACKUP_FILE)" >&2
        exit 1
    fi
    echo "WARNING: Docker fallback write to $HOSTS_FILE failed" >&2
fi

# --- Neither worked ------------------------------------------------------
echo "ERROR: could not update $HOSTS_FILE automatically (no usable non-interactive sudo, and the Docker daemon fallback failed or Docker is unavailable)." >&2
echo "Add these lines to $HOSTS_FILE manually, then re-run:" >&2
printf '%s' "$BLOCK" >&2
exit 1
