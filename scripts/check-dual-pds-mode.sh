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
# BOTH VOLUMES, NOT JUST ALICE'S (atchess-1c9.79): create-dual-pds-
# accounts.sh's ensure_account() re-uses each PDS's account independently
# per-PDS (scripts/create-dual-pds-accounts.sh's ensure_account(), ~line
# 104). A marker stamped only on pds-alice-data can't see a state where
# alice's volume is fresh/unmarked but bob's still holds the other mode's
# account — that half of the carry-over this guard exists to stop. So this
# script stamps and checks pds-alice-data AND pds-bob-data, and treats the
# two volumes' markers actively disagreeing with each other as a conflict
# in its own right, independent of which mode was requested.
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
#   - A file written *inside* pds-alice-data/pds-bob-data (mounted at /pds
#     by pds-alice/pds-bob respectively in docker-compose.dual-pds.yml,
#     present in both modes) lives and dies with exactly the volumes whose
#     contents determine create-dual-pds-accounts.sh's re-use behavior:
#     `down -v` removes them as a matter of course, nothing else does.
#
# NO-MARKER (LEGACY VOLUME) HANDLING: volumes created before this check
# existed (or before atchess-1c9.79 extended it to bob) have no marker
# file. This script treats "neither volume has an opinion" as "proceed and
# stamp both now" rather than "refuse" — refusing would strand any
# pre-existing dev/CI environment (forcing an unwanted, possibly-public-
# plc.directory-publishing `down -v` just to unblock it) for a condition
# nobody actually caused. If exactly one volume has a stamped mode and the
# other has none, the stamped one's mode is treated as authoritative (same
# legacy-tolerant spirit) and compared against the requested mode as
# before. From the moment `stamp` next runs, both volumes are marked and
# future mismatches — including a partial teardown of just one volume — ARE
# caught. A genuinely fresh machine (neither named volume exists at all) is
# unaffected either way: "check" exits 0 immediately without ever creating
# either volume.
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
#   0  safe to proceed (fresh volumes, matching mode, legacy no-marker
#      volume(s), or a force-reset was just performed)
#   1  mode mismatch (refused), the two volumes disagree with each other
#      (refused), an internal failure (e.g. could not resolve a docker
#      volume name), or a docker/API error while probing a volume
#      (atchess-1c9.79: this used to fail open and proceed; it now refuses)
#   2  usage error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.dual-pds.yml"

# Small, already-present-in-most-environments image used purely to read/write
# a one-line file inside the pds-alice-data/pds-bob-data volumes. Deliberately
# NOT reusing ghcr.io/bluesky-social/pds:latest here: this marker's lifecycle
# is a harness-internal concern, not a PDS concern, and shouldn't be coupled
# to that image's presence/tag.
MARKER_IMAGE="alpine:3"
MARKER_PATH="/pds/.atchess-harness-mode"
# Top-level `volumes:` keys in docker-compose.dual-pds.yml whose resolved
# (project-prefixed) names we key the marker to. Both PDSes, per
# atchess-1c9.79 — see the header comment above.
MARKER_VOLUME_KEYS="pds-alice-data pds-bob-data"

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

CONFIG_JSON=$(docker compose -f "$COMPOSE_FILE" config --format json 2>/dev/null) || CONFIG_JSON=""

ALICE_VOLUME_NAME=$(printf '%s' "$CONFIG_JSON" | jq -r '.volumes["pds-alice-data"].name // empty')
BOB_VOLUME_NAME=$(printf '%s' "$CONFIG_JSON" | jq -r '.volumes["pds-bob-data"].name // empty')

if [ -z "$ALICE_VOLUME_NAME" ] || [ -z "$BOB_VOLUME_NAME" ]; then
    echo "ERROR: could not resolve the docker volume names for $MARKER_VOLUME_KEYS from $COMPOSE_FILE (is 'docker compose config' working?)" >&2
    exit 1
fi

# volume_exists: distinguishes "volume genuinely does not exist" (ok — a
# fresh/never-created volume, or one that was explicitly torn down) from
# "docker couldn't be asked" (a daemon/API error, e.g. an unreachable
# DOCKER_HOST) via `docker volume inspect`'s stderr text, not just its exit
# status — inspect exits 1 for both cases, so exit status alone can't tell
# them apart. On a genuine "no such volume" it returns 1 (volume absent).
# On any other error it prints the docker error and *exits the whole
# script* rather than returning — atchess-1c9.79: this used to fail open
# (any inspect error == "absent" == proceed); a probe error is not
# evidence of anything and must not be treated as a green light.
#
# Per this fleet's shell hazards: don't trust a pipeline's exit status (the
# grep below runs on captured text, not piped output) and don't trust `set
# -e` across a command-substitution assignment — the exit status is
# captured explicitly into $status on its own line, immediately after the
# assignment and before any other command can change $?.
volume_exists() {
    local name="$1"
    local err status
    err=$(docker volume inspect "$name" 2>&1 >/dev/null)
    status=$?

    if [ "$status" -eq 0 ]; then
        return 0
    fi

    if printf '%s' "$err" | grep -qi "no such volume"; then
        return 1
    fi

    echo "ERROR: could not query docker for volume '$name': $err" >&2
    exit 1
}

