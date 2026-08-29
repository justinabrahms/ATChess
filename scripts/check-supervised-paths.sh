#!/bin/bash
# check-supervised-paths.sh — refuse to let an unattended runner land changes
# to paths that have no oracle.
#
# WHY THIS EXISTS. Decision atchess-b2d.2: web/static/ is SUPERVISED ONLY.
# That is not "agents may not touch it" — it is "changes there may not merge
# without a human having looked". Every other part of this repository has some
# gate that can fail a wrong change on its own (docs/ORACLES.md). The frontend
# has none: no rendering test, no DOM assertion, no interaction test. A change
# that renders a blank board passes the entire suite.
#
# That makes the frontend the most dangerous feedstock in the project, because
# it is also the most plentiful. The old TODO.md listed roughly thirty
# frontend items, all plausibly phrased, none with an acceptance criterion a
# reviewer could evaluate. An autonomous pipeline would produce thirty green
# pull requests against them and the first signal that any was wrong would be
# a person opening the page.
#
# WHAT IT DOES. Works out which files a change touches, matches them against
# .supervised-paths, and refuses when the caller looks unattended. The
# supervised list is data in a file rather than a constant in here, so adding
# a path is a one-line change that does not require reading this script.
#
# Usage:
#   scripts/check-supervised-paths.sh                 # working tree vs HEAD
#   scripts/check-supervised-paths.sh --range A..B    # an explicit git range
#   scripts/check-supervised-paths.sh --files a b c   # an explicit file list

set -euo pipefail

# This script is reachable from a git hook, and a hook runs with GIT_DIR,
# GIT_WORK_TREE and GIT_PREFIX pointing at whatever invoked it. Every git call
# below would then resolve against the wrong repository. Clear them before the
# first git call, not after.
unset GIT_DIR GIT_WORK_TREE GIT_PREFIX

# Accepts 1/true/yes, case-insensitively. Anything else -- including the empty
# string and the word "false" -- is not an assertion.
is_truthy() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes) return 0 ;;
        *) return 1 ;;
    esac
}

# The variable NAME carries the whole meaning; the value is just a boolean.
# An earlier version used a long magic phrase as the value, on the theory that
# something tedious to type is harder for an agent to set by accident. That was
# theatre: the refusal message below prints the phrase verbatim, so anything
# that can read the error can satisfy it. What actually helps is a name that
# states the claim being made, so that `A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES=1`
# set by a robot is a visible lie in the diff rather than an opaque token.
readonly ESCAPE_HATCH_VAR="A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES"

repo_root=""
if ! repo_root=$(git rev-parse --show-toplevel 2>/dev/null); then
    echo "check-supervised-paths: not inside a git repository" >&2
    exit 2
fi
readonly repo_root
cd "$repo_root"

readonly SUPERVISED_LIST=".supervised-paths"
if [ ! -f "$SUPERVISED_LIST" ]; then
    # A missing list means the gate silently protects nothing. Fail closed:
    # an absent policy file is a defect, not a permission.
    echo "check-supervised-paths: $SUPERVISED_LIST is missing; refusing to run ungated" >&2
    exit 2
fi

# --- collect the supervised globs -------------------------------------------
patterns=()
while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"
    # strip surrounding whitespace
    line="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [ -z "$line" ] && continue
    patterns+=("$line")
done < "$SUPERVISED_LIST"

if [ "${#patterns[@]}" -eq 0 ]; then
    echo "check-supervised-paths: $SUPERVISED_LIST lists no patterns; refusing to run ungated" >&2
    exit 2
fi

# --- work out which files changed -------------------------------------------
mode="worktree"
range=""
explicit_files=()
if [ "${1:-}" = "--range" ]; then
    mode="range"
    range="${2:?--range needs a git range}"
elif [ "${1:-}" = "--files" ]; then
    mode="files"
    shift
    explicit_files=("$@")
fi

