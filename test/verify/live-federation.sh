#!/usr/bin/env bash
# A whole game, between two real accounts, against the deployed site.
#
# WHAT THIS PROVES THAT NOTHING ELSE DID. Every other gate in this repository
# checks a part: the engine's move generation (perft), the endpoints' existence
# (deployed smoke), the unit's write grants, the page's API contract. None of
# them answers the only question a player has -- can two people actually play
# a game of chess? atchess-1c9.14 and atchess-1c9.16 have been open precisely
# because nobody had ever done it end to end.
#
# The answer, measured 2026-08-30: yes. Login, challenge, notification
# delivery, accept, alternating moves with turn enforcement, both players
# reading identical state across two accounts, and a real checkmate scoring
# 1-0.
#
# THIS WRITES REAL RECORDS TO REAL ACCOUNTS. Every run creates a challenge, a
# game and a handful of moves in the two players' repositories on bsky.social.
# That is the point -- a test against a fake PDS proves the fake works -- but
# it is why this is not in `make test` and never runs unattended. It needs
# test/.live-accounts.json, which is gitignored and holds APP passwords
# (revocable at https://bsky.app/settings/app-passwords), never real ones.
#
# Usage: bash test/verify/live-federation.sh
set -uo pipefail
cd "$(dirname "$0")/../.."

SITE="${ATCHESS_SITE:-https://atchess.abrah.ms}"
ACC="${ATCHESS_LIVE_ACCOUNTS:-test/.live-accounts.json}"

if [ ! -f "$ACC" ]; then
  echo "skip: no $ACC — this validation needs two real accounts with app passwords"
  exit 0
fi

fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; fail=1; }

login() { # login <alice|bob> -> session id on stdout
  jq -cn --arg h "$(jq -r ".$1.handle" "$ACC")" --arg p "$(jq -r ".$1.password" "$ACC")" \
     '{handle:$h,password:$p}' \
    | curl -s --max-time 30 -X POST -H 'Content-Type: application/json' -d @- \
        "$SITE/api/auth/login" | jq -r '.accessToken // empty'
}
api() { # api <session> <method> <path> [body]
  if [ -n "${4:-}" ]; then
    curl -s --max-time 60 -X "$2" -H "X-Session-ID: $1" -H 'Content-Type: application/json' -d "$4" "$SITE/api$3"
  else
    curl -s --max-time 60 -X "$2" -H "X-Session-ID: $1" "$SITE/api$3"
  fi
}
# {key} and game ids travel as the at:// URI in URL-safe base64, PADDING KEPT
# (internal/web/service.go decodes with StdEncoding after swapping -_ for +/).
b64url() { printf '%s' "$1" | base64 -w0 | tr '+/' '-_'; }

ASESS=$(login alice); BSESS=$(login bob)
[ -n "$ASESS" ] && ok "alice signed in" || { bad "alice could not sign in"; exit 1; }
[ -n "$BSESS" ] && ok "bob signed in"   || { bad "bob could not sign in"; exit 1; }

BH=$(jq -r '.bob.handle' "$ACC")

# --- challenge -----------------------------------------------------------
CH=$(api "$ASESS" POST /challenges "$(jq -cn --arg o "$BH" \
      '{opponent_did:$o,color:"white",message:"live federation check"}')")
CHURI=$(jq -r '.ID // .id // empty' <<<"$CH")
[ -n "$CHURI" ] && ok "alice challenged bob: ${CHURI##*/}" \
  || { bad "challenge not created: $(head -c 200 <<<"$CH")"; exit 1; }

# --- delivery ------------------------------------------------------------
# The challenge is written to alice's repo; bob has to be able to SEE it. This
# is the step that federation actually buys, and the one that used to be listed
# as missing.
NOTES=$(api "$BSESS" GET /challenge-notifications)
if jq -e --arg u "$CHURI" 'any(.[]; .challengeUri == $u)' >/dev/null 2>&1 <<<"$NOTES"; then
  ok "bob sees the challenge in his notifications"
else
  bad "bob does not see the challenge: $(head -c 200 <<<"$NOTES")"; exit 1
fi