# Prints the stamped mode ("local" or "ci") on stdout, or nothing if unset.
# Only called after volume_exists has already confirmed the volume exists.
read_marker() {
    local name="$1"
    docker run --rm -v "$name:/pds:ro" "$MARKER_IMAGE" \
        sh -c "cat $MARKER_PATH 2>/dev/null || true"
}

write_marker() {
    local name="$1"
    docker run --rm -v "$name:/pds" "$MARKER_IMAGE" \
        sh -c "printf '%s' '$MODE' > $MARKER_PATH"
}

# probe_volume_state: sets PROBE_STATE to one of:
#   "absent"     - the volume does not exist
#   "no-marker"  - the volume exists but has no mode marker (legacy/never
#                  stamped)
#   <mode>       - the volume exists and is stamped with that mode
probe_volume_state() {
    local name="$1"
    if ! volume_exists "$name"; then
        PROBE_STATE="absent"
        return
    fi

    local marker
    marker=$(read_marker "$name")
    if [ -z "$marker" ]; then
        PROBE_STATE="no-marker"
    else
        PROBE_STATE="$marker"
    fi
}

do_check() {
    probe_volume_state "$ALICE_VOLUME_NAME"
    local alice_state="$PROBE_STATE"
    probe_volume_state "$BOB_VOLUME_NAME"
    local bob_state="$PROBE_STATE"

    if [ "$alice_state" = "absent" ] && [ "$bob_state" = "absent" ]; then
        echo "check-dual-pds-mode: neither '$ALICE_VOLUME_NAME' nor '$BOB_VOLUME_NAME' exists yet; fresh stack, proceeding in '$MODE' mode." >&2
        exit 0
    fi

    # "absent" and "no-marker" both mean "this volume has no opinion about
    # mode"; anything else is an actual stamped mode.
    local alice_mode="" bob_mode=""
    case "$alice_state" in
        absent|no-marker) ;;
        *) alice_mode="$alice_state" ;;
    esac
    case "$bob_state" in
        absent|no-marker) ;;
        *) bob_mode="$bob_state" ;;
    esac

    if [ -n "$alice_mode" ] && [ -n "$bob_mode" ] && [ "$alice_mode" != "$bob_mode" ]; then
        cat >&2 <<EOF
ERROR: dual-PDS harness volumes disagree with each other.

  '$ALICE_VOLUME_NAME' is stamped '$alice_mode' mode, but
  '$BOB_VOLUME_NAME' is stamped '$bob_mode' mode.

  This normally means one PDS's volume was reset (or removed) on its own
  outside of 'make test-federation-down' while the other PDS's volume —
  and the account/identity on it — was left in place. Proceeding would
  risk create-dual-pds-accounts.sh silently mixing identities from two
  different modes.

  Resolve this by running:
    make test-federation-down

  ...and then re-running this target. To skip this check and reset
  automatically instead, set ATCHESS_HARNESS_FORCE_RESET=1.
EOF
        exit 1
    fi

    local existing_mode="${alice_mode:-$bob_mode}"

    if [ -z "$existing_mode" ]; then
        echo "check-dual-pds-mode: no mode marker present on '$ALICE_VOLUME_NAME' or '$BOB_VOLUME_NAME' (predates this check, or predates atchess-1c9.79's bob marker); proceeding in '$MODE' mode and stamping both now." >&2
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

  The existing dual-PDS volumes ('$ALICE_VOLUME_NAME' and/or
  '$BOB_VOLUME_NAME') were created in '$existing_mode' mode, but '$MODE'
  mode was just requested.

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
    write_marker "$ALICE_VOLUME_NAME"
    write_marker "$BOB_VOLUME_NAME"
    echo "check-dual-pds-mode: stamped '$ALICE_VOLUME_NAME' and '$BOB_VOLUME_NAME' as '$MODE' mode." >&2
}

case "$ACTION" in
    check) do_check ;;
    stamp) do_stamp ;;
esac