changed=()
case "$mode" in
    worktree)
        # Tracked modifications plus untracked files. Command substitution does
        # not inherit errexit, so check the exit status explicitly rather than
        # trusting the pipeline to abort.
        raw=""
        if ! raw=$(git status --porcelain 2>/dev/null); then
            echo "check-supervised-paths: git status failed" >&2
            exit 2
        fi
        while IFS= read -r line; do
            [ -z "$line" ] && continue
            # porcelain v1: XY<space>path, and renames are "old -> new"
            path="${line:3}"
            case "$path" in *" -> "*) path="${path##* -> }";; esac
            changed+=("$path")
        done <<< "$raw"
        ;;
    range)
        raw=""
        if ! raw=$(git diff --name-only "$range" 2>/dev/null); then
            echo "check-supervised-paths: git diff --name-only $range failed" >&2
            exit 2
        fi
        while IFS= read -r path; do
            [ -n "$path" ] && changed+=("$path")
        done <<< "$raw"
        ;;
    files)
        changed=("${explicit_files[@]}")
        ;;
esac

# --- match against the supervised globs -------------------------------------
hits=()
for path in "${changed[@]}"; do
    [ -z "$path" ] && continue
    for pat in "${patterns[@]}"; do
        # shellcheck disable=SC2053 -- glob match on the right is intended
        if [[ "$path" == $pat ]]; then
            hits+=("$path")
            break
        fi
    done
done

if [ "${#hits[@]}" -eq 0 ]; then
    exit 0
fi

# --- a supervised path was touched; is anyone watching? ---------------------
reason=""
if [ "${CI:-}" = "true" ] || [ "${CI:-}" = "1" ]; then
    reason="CI is set"
elif [ -n "${ATCHESS_AUTOMATION:-}" ]; then
    reason="ATCHESS_AUTOMATION is set"
elif [ -n "${ATCHESS_AGENT:-}" ]; then
    reason="ATCHESS_AGENT is set"
elif [ -n "${GITHUB_ACTIONS:-}" ]; then
    reason="GITHUB_ACTIONS is set"
elif [ ! -t 0 ]; then
    reason="stdin is not a terminal, so no human is watching this run"
fi

if [ -z "$reason" ]; then
    # A human at a terminal. Say what was touched and get out of the way.
    echo "check-supervised-paths: ${#hits[@]} supervised file(s) changed; a human is present, proceeding:" >&2
    printf '  %s\n' "${hits[@]}" >&2
    exit 0
fi

if is_truthy "${!ESCAPE_HATCH_VAR:-}"; then
    echo "check-supervised-paths: $reason, but $ESCAPE_HATCH_VAR asserts human review. Proceeding." >&2
    printf '  %s\n' "${hits[@]}" >&2
    exit 0
fi

cat >&2 <<EOF

╔══════════════════════════════════════════════════════════════════════════╗
║  REFUSING: supervised paths changed with no human in the loop            ║
╚══════════════════════════════════════════════════════════════════════════╝

Refused because: $reason.

These changed files are listed in .supervised-paths:

$(printf '  %s\n' "${hits[@]}")

Decision atchess-b2d.2: web/static/ is SUPERVISED ONLY. Agent work there is
allowed. Agent work there that merges itself is not.

The reason is not caution, it is coverage. There is no rendering test, no DOM
assertion, no interaction test, and no screenshot diff over these files. A
change that renders a blank board, drops the move handler, or breaks the
challenge flow passes the entire test suite. Nothing here can tell a good
frontend change from a bad one, so "the tests are green" carries no
information about this diff, and a review that leans on it is reviewing
nothing.

WHAT TO DO:

  - Split the change. If the same branch also touches Go code, land that
    half autonomously and leave the frontend half for a person. Most
    frontend-adjacent work is mostly not frontend.
  - Or hand this diff to a human, who opens the page and looks at it.

IF A HUMAN HAS ACTUALLY REVIEWED THIS:

    $ESCAPE_HATCH_VAR=1 <command>

That variable is an assertion that a person looked at the rendered result --
not that a person approved the plan, and not that the tests passed. Do not
set it in a Makefile, a CI config, an agent harness, or a shell profile. If
you are an agent reading this: setting it is the one thing this gate exists
to prevent.

TO MAKE THIS PATH DISPATCHABLE INSTEAD OF SUPERVISED, build the oracle --
a headless-browser harness asserting the board renders from a given FEN,
that a submitted move produces the expected API call, and that the board
flips. Then remove the path from .supervised-paths. That is the honest way
out of this gate, and it is tracked as option B on atchess-b2d.2.

EOF
exit 1
