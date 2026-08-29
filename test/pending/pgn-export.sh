#!/usr/bin/env bash
# A finished game can be exported as PGN, and the PGN replays to the same game.
#
# Slice: atchess-b2d.3 · Flag: atchess.pgn_export
#
# WHY THIS DEMO EXISTS BEFORE THE CODE (pipeline#adr-003). This file is the work
# order, the definition of done, and — once `pipe deploy` promotes it out of
# test/pending/ — the permanent regression test. It is red until the slice
# lands, which is the expected state and why `make test` skips test/pending/.
#
# WHAT A HUMAN SHOULD SEE at gate 2: a real PGN for a game they can look up,
# with the Seven Tag Roster filled in from the actual players, and a Result that
# agrees with how the game actually ended. The interesting assertion is not
# "the endpoint returns 200" — it is that the exported history REPLAYS to the
# position the game is actually in. A PGN that parses but reconstructs a
# different game is the failure this is really watching for, and it looks
# entirely fine to anyone eyeballing the output.
#
# FLAGS. Overrides arrive through curl's own config file: `pipe demo` writes a
# .curlrc setting `X-Fleet-Flags` and points CURL_HOME at it, so every curl
# below picks the header up without this script mentioning it. That is what
# lets the same demo run against flags-as-configured (dark: 404) and against
# flags-plus-this-one (the feature). Do not add the header by hand — and do not
# switch these calls to wget or a Go helper, which would silently stop
# receiving the override and make a dark slice look shipped.
set -uo pipefail
cd "$(dirname "$0")/.."

PROTOCOL_URL="${PROTOCOL_URL:-http://localhost:8080}"

fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; fail=1; }

# The slice is dark unless the flag is on for this request. With no override
# the endpoint must not exist — that is what "deployed dark" means, and it is
# asserted rather than assumed so that a slice which quietly shipped enabled
# fails here instead of at whoever notices later.
if [ -z "${FLEET_FLAG_OVERRIDES:-}" ]; then
  # curl already prints 000 when it cannot connect, so do NOT add a `|| echo
  # 000` fallback: that appends a SECOND 000 and yields "000000", which matches
  # no case. An earlier version of this file did exactly that and fell through
  # to a catch-all that reported ok — so with both services down, the demo
  # printed PASS. A demo that passes when nothing is deployed is worse than no
  # demo, because it is evidence.
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
         "$PROTOCOL_URL/api/games/any/pgn" 2>/dev/null)
  case "$code" in
    404|403)
      ok "dark by default: /pgn is $code with the flag off" ;;
    000|"")
      bad "the protocol service is not answering at $PROTOCOL_URL — nothing was demonstrated" ;;
    200)
      bad "/pgn served 200 with NO flag override — the slice shipped enabled, not dark" ;;
    *)
      # Anything else is a service in a state this demo does not understand.
      # Unknown is a failure, never a pass: the whole job here is to be the
      # evidence, and evidence that shrugs is not evidence.
      bad "/pgn returned an unexpected $code with the flag off" ;;
  esac
  printf '\n'
  [ "$fail" -eq 0 ] && echo "PASS pgn-export (dark)" || echo "FAIL pgn-export"
  exit "$fail"
fi

# --- with the flag on ---------------------------------------------------------

# A finished game to export. The demo makes its own rather than depending on
# fixture data that another slice may delete: a demo that needs the database to
# already contain the right thing is a demo that goes red for unrelated reasons.
GAME=$(curl -fsS --max-time 10 -X POST "$PROTOCOL_URL/api/games/demo-fixture" 2>/dev/null \
       | grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
if [ -z "$GAME" ]; then
  bad "could not create a game to export (POST /api/games/demo-fixture)"
  printf '\n'; echo "FAIL pgn-export"; exit 1
fi
ok "created game $GAME"

PGN=$(curl -fsS --max-time 10 "$PROTOCOL_URL/api/games/$GAME/pgn" 2>/dev/null)
if [ -z "$PGN" ]; then
  bad "GET /api/games/$GAME/pgn returned nothing with the flag on"
  printf '\n'; echo "FAIL pgn-export"; exit 1
fi
ok "exported $(printf '%s' "$PGN" | wc -c) bytes of PGN"

# The Seven Tag Roster. A PGN missing these is not a PGN any other tool will
# take, which is the entire reason to export the format rather than our own.
for tag in Event Site Date Round White Black Result; do
  if grep -q "^\[$tag " <<<"$PGN"; then
    ok "has [$tag]"
  else
    bad "PGN is missing the [$tag] tag"
  fi
done

# Result must agree with the game, not be a constant. "*" here means the
# exporter never looked at how the game ended.
RESULT=$(grep -oE '^\[Result "[^"]*"\]' <<<"$PGN" | sed 's/.*"\(.*\)".*/\1/')
case "$RESULT" in
  "1-0"|"0-1"|"1/2-1/2") ok "Result is $RESULT" ;;
  "*") bad "Result is \"*\" — the exporter did not read the game's outcome" ;;
  *)   bad "Result is $RESULT, which is not a legal PGN result" ;;
esac

# THE ASSERTION THAT MATTERS. Replay the exported PGN and compare the position
# to the live game. Everything above can pass on a PGN that describes a
# different game; this cannot.
LIVE_FEN=$(curl -fsS --max-time 10 "$PROTOCOL_URL/api/games/$GAME" 2>/dev/null \
           | grep -oE '"fen"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
REPLAY_FEN=$(curl -fsS --max-time 10 -X POST "$PROTOCOL_URL/api/games/replay" \
             --data-binary "$PGN" -H 'Content-Type: application/x-chess-pgn' 2>/dev/null \
             | grep -oE '"fen"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')

if [ -z "$LIVE_FEN" ]; then
  bad "could not read the live game's FEN to compare against"
elif [ -z "$REPLAY_FEN" ]; then
  bad "replaying the exported PGN produced no position — the export does not round-trip"
elif [ "$LIVE_FEN" = "$REPLAY_FEN" ]; then
  ok "the exported PGN replays to the live position"
  ok "  $LIVE_FEN"
else
  bad "the exported PGN replays to a DIFFERENT game"
  bad "  live:     $LIVE_FEN"
  bad "  replayed: $REPLAY_FEN"
fi

printf '\n'
[ "$fail" -eq 0 ] && echo "PASS pgn-export" || echo "FAIL pgn-export"
exit "$fail"
