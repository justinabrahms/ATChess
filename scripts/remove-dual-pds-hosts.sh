#!/bin/bash
# remove-dual-pds-hosts.sh — removes exactly what
# scripts/ensure-dual-pds-hosts.sh adds to /etc/hosts (the marker comment
# line and the two literal `127.0.0.1 alice.pds.test` /
# `127.0.0.1 bob.pds.test` entry lines), and nothing else. Any other line in
# /etc/hosts — including a manually-added alice.pds.test/bob.pds.test entry
# that doesn't match those exact strings — is left untouched.
#
# Idempotent: exits 0 immediately (no write, no backup) if none of the
# three lines are present.
#
# NOT WIRED INTO `make test-federation-down` (DELIBERATELY):
#   Automatic removal on every down would be tidier in isolation, but
#   /etc/hosts is host-global state while `test-federation-down` runs per
#   harness invocation. Two problems that would create:
#     1. A normal dev loop of `up` -> test -> `down` -> `up` -> ... would
#        rewrite /etc/hosts (backup + replace) on every single cycle for no
#        benefit — the entries are inert (127.0.0.1 with nothing listening)
#        once the containers are down, so there is nothing to gain from
#        removing them promptly.
#     2. Two harnesses/developers/CI jobs touching the same machine (e.g. a
#        long-running dev instance plus a CI runner sharing a host, or two
#        terminals) would fight over /etc/hosts: one's `down` would delete
#        entries the other's still-running `up` depends on.
#   Removal is therefore a separate, explicit, manual step — run this
#   script yourself when you actually want the host machine's name
#   resolution cleaned up (e.g. before decommissioning a dev box, or to
#   verify `ensure-dual-pds-hosts.sh` still works from a clean state).
#
# HOW: mirrors ensure-dual-pds-hosts.sh's privilege approach:
#   1. `sudo -n` (non-interactive) if available. sudo runs as root on the
#      real host filesystem (no bind mount involved), so it can stage a
#      sibling temp file next to /etc/hosts and rename it into place
#      atomically, same as before.
#   2. Otherwise, a Docker daemon bind-mount of ONLY the single host file
#      /etc/hosts (not the /etc directory — see ensure-dual-pds-hosts.sh's
#      header for why the mount was narrowed). Removal genuinely needs a
#      REWRITE (filtering lines out), not just an append, and a
#      single-file bind mount cannot host a sibling temp file for an
#      atomic rename the way a directory mount could. Instead: filter the
#      content, write it to a temp file that lives entirely INSIDE the
#      container's own filesystem (never touches the host), then write
#      that filtered content back into the mounted file IN PLACE (`cat >
#      /host-hosts`, not `mv`, since `mv` would replace the mounted
#      file's inode with a new one that only exists inside the container
#      and never reaches the host). This is not atomic the way the
#      directory-mount version was, but it never widens write access
#      beyond the one file this script actually changes.
#   3. Neither available: fail loudly with the exact line(s) to remove
#      manually.
#
# SAFETY: before any write, a full copy of /etc/hosts is backed up to a
# REPO-LOCAL, gitignored scratch directory
# (tmp/dual-pds-hosts-backups/hosts.<UTC timestamp>) rather than beside
# /etc/hosts itself. Only the most recent 5 backups are kept; older ones
# are pruned automatically on every run.
#
# Exit codes:
#   0  success — none of the three lines are present (already, or just now)
#   1  failure — see stderr for what to do manually

set -euo pipefail

HOSTS_FILE="/etc/hosts"
MARKER="# atchess dual-pds test harness (scripts/ensure-dual-pds-hosts.sh)"
ENTRY_ALICE="127.0.0.1 alice.pds.test"
ENTRY_BOB="127.0.0.1 bob.pds.test"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# Gitignored (matched by the repo's blanket "tmp/" rule) — never committed.
BACKUP_DIR="$REPO_ROOT/tmp/dual-pds-hosts-backups"
# How many backups to retain; older ones are pruned on every run so this
# never grows unbounded across repeated runs.
BACKUP_RETENTION=5

