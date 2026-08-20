#!/bin/bash
# check-dual-pds-mode.sh — guards against silently mixing the dual-PDS
# harness's two PLC-directory modes (atchess-1c9.22).
#
# WHY THIS EXISTS: docker-compose.dual-pds.yml's pds-alice/pds-bob volumes
# (pds-alice-data, pds-bob-data) persist real PDS account state across
# `up`/`up -d` invocations. If the harness is brought up once in CI mode
# (PDS_DID_PLC_URL -> the hermetic local-plc service) and later brought up
# again in local mode (PDS_DID_PLC_URL -> the real, public
# https://plc.directory) WITHOUT first running `docker compose down -v`,
# scripts/create-dual-pds-accounts.sh's idempotent "handle already taken"
# re-use path (see its ensure_account()) just re-reads whatever DIDs are
# already on the still-mounted volumes and re-emits them into
# test/.harness-accounts.json — silently carrying identities minted under
# one PLC directory into a file/mode that claims a different one. Neither
# mode is wrong in isolation; the silent carry-over between them is the bug.
# See docker-compose.dual-pds.yml's header comment for the two modes.
#
# WHAT THIS SCRIPT DOES: stamps a small marker recording which mode
# (`local` or `ci`) the dual-PDS volumes currently belong to, and refuses
# (nonzero exit, explicit message) to proceed when the requested mode
# doesn't match. Invoked by both `make test-federation-up` and
# `make test-federation-up-ci` (see Makefile) so the logic lives in exactly
# one place instead of being duplicated per target.
#
# MARKER STORAGE — WHY A FILE INSIDE A NAMED VOLUME, NOT A FILE IN THE REPO
# OR A CONTAINER LABEL: the marker MUST be destroyed by exactly the same
# operation that destroys the PDS state it describes
# (`docker compose ... down -v`, which is what `make test-federation-down`
# runs), or it becomes a lie the next time the harness starts:
#   - A marker file in the repo/tmp would SURVIVE `down -v` (volumes gone,
#     marker file still on disk) and cause a FALSE refusal on a stack that
#     is actually clean — worse than the bug being fixed.
#   - A marker baked into a container (label, or a file in the container's
#     writable layer) would NOT survive a plain `docker compose down`
#     (containers are always removed by `down`, with or without `-v`) even
#     though the volumes — and the identities on them — are NOT removed
#     without `-v`. That marker would disappear too early and mask a real
#     mix-up.
#   - A file written *inside* pds-alice-data (mounted at /pds by pds-alice
#     in docker-compose.dual-pds.yml, present in both modes) lives and dies
#     with exactly the volume whose contents determine
#     create-dual-pds-accounts.sh's re-use behavior: `down -v` removes it as
#     a matter of course, nothing else does.
#
# NO-MARKER (LEGACY VOLUME) HANDLING: volumes created before this check
# existed have no marker file. This script treats that as "proceed and
# stamp now" rather than "refuse" — refusing would strand any pre-existing
# dev/CI environment (forcing an unwanted, possibly-public-plc.directory-
# publishing `down -v` just to unblock it) for a condition nobody actually
# caused. From the moment this script's `stamp` action runs, the volume is
# marked and future mismatches ARE caught. A genuinely fresh machine (the
# named volume does not exist at all) is unaffected either way: "check"
# exits 0 immediately without ever creating the volume.
#
# Usage:
#   check-dual-pds-mode.sh <local|ci> check   # run BEFORE `docker compose up`
#   check-dual-pds-mode.sh <local|ci> stamp   # run AFTER `up` succeeds,
#                                              # BEFORE account provisioning
#
# Escape hatch: ATCHESS_HARNESS_FORCE_RESET=1 makes a "check" mismatch
# perform `docker compose -f docker-compose.dual-pds.yml --profile ci
# down -v` automatically instead of refusing. Opt-in only, never the
# default — silently destroying volumes is just a different unpleasant
# surprise.
#
# Exit codes:
#   0  safe to proceed (fresh volume, matching mode, legacy no-marker
#      volume, or a force-reset was just performed)
#   1  mode mismatch (refused) or an internal failure (e.g. could not
#      resolve the docker volume name)
#   2  usage error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.dual-pds.yml"

