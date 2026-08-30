# Where game state actually lives

Read this before writing code that reads a game.

Three separate bugs in one afternoon (2026-08-30) came from the same wrong
assumption, and the docs were teaching it. This page is the correction.

## The one constraint everything follows from

**You can only write to your own repository.**

AT Protocol has no mechanism for writing a record into somebody else's repo, and
no amount of design makes one appear. This project has now learned it twice: the
cross-repo challenge notification was removed for being *impossible* rather than
merely broken, and the docs still claimed games were copied into both players'
repos "for redundancy" months afterwards.

Everything below is a consequence.

## What lives where

| Record | Whose repo | Written by |
|---|---|---|
| `app.atchess.challenge` | the challenger | the challenger |
| `app.atchess.game` | **the acceptor** | the acceptor, once |
| `app.atchess.move` | **each mover's own** | each player, per move |
| `app.atchess.resignation` | the resigning player | that player |
| `app.atchess.drawOffer` / `drawResponse` | their author | that author |
| `app.atchess.gameIndex` | each player's own | each player, about games elsewhere |

Verified against a live game on 2026-08-30: the game record sat in the
acceptor's repo, white's `d4` was in white's repo, and black's `d5` was in
black's. Two repos, one game, no copies.

## The consequence: no record is the truth

**A game's state is the replay of both players' move records.** It is not the
`fen` on the game record.

That stored `fen` can only be updated by whoever owns the game record — the
acceptor. When the opponent moves, the acceptor's record cannot learn about it,
because the opponent cannot write there. So the stored `fen` lags by every move
the opponent has made, and is only ever correct by coincidence.

Measured on one live game at one moment:

```
record   rnbqkbnr/pppppppp/8/8/3P4/8/PPP1PPPP/RNBQKBNR b KQkq d3 0 1     after 1.d4
derived  rnbqkbnr/ppp1pppp/8/3p4/3P4/8/PPP1PPPP/RNBQKBNR w KQkq d6 0 2   after 1.d4 d5
```

Black had moved. The record still held the position before it.

`atproto.Client.GetGame` exists to do this derivation, and returns
`DerivationIncomplete` when it could not read every repo it needed. **That flag
means the status is unproven, not merely stale** — a truncated scan cannot tell
"still active" from "a terminal event exists that I could not see" — so it must
never authorize a write.

## What this broke, so you can recognise the shape

Three bugs, one cause. If you find a fourth, it will look like these.

**The games list was empty.** A player's own repo holds no game record unless
they were the acceptor, so "list my games" found nothing. Finding a game you did
not create means scanning the repo of someone you have played — derived from
your challenges in both directions, then memoized into `gameIndex`.

**A challenge reads `pending` forever.** The acceptor cannot update the
challenger's challenge record. "Was my challenge accepted?" is answerable only
by looking for the game, matched via the challenge's `proposedGameId`.

**The turn indicator disagreed with the game view.** The list read the stored
`fen`, the game view derived it, and they were a move apart. The list now
derives too, and a row it cannot derive declines to say whose turn it is rather
than guess.

## Rules

1. **Never trust a game record's `fen` for whose turn it is, or whether the game
   is over.** Derive it. `GetGame` is the derivation.
2. **Never plan a write into another player's repo.** If a design needs one, the
   design is wrong, not the protocol.
3. **A record you want the other player to see goes in YOUR repo, and they read
   it.** That is what `gameIndex` is for, and it is why it is written by each
   player about games held elsewhere.
4. **"Could not read" and "not there" are different answers.** Collapsing them
   is how a listing silently loses a game. Report incompleteness; do not return
   a short list as if it were the whole one.
5. **One authoritative record per repo, truth by replay.** If you catch yourself
   writing "for redundancy", stop.
