#!/usr/bin/env bash
# check-unit-drift.sh — refuse to deploy when the systemd unit running on the
# server is not the one in this repository.
#
# WHY THIS EXISTS. On 2026-08-30 a deploy took atchess.abrah.ms down for twenty
# minutes: the build had gained a SQLite challenge store, the unit granted only
# the log directory under ProtectSystem=strict, and the service crash-looped on
# a read-only filesystem. A test was then added asserting the repo's unit
# grants every path the service writes.
#
# That test would not have caught it. The repo's template and the installed
# unit had drifted completely apart -- different ExecStart, Group, logging and
# ReadWritePaths -- because nothing has ever copied one to the other. The
# deploy only ever ran `systemctl restart`. So there were two units, one tested
# and one running, and the tested one was not the one that mattered. A gate on
# an artifact nobody deploys is decoration.
#
# WHY THIS DETECTS RATHER THAN CORRECTS. The obvious fix is to have the deploy
# install the unit. That needs write access to /etc/systemd/system, which is
# root code execution: whoever can push to the default branch could then
# rewrite any unit as root. This repository is a workload for an autonomous
# agent pipeline, so "can push to main" is a wider set than "should be root".
# The deploy user's sudo is deliberately seven commands -- systemctl
# stop/restart/status/daemon-reload on these two units and nothing else.
#
# The units are mode 644, so drift can be OBSERVED without any new privilege.
# This script does that and fails the deploy. A human applies a unit change
# once, by hand; after that, drift cannot recur without the next deploy
# stopping and printing exactly what differs.
#
# Usage:
#   deploy/scripts/check-unit-drift.sh                 # ssh target from env
#   SSH_TARGET=justin@abrah.ms deploy/scripts/check-unit-drift.sh
#   SSH_OPTS="-i ~/.ssh/deploy_key -p 22" ... (as CI passes it)

set -uo pipefail

# Reachable from a git hook, where GIT_DIR and friends point at whatever
# invoked it and would make the repo-root lookup below resolve elsewhere.
unset GIT_DIR GIT_WORK_TREE GIT_PREFIX

SSH_TARGET="${SSH_TARGET:-justin@abrah.ms}"
# shellcheck disable=SC2206 -- deliberate word splitting; these are ssh flags
SSH_OPTS_ARR=(${SSH_OPTS:--o BatchMode=yes -o ConnectTimeout=10})

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "check-unit-drift: not inside a git repository" >&2
  exit 2
}
cd "$repo_root"

fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; fail=1; }

# directives strips comments and blank lines, so a template may carry the
# rationale for its own contents without that counting as drift. Everything
# else -- including ordering -- is compared verbatim, because a systemd unit is
# small enough that "close enough" is not a useful category.
directives() {
  grep -vE '^[[:space:]]*(#|$)' "$1" | sed -e 's/[[:space:]]*$//'
}

check_unit() { # check_unit <service-name>
  local svc=$1
  local repo_file="deploy/systemd/${svc}.service"
  local live_file remote_path="/etc/systemd/system/${svc}.service"

  if [ ! -f "$repo_file" ]; then
    bad "$svc: no template at $repo_file"
    return
  fi

  live_file=$(mktemp)
  # Command substitution does not inherit errexit and this script does not set
  # it, so check the status explicitly rather than trusting the pipeline.
  if ! ssh "${SSH_OPTS_ARR[@]}" "$SSH_TARGET" "cat $remote_path" >"$live_file" 2>/dev/null; then
    bad "$svc: could not read $remote_path on $SSH_TARGET"
    rm -f "$live_file"
    return
  fi
  if [ ! -s "$live_file" ]; then
    bad "$svc: $remote_path is empty or unreadable — treating as drift, not as agreement"
    rm -f "$live_file"
    return
  fi

  local d
  d=$(diff -u <(directives "$repo_file") <(directives "$live_file") 2>/dev/null)
  if [ -z "$d" ]; then
    ok "$svc: the installed unit matches $repo_file"
  else
    bad "$svc: the installed unit does NOT match $repo_file"
    echo "" >&2
    echo "  --- $repo_file (what is tested)" >&2
    echo "  +++ $remote_path (what is running)" >&2
    sed 's/^/  /' <<<"$d" >&2
    echo "" >&2
    echo "  The unit under test is not the unit in production, so every gate that" >&2
    echo "  reads the template is describing a file nobody runs. Apply the change" >&2
    echo "  on the server by hand:" >&2
    echo "" >&2
    echo "    scp $repo_file $SSH_TARGET:/tmp/${svc}.service" >&2
    echo "    ssh $SSH_TARGET 'sudo install -m 0644 /tmp/${svc}.service $remote_path \\" >&2
    echo "       && sudo systemctl daemon-reload && sudo systemctl restart ${svc}'" >&2
    echo "" >&2
    echo "  Or, if the RUNNING unit is the correct one, copy it into the repo" >&2
    echo "  instead and commit that — reconciling in either direction is fine," >&2
    echo "  disagreeing silently is not." >&2
  fi
  rm -f "$live_file"
}

echo "checking installed units on $SSH_TARGET against deploy/systemd/"
check_unit atchess-protocol
check_unit atchess-web

printf '\n'
if [ "$fail" -eq 0 ]; then
  echo "PASS the units in production are the units in this repository"
else
  echo "FAIL the units in production have drifted from this repository"
fi
exit "$fail"