# Small, already-present-in-most-environments image used purely to read/write
# a one-line file inside the pds-alice-data volume. Deliberately NOT reusing
# ghcr.io/bluesky-social/pds:latest here: this marker's lifecycle is a
# harness-internal concern, not a PDS concern, and shouldn't be coupled to
# that image's presence/tag.
MARKER_IMAGE="alpine:3"
MARKER_PATH="/pds/.atchess-harness-mode"
# Top-level `volumes:` key in docker-compose.dual-pds.yml whose resolved
# (project-prefixed) name we key the marker to.
MARKER_VOLUME_KEY="pds-alice-data"

usage() {
    echo "Usage: $0 <local|ci> <check|stamp>" >&2
    exit 2
}

[ $# -eq 2 ] || usage
MODE="$1"
ACTION="$2"

case "$MODE" in
    local|ci) ;;
    *) echo "ERROR: mode must be 'local' or 'ci', got '$MODE'" >&2; usage ;;
esac

case "$ACTION" in
    check|stamp) ;;
    *) echo "ERROR: action must be 'check' or 'stamp', got '$ACTION'" >&2; usage ;;
esac

if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: docker is required but not found on PATH" >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "ERROR: jq is required but not found on PATH" >&2
    exit 1
fi

cd "$REPO_ROOT"

VOLUME_NAME=$(docker compose -f "$COMPOSE_FILE" config --format json 2>/dev/null \
    | jq -r --arg k "$MARKER_VOLUME_KEY" '.volumes[$k].name // empty')

if [ -z "$VOLUME_NAME" ]; then
    echo "ERROR: could not resolve the docker volume name for '$MARKER_VOLUME_KEY' from $COMPOSE_FILE (is 'docker compose config' working?)" >&2
    exit 1
fi

volume_exists() {
    docker volume inspect "$VOLUME_NAME" >/dev/null 2>&1
}

# Prints the stamped mode ("local" or "ci") on stdout, or nothing if unset.
read_marker() {
    docker run --rm -v "$VOLUME_NAME:/pds:ro" "$MARKER_IMAGE" \
        sh -c "cat $MARKER_PATH 2>/dev/null || true"
}

write_marker() {
    docker run --rm -v "$VOLUME_NAME:/pds" "$MARKER_IMAGE" \
        sh -c "printf '%s' '$MODE' > $MARKER_PATH"
}

do_check() {
    if ! volume_exists; then
        echo "check-dual-pds-mode: '$VOLUME_NAME' does not exist yet; fresh stack, proceeding in '$MODE' mode." >&2
        exit 0
    fi

    local existing_mode
    existing_mode=$(read_marker)

    if [ -z "$existing_mode" ]; then
        echo "check-dual-pds-mode: '$VOLUME_NAME' exists but has no mode marker (predates this check); proceeding in '$MODE' mode and stamping it now." >&2
        exit 0
    fi

    if [ "$existing_mode" = "$MODE" ]; then
        echo "check-dual-pds-mode: existing volumes already belong to '$MODE' mode; proceeding." >&2
        exit 0
    fi

    if [ "${ATCHESS_HARNESS_FORCE_RESET:-}" = "1" ]; then
        echo "check-dual-pds-mode: volumes belong to '$existing_mode' mode but '$MODE' mode was requested. ATCHESS_HARNESS_FORCE_RESET=1 is set; running 'docker compose -f $COMPOSE_FILE --profile ci down -v' to reset." >&2
        docker compose -f "$COMPOSE_FILE" --profile ci down -v
        exit 0
    fi

    cat >&2 <<EOF
ERROR: dual-PDS harness mode mismatch.

  The existing dual-PDS volumes (starting with '$VOLUME_NAME') were created
  in '$existing_mode' mode, but '$MODE' mode was just requested.

  Bringing '$MODE' mode up on top of '$existing_mode'-mode volumes would
  silently carry '$existing_mode' mode's identities (DIDs) into '$MODE'
  mode's account file (test/.harness-accounts.json), defeating the
  hermeticity '$MODE' mode is supposed to provide.

  Resolve this by running:
    make test-federation-down

  ...and then re-running this target. To skip this check and reset
  automatically instead, set ATCHESS_HARNESS_FORCE_RESET=1.
EOF
    exit 1
}

do_stamp() {
    write_marker
    echo "check-dual-pds-mode: stamped '$VOLUME_NAME' as '$MODE' mode." >&2
}

case "$ACTION" in
    check) do_check ;;
    stamp) do_stamp ;;
esac