# --- accept --------------------------------------------------------------
ACCEPT=$(api "$BSESS" POST "/challenge-notifications/$(b64url "$CHURI")/accept" '{}')
GID=$(jq -r '.id // empty' <<<"$ACCEPT")
FEN=$(jq -r '.fen // empty' <<<"$ACCEPT")
[ -n "$GID" ] && ok "bob accepted; game ${GID##*/}" \
  || { bad "accept produced no game: $(head -c 200 <<<"$ACCEPT")"; exit 1; }

# --- both players see the same game --------------------------------------
ENC=$(b64url "$GID")
AFEN=$(api "$ASESS" GET "/games/$ENC" | jq -r '.fen // empty')
BFEN=$(api "$BSESS" GET "/games/$ENC" | jq -r '.fen // empty')
if [ -n "$AFEN" ] && [ "$AFEN" = "$BFEN" ]; then
  ok "both players read the same position"
else
  bad "the two players disagree about the game: alice=$AFEN bob=$BFEN"
fi

# --- turn enforcement ----------------------------------------------------
# Black moving on white's turn must be refused. Without this a game is not a
# game, and it is the cheapest possible forgery to attempt.
OOT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 40 -X POST \
      -H "X-Session-ID: $BSESS" -H 'Content-Type: application/json' \
      -d "$(jq -cn --arg g "$GID" --arg f "$FEN" '{game_id:$g,from:"d7",to:"d5",fen:$f}')" \
      "$SITE/api/moves")
[ "$OOT" = 403 ] && ok "moving out of turn is refused (403)" \
  || bad "out-of-turn move answered $OOT, want 403"

# --- play to checkmate ---------------------------------------------------
# Scholar's mate. Chosen because it ends, quickly, in a result the engine must
# score correctly -- a game that merely accepts moves proves much less than one
# that knows it is over.
play() { # play <session> <from> <to> <who>
  local r san
  r=$(api "$1" POST /moves "$(jq -cn --arg g "$GID" --arg f "$2" --arg t "$3" --arg fen "$FEN" \
        '{game_id:$g,from:$f,to:$t,fen:$fen}')")
  san=$(jq -r '.san // empty' <<<"$r")
  FEN=$(jq -r '.fen // empty' <<<"$r")
  if [ -z "$san" ] || [ -z "$FEN" ]; then
    # DISTINGUISH AN UPSTREAM FLAKE FROM OUR BUG. Measured 2026-08-30: this
    # run failed on "Failed to record move" because bsky.social answered the
    # record write with HTTP 502 UpstreamFailure. The move had validated and
    # executed correctly; someone else's PDS blinked. The immediately
    # following run passed.
    #
    # Reporting that as a failure of ATChess would make this check flaky, and
    # a gate that fails good work stops being believed -- which is worse than
    # not having it. So an upstream failure is called out as such and exits
    # 2, distinct from a real regression's 1.
    if grep -qiE "upstreamfailure|502|upstream" <<<"$r" \
       || grep -qiE "upstreamfailure" <(api "$1" GET "/games/$(b64url "$GID")" 2>/dev/null); then
      printf '  \033[33mUPSTREAM\033[0m %s\n' \
        "$4 $2$3 could not be recorded: the opponent PDS failed the write, not ATChess" >&2
      UPSTREAM=1
      return 1
    fi
    bad "$4 $2$3 rejected: $(head -c 160 <<<"$r")"; return 1
  fi
  OVER=$(jq -r '.gameOver' <<<"$r"); RESULT=$(jq -r '.result' <<<"$r")
  return 0
}
OVER=false; RESULT=""; UPSTREAM=0
play "$ASESS" e2 e4 white && play "$BSESS" e7 e5 black \
  && play "$ASESS" f1 c4 white && play "$BSESS" b8 c6 black \
  && play "$ASESS" d1 h5 white && play "$BSESS" g8 f6 black \
  && play "$ASESS" h5 f7 white
if [ "$OVER" = true ] && [ "$RESULT" = "1-0" ]; then
  ok "checkmate reached and scored: Qxf7# 1-0"
else
  bad "the game did not end in a scored checkmate (gameOver=$OVER result=$RESULT)"
fi

printf '\n'
if [ "${UPSTREAM:-0}" -eq 1 ]; then
  echo "UPSTREAM the flow is correct up to the point where a remote PDS failed a write."
  echo "         Re-run before treating this as a regression in ATChess."
  exit 2
fi
[ "$fail" -eq 0 ] && echo "PASS two people can play a game of chess on $SITE" \
                  || echo "FAIL live federation check against $SITE"
exit "$fail"
