#!/usr/bin/env bash
# Is the DEPLOYED thing actually usable? Run by `make verify` after a deploy.
#
# Not a unit test and not a demo for one slice. This asks the question nobody
# was asking: a human opens https://atchess.abrah.ms, signs in with their own
# PDS, and tries to play. Every step of that crosses a boundary the Go suite
# cannot see — DNS, the PLC directory, someone else's OAuth server, a reverse
# proxy, and whichever binary is actually on disk over there.
#
# THE TRAP THIS ENCODES, and the reason it checks methods rather than paths.
# Measured 2026-08-30: `GET /api/games` on the live site answers 404. That
# looks exactly like "the endpoint was never deployed", and it was read that
# way for a while. It is not. gorilla/mux answers a METHOD mismatch with 404
# by default -- /api/games is registered POST-only, so a GET finds no matching
# route and 404s. `POST /api/games` answers 401, which is the route saying "I
# am here, authenticate". So a GET probe cannot distinguish a missing endpoint
# from a wrong verb, and using one to conclude anything about a deployment is
# how you end up rewriting a backend that was fine. Probe with the verb the UI
# actually uses, and treat 401/403 as PRESENT.
set -uo pipefail
cd "$(dirname "$0")/.."

SITE="${ATCHESS_SITE:-https://atchess.abrah.ms}"
# A handle whose PDS is NOT bsky.social. The whole point of building on AT
# Protocol is that someone else's PDS works; if this only ever gets tested with
# a bsky.social handle, the federation claim is untested marketing.
HANDLE="${ATCHESS_SMOKE_HANDLE:-justin.abrah.ms}"
EXPECT_PDS="${ATCHESS_SMOKE_PDS:-eurosky.social}"

fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; fail=1; }

code() { # code <method> <path> [body]
  local m=$1 p=$2 b=${3:-}
  if [ "$m" = GET ]; then
    curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$SITE$p"
  else
    curl -s -o /dev/null -w '%{http_code}' --max-time 15 -X "$m" \
         -H 'Content-Type: application/json' -d "${b:-{\}}" "$SITE$p"
  fi
}

# --- the page and its OAuth identity -----------------------------------------
[ "$(code GET /)" = 200 ] && ok "the page loads" || bad "the page did not load"

CM=$(curl -s --max-time 15 "$SITE/client-metadata.json" 2>/dev/null)
if grep -q '"client_id"' <<<"$CM" && grep -q '"jwks"' <<<"$CM"; then
  ok "client-metadata.json is served with a client_id and jwks"
else
  bad "client-metadata.json is missing or malformed — OAuth cannot start without it"
fi
# The client_id MUST equal the URL it is served from, or every authorization
# server rejects the request. It is a self-referential document and the one
# field a hostname change silently breaks.
if grep -q "\"client_id\":\"$SITE/client-metadata.json\"" <<<"$CM"; then
  ok "client_id matches the URL it is served from"
else
  bad "client_id does not match $SITE/client-metadata.json — authorization servers will refuse this client"
fi

# --- every endpoint the UI calls, with the verb the UI uses -------------------
# PRESENT means: not 404. 401/403 are the route answering. See the header.
# PRESENT means the route exists AND the service behind it is alive.
#
# The first version of this function treated everything except 404 and 000 as
# ok, which meant a 502 -- the backend crash-looping and Caddy having nothing
# to proxy to -- was reported as "deployed". Measured 2026-08-30: a deploy took
# the protocol service down entirely, and this check printed five green lines
# about endpoints that were answering only in the sense that a reverse proxy
# was answering for them. That is the same mistake as reading a 404 as absence:
# believing a status code means what it looks like it means.
#
# 401/403 are the strongest evidence a route exists -- something authenticated
# enough to refuse you. 5xx is the service being broken, and is never ok here.
present() { # present <label> <method> <path>
  local c; c=$(code "$2" "$3")
  case "$c" in
    401|403|200|204|400|409|422)
      ok "$1: $2 $3 -> $c (deployed and answering)" ;;
    404)
      bad "$1: $2 $3 -> 404 with the verb the UI uses; the route is not deployed" ;;
    000)
      bad "$1: $2 $3 -> no answer at all; the site is unreachable" ;;
    5*)
      bad "$1: $2 $3 -> $c; the service behind the proxy is DOWN, not merely unauthenticated" ;;
    *)
      bad "$1: $2 $3 -> $c, which this check does not recognise; treat unknown as broken" ;;
  esac
}
present "session"      GET  /api/auth/session
present "notifications" GET /api/challenge-notifications
present "create game"  POST /api/games
present "challenge"    POST /api/challenges
present "move"         POST /api/moves

# --- the federation claim, tested against a third-party PDS ------------------
# This is the assertion worth having. It exercises handle -> DID -> DID
# document -> PDS -> that PDS's OAuth metadata -> its authorize endpoint. Five
# network hops through infrastructure this project does not own, and the thing
# a real user does first.
AUTH=$(curl -s --max-time 25 -X POST -H 'Content-Type: application/json' \
        -d "{\"handle\":\"$HANDLE\"}" "$SITE/api/auth/oauth/login" 2>/dev/null)
if grep -q '"authorization_url"' <<<"$AUTH"; then
  ok "login resolves $HANDLE to an authorization URL"
  if grep -q "$EXPECT_PDS" <<<"$AUTH"; then
    ok "  and it points at $EXPECT_PDS — a PDS that is not bsky.social"
  else
    bad "  but it does not point at $EXPECT_PDS: $(head -c 160 <<<"$AUTH")"
  fi
else
  bad "login could not resolve $HANDLE: $(head -c 200 <<<"$AUTH")"
fi

# A handle that cannot exist must be refused, not silently accepted. Without
# this the check above passes on a build that hands back an authorization URL
# for anything, which is worse than no login at all.
NEG=$(curl -s --max-time 20 -X POST -H 'Content-Type: application/json' \
       -d '{"handle":"definitely-not-a-real-handle-xyzzy.invalid"}' \
       "$SITE/api/auth/oauth/login" 2>/dev/null)
if grep -q '"authorization_url"' <<<"$NEG"; then
  bad "a nonsense handle produced an authorization URL — resolution is not really happening"
elif [ "$fail" -ne 0 ]; then
  # Do not claim this one when the service is already failing: with the backend
  # down, EVERY handle is "refused" and this line reads as a passing control
  # while proving nothing. Measured 2026-08-30 during an outage, where it was
  # the single green line among failures.
  echo "  --   nonsense handle: not asserted, the service is already failing"
else
  ok "a nonsense handle is refused"
fi

printf '\n'
if [ "$fail" -eq 0 ]; then
  echo "PASS deployed smoke against $SITE"
else
  echo "FAIL deployed smoke against $SITE"
fi
exit "$fail"
