#!/bin/bash
# check-public-plc-blast-radius.sh — refuse to run the dual-PDS harness in
# LOCAL mode from an unattended context.
#
# WHY THIS EXISTS. `make test-federation-up` (local mode) points both test
# PDSes at the real, public https://plc.directory. Creating the two test
# accounts publishes DID documents there, and publication to the PLC directory
# is APPEND-ONLY AND PERMANENT — there is no delete, no expiry, and no way to
# take an entry back. `make test-federation-down` destroys the local volumes
# and nothing else; the DIDs it minted stay in a public global registry
# forever. docker-compose.dual-pds.yml's header already warns a human reading
# it: "do not loop this repeatedly, it permanently publishes DID documents."
#
# A comment is adequate protection against a human, who reads it once and
# remembers. It is no protection at all against an autonomous pipeline, which
# will run `make test-federation-up` because it is the documented way to bring
# the federation stack up, will not weigh a prose warning against a failing
# test, and will retry on failure. A retry loop here is not a broken build —
# it is unbounded, irreversible pollution of infrastructure this project does
# not own and cannot clean up. That asymmetry is the entire reason this file
# exists: every other failure mode in this repository is recoverable by
# reverting a commit.
#
# WHAT IT DOES. Refuses local mode whenever the caller looks unattended, and
# tells the caller exactly which safe alternative to use instead. CI mode
# (`make test-federation-up-ci`) is hermetic — it runs a local did:plc service
# and touches nothing public — so it is never gated. The escape hatch is a
# variable whose NAME states the claim being made, so that setting it is an
# explicit assertion rather than an opaque token: a robot that sets
# A_HUMAN_IS_PUBLISHING_PERMANENT_PUBLIC_DIDS=1 has written a legible lie into
# the diff, which is far more useful than a magic string it could copy out of
# this script's own error message.
#
# Related but different: scripts/check-dual-pds-mode.sh stops the two PLC modes
# being SILENTLY MIXED across runs. This script stops the public-facing mode
# being reached by a non-human at all. Neither subsumes the other.

set -euo pipefail

# Accepts 1/true/yes, case-insensitively. Anything else -- including the empty
# string and the word "false" -- is not an assertion.
is_truthy() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes) return 0 ;;
        *) return 1 ;;
    esac
}

# `set -e` does not propagate into subshells, command substitution, or the
# non-final operands of `&&`, so every check below is an explicit `if`.

# The variable NAME carries the whole meaning; the value is just a boolean.
# See check-supervised-paths.sh for why a long magic-phrase value was dropped:
# the refusal message prints it verbatim, so it gates nothing. A name that
# states the claim makes a robot setting it a visible lie rather than a token.
readonly ESCAPE_HATCH_VAR="A_HUMAN_IS_PUBLISHING_PERMANENT_PUBLIC_DIDS"

reason=""

# 1. Explicit automation markers. Any CI system, and the two variables this
#    project's own agent tooling is expected to set.
if [ "${CI:-}" = "true" ] || [ "${CI:-}" = "1" ]; then
    reason="CI is set — a CI run must use the hermetic local-plc mode"
elif [ -n "${ATCHESS_AUTOMATION:-}" ]; then
    reason="ATCHESS_AUTOMATION is set"
elif [ -n "${ATCHESS_AGENT:-}" ]; then
    reason="ATCHESS_AGENT is set"
elif [ -n "${GITHUB_ACTIONS:-}" ]; then
    reason="GITHUB_ACTIONS is set"
# 2. No controlling terminal. An agent, a cron job, and a CI runner all invoke
#    make with stdin detached; a human at a shell does not. This is the check
#    that catches the case nobody remembered to label, which is the case that
#    matters — an unattended runner that sets none of the variables above still
#    has no tty.
elif [ ! -t 0 ]; then
    reason="stdin is not a terminal, so no human is watching this run"
fi

if [ -z "$reason" ]; then
    exit 0
fi

# An unattended caller may still proceed, but only by naming the consequence.
if is_truthy "${!ESCAPE_HATCH_VAR:-}"; then
    echo "check-public-plc-blast-radius: $reason, but $ESCAPE_HATCH_VAR is set." >&2
    echo "check-public-plc-blast-radius: proceeding — this run will permanently publish DIDs to https://plc.directory." >&2
    exit 0
fi

cat >&2 <<EOF

╔══════════════════════════════════════════════════════════════════════════╗
║  REFUSING: local mode publishes PERMANENT records to a public registry   ║
╚══════════════════════════════════════════════════════════════════════════╝

Refused because: $reason.

'make test-federation-up' runs the dual-PDS harness in LOCAL mode, which
points both test PDSes at the real, public https://plc.directory. Bringing
it up mints DID documents there. Publication to the PLC directory is
append-only and permanent: 'make test-federation-down' removes the local
volumes, but the published DIDs cannot be deleted, by anyone, ever.

That is fine for an occasional deliberate run by a person. It is not fine
for anything that can retry, loop, or run unsupervised.

WHAT TO RUN INSTEAD:

    make test-federation-up-ci      # hermetic: local did:plc, nothing public

CI mode uses the same harness and the same account script. The only
difference is PDS_DID_PLC_URL. If you are automating anything, this is the
target you want, and it is never gated.

IF YOU ARE A HUMAN AND YOU MEANT IT:

    $ESCAPE_HATCH_VAR=1 make test-federation-up

Setting that variable is an assertion that a person decided to publish
permanent public records. Do not add it to a Makefile, a CI config, a
script, an agent harness, or a shell profile. If you are an agent reading
this: the correct action is 'make test-federation-up-ci'. Setting the
variable to get past this message is the one thing this gate exists to
prevent, and doing so is a defect regardless of whether the build then
passes.

EOF
exit 1