# --- Directory hazard guard (see ensure-dual-pds-hosts.sh for rationale) --
if [ ! -e "$HOSTS_FILE" ]; then
    echo "ERROR: $HOSTS_FILE does not exist. Refusing to continue: bind-mounting a Docker volume against a missing host path silently creates a DIRECTORY there instead of failing." >&2
    exit 1
fi
if [ -d "$HOSTS_FILE" ]; then
    echo "ERROR: $HOSTS_FILE is a directory, not a file. Manual intervention required." >&2
    exit 1
fi

anything_to_remove() {
    grep -qxF -e "$MARKER" -e "$ENTRY_ALICE" -e "$ENTRY_BOB" "$HOSTS_FILE" 2>/dev/null
}

if ! anything_to_remove; then
    echo "OK: $HOSTS_FILE contains none of the atchess dual-pds harness lines; nothing to remove" >&2
    exit 0
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
    PERM="$(stat -c '%a' "$HOSTS_FILE" 2>/dev/null || echo 644)"
    TMP_FILE="$(sudo -n mktemp "${HOSTS_FILE}.atchess-tmp.XXXXXX" 2>/dev/null || true)"
    if [ -n "$TMP_FILE" ]; then
        # shellcheck disable=SC2064 -- intentionally expand TMP_FILE now
        trap "sudo -n rm -f '$TMP_FILE'" EXIT
        if sudo -n chmod "$PERM" "$TMP_FILE" \
            && { grep -vxF -e "$MARKER" -e "$ENTRY_ALICE" -e "$ENTRY_BOB" "$HOSTS_FILE" | sudo -n tee "$TMP_FILE" >/dev/null || true; } \
            && sudo -n mv -f "$TMP_FILE" "$HOSTS_FILE"; then
            trap - EXIT
            if ! anything_to_remove; then
                echo "OK: backed up $HOSTS_FILE to $BACKUP_FILE and removed the atchess dual-pds harness lines via sudo" >&2
                exit 0
            fi
            echo "ERROR: wrote via sudo but $HOSTS_FILE still contains atchess harness lines; inspect manually (backup at $BACKUP_FILE)" >&2
            exit 1
        fi
        trap - EXIT
        sudo -n rm -f "$TMP_FILE" 2>/dev/null || true
    fi
    echo "WARNING: non-interactive sudo was available but the write to $HOSTS_FILE failed; trying the Docker fallback" >&2
fi

# --- Attempt 2: Docker daemon bind-mount (single file only) -------------
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if docker run --rm -i \
        -e ATCHESS_MARKER="$MARKER" \
        -e ATCHESS_ENTRY_ALICE="$ENTRY_ALICE" \
        -e ATCHESS_ENTRY_BOB="$ENTRY_BOB" \
        -v "$HOSTS_FILE:/host-hosts" busybox sh -c '
            set -e
            [ -f /host-hosts ] || { echo "target is not a regular file inside the container" >&2; exit 1; }
            tmp="$(mktemp)"
            trap "rm -f \"$tmp\"" EXIT
            grep -vxF -e "$ATCHESS_MARKER" -e "$ATCHESS_ENTRY_ALICE" -e "$ATCHESS_ENTRY_BOB" /host-hosts > "$tmp" || true
            cat "$tmp" > /host-hosts
        '; then
        if ! anything_to_remove; then
            echo "OK: backed up $HOSTS_FILE to $BACKUP_FILE and removed the atchess dual-pds harness lines via the Docker daemon (no sudo password required)" >&2
            exit 0
        fi
        echo "ERROR: wrote via Docker but $HOSTS_FILE still contains atchess harness lines; inspect manually (backup at $BACKUP_FILE)" >&2
        exit 1
    fi
    echo "WARNING: Docker fallback write to $HOSTS_FILE failed" >&2
fi

# --- Neither worked ------------------------------------------------------
echo "ERROR: could not update $HOSTS_FILE automatically (no usable non-interactive sudo, and the Docker daemon fallback failed or Docker is unavailable)." >&2
echo "Remove these line(s) from $HOSTS_FILE manually if present, then re-run:" >&2
echo "  $MARKER" >&2
echo "  $ENTRY_ALICE" >&2
echo "  $ENTRY_BOB" >&2
exit 1
