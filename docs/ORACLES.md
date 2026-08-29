# Oracles

This document exists because ATChess is a candidate workload for an autonomous
agent pipeline — one that implements an issue, reviews its own work, and merges
without a human reading the diff.

That changes what tests are for. When a human reviews every merge, the test
suite is a helper: it catches what the reviewer might miss, and the reviewer
catches what the suite misses. When nothing human reads the diff, the suite is
the only thing standing between a plausible-looking change and `main`. The
suite stops being a helper and becomes the *definition of correct*.

So the question this document answers is not "what do we test?" It is:

> **What can this project check that is true independently of anything an agent
> in this repository believes?**

That is what an oracle is here: a source of truth the implementation cannot
talk its way around, and that was not authored by the same process that writes
the code it judges.

## The standing rule

**An oracle's expected values are inputs, not outputs.**

Every constant in this document's gates came from outside this codebase. None
of them was produced by running this code and recording what happened. If a
gate fails, the possibilities are, in order of likelihood:

1. The change under test is wrong.
2. A dependency changed underneath us.
3. The oracle's corpus was transcribed incorrectly *when it was written*.

"The expected value should be updated to match what the code now does" is not
on that list. A change that edits an oracle's expected values to make a test
pass has removed the gate rather than passed it, and should be treated as a
defect regardless of whether CI is green afterwards.

## Naming a gate's escape hatch

**The variable's name carries the meaning. The value is a boolean.**

`A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES=1`, not
`ATCHESS_SUPERVISED_OK=<long magic phrase>`.

An earlier version of both gates used a tedious phrase as the *value*, on the
theory that something awkward to type is harder to set by accident. That was
theatre. Each gate prints its own escape phrase verbatim in the refusal
message, so anything able to read the error is able to satisfy it — the
friction stopped nobody and obscured the meaning for everybody.

A name that states the claim is strictly better, because the protection was
never friction. It is **legibility**: an agent that sets
`A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES=1` has written a plain falsehood
into a diff, where a reviewer, a grep, or an audit will find it. An agent that
sets `ATCHESS_SUPERVISED_OK=1` has written an opaque token that reads like
configuration.

Both gates accept `1`, `true`, or `yes`, case-insensitively. Everything else,
including `0` and `false`, is not an assertion.

## What each oracle covers

### 1. Perft — `internal/chess/perft_test.go`

Node counts for the six canonical Chess Programming Wiki positions. A perft
count is a single integer, independently verified by every serious chess engine
for thirty years, and it changes the instant move generation, castling rights,
en passant, or promotion is wrong. There is no way to make a broken engine
produce the right number, and no way to argue with the result.

Move generation itself lives in `github.com/notnil/chess`, so on its face this
tests a dependency. That is deliberate: a dependency bump is exactly the kind
of change an autonomous pipeline makes confidently and cannot evaluate, and
`go get -u` silently altering castling would otherwise pass every other gate
here.

The half that tests *our* code is `TestEngineAcceptsExactlyTheLegalMoves`,
which drives every legal move through `Engine.MakeMove` using the same string
coordinates the HTTP API accepts, and compares the resulting FEN against ground
truth. It covers `parseSquare`, `ParsePromotion`, and the from/to/promo
matching loop in `MakeMove`.

*Measured, 2026-08-29:* deleting the `vm.Promo() == promotion` term from
`MakeMove`'s move-matching loop — so the wrapper silently applies whichever
promotion the generator happened to list first — leaves the entire pre-existing
`internal/chess` suite green. `TestEngineAcceptsExactlyTheLegalMoves` fails on
it. That bug would hand a player a knight when they asked for a queen, in a
record replicated to both PDSes.

`TestEngineRejectsIllegalMoves` sweeps all 4096 from/to pairs in a dense
position and requires refusal for every one that is not legal. An accepted
illegal move is a forgery vector: it is written to the player's repository and
replayed by every peer.

### 2. Game outcomes — `internal/chess/outcomes_test.go`

Perft proves the legal move *set* is right and says nothing about what this
package reports when a game *ends*. That reporting is ours:

- `MoveResult.Check` / `Checkmate` are inferred by sniffing the last byte of
  the SAN string for `+` / `#` — a real inference over a formatting detail of
  an external encoder, which nothing else pins.
- `GetStatus` maps `chess.Outcome` onto this project's `GameStatus`.
- `MoveResult.Result` is read back out of a PDS record by peers.

The corpus covers checkmate, check-that-is-not-mate, a quiet move, and
stalemate — the case a naive "no legal moves means checkmate" shortcut gets
backwards. Structural invariants are asserted on every case: a checkmate is
always also a check and a game over, and never also a draw.

`TestFENRoundTripsThroughEngine` and `TestPGNGrowsMonotonicallyAndReplays`
cover the serialization boundary. A FEN that mutates in transit desynchronizes
the two players' copies of one game; a PGN that does not replay to the live
position means the durable record is not the game that was played.

### 3. Blast radius — `scripts/check-public-plc-blast-radius.sh`

This one is not a correctness oracle. It is the answer to a different question:
*what can this repository do that cannot be undone by reverting a commit?*

There is exactly one such thing. `make test-federation-up` runs the dual-PDS
harness in local mode, pointed at the real, public `https://plc.directory`.
Bringing it up mints DID documents there, and publication to the PLC directory
is append-only and permanent — `make test-federation-down` destroys the local
volumes, and the DIDs it published stay in a public global registry forever.

`docker-compose.dual-pds.yml` already warns about this in prose: *"do not loop
this repeatedly, it permanently publishes DID documents."* That is adequate
protection against a human, who reads it once and remembers. It is no
protection against an autonomous pipeline, which will run the documented way to
bring the stack up, will not weigh a comment against a failing test, and will
retry. A retry loop here is not a broken build — it is unbounded, irreversible
pollution of infrastructure this project does not own.

The gate refuses local mode when the caller looks unattended: `CI`,
`ATCHESS_AUTOMATION`, `ATCHESS_AGENT`, `GITHUB_ACTIONS`, or — the check that
catches the case nobody remembered to label — no controlling terminal on stdin.
It runs as the first line of the `test-federation-up` recipe, ahead of the mode
check and any `mkdir`, so a refusal touches nothing at all.

`make test-federation-up-ci` is hermetic (a local `did:plc` service, nothing
public), uses the same harness and the same account script, and is deliberately
never gated. **For anything automated, that is the target.**

The escape hatch is `A_HUMAN_IS_PUBLISHING_PERMANENT_PUBLIC_DIDS=1`. Setting
it from a Makefile, a CI config, an agent harness, or a shell profile defeats
the gate's only purpose.

### 4. Supervised paths — `scripts/check-supervised-paths.sh`

Like the blast-radius gate, this one is not a correctness oracle. It answers a
third question: *where does this project have no oracle at all, and what
follows from that?*

`web/static/` is the answer. `index.html` and `spectator.html` carry the board
rendering, move input, challenge flow, and live-update handling inline, and
there is no rendering test, no DOM assertion, no interaction test, and no
screenshot diff over any of it. `internal/web/static_derivation_incomplete_guard_test.go`
greps those files for one string; that is a tripwire for a single known
regression, not coverage. A change that renders a blank board, drops the move
handler, or breaks the challenge flow passes the entire suite.

**Decision `atchess-b2d.2`: these paths are supervised only.** Agent work on
them is allowed. Agent work that *merges itself* is not. The distinction
matters — "out of bounds" would forfeit the largest single block of real
product work in the backlog, and the problem was never that agents write bad
frontend code. The problem is that nothing here can tell whether they did.

The path list lives in `.supervised-paths` as data, so adding a path is a
one-line change. The gate refuses when a change touches a listed path *and*
the caller looks unattended — the same detection the blast-radius gate uses. A
human at a terminal passes with a notice naming the files. The escape hatch is `A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES=1`, and it
asserts that a person looked at the **rendered result** — not that they
approved a plan, and not that the tests were green. On this diff, green tests
carry no information.

It fails closed: a missing or empty `.supervised-paths` exits 2 rather than
passing. An absent policy file is a defect, not a permission.

The honest way out of this gate is to build the oracle — a headless-browser
harness asserting the board renders from a given FEN, that a submitted move
produces the expected API call, and that the board flips — and then remove the
path from the list. That is tracked as option B on `atchess-b2d.2`; making the
gate a required CI check keyed on human PR approval is `atchess-b2d.9`.

## Gates that have been shown to fail

A gate nobody has watched fail is a guess about a gate. Each of these was
fault-injected and observed to fail for the intended reason, not merely to be
green:

| Gate | Injected fault | Result |
|---|---|---|
| `TestEngineAcceptsExactlyTheLegalMoves` | dropped promotion matching in `MakeMove` | fails; pre-existing suite stays green |
| blast-radius, unattended | piped stdin, no tty | refuses, exit 1, nothing created |
| blast-radius, CI | `CI=true` | refuses, names CI as the reason |
| blast-radius, escape hatch | correct phrase set | proceeds, warns on stderr |
| blast-radius, near-miss | `A_HUMAN_IS_PUBLISHING_PERMANENT_PUBLIC_DIDS=0` | still refuses |
| supervised paths, real edit | edited `web/static/index.html` in the worktree | refuses, exit 1, names the file |
| supervised paths, clean | changed only `internal/chess/engine.go` | passes |
| supervised paths, escape hatch | correct phrase set | proceeds, warns on stderr |
| supervised paths, near-miss | `A_HUMAN_HAS_REVIEWED_THE_FRONTEND_CHANGES=false` | still refuses |
| both gates, retired names | the old magic-phrase variables | no longer accepted |
| supervised paths, fail-closed | `.supervised-paths` removed | exits 2 rather than passing |

When you add an oracle, add its row. Breaking the code on purpose and watching
the gate catch it is the only evidence that the gate is real.

## Known gaps

These are named rather than quietly left out. None is closed.

- **Lexicon conformance is partly vacuous.** `atchess-1c9.89` records that a
  vanished secondary write still passes the lexicon suite. A record-shape gate
  that cannot fail is not protecting the wire format.
- **No oracle covers time controls.** `internal/chess/timecontrol.go` has unit
  tests, but `atchess-1c9.90` asks whether time controls are a supported
  feature or dead code. An unanswered product question is not a gate.
- **The web frontend still has no oracle.** This is now a *managed* gap rather
  than an unmanaged one: `.supervised-paths` plus
  `scripts/check-supervised-paths.sh` stop unattended merges there (see oracle
  4 above). The gap itself is unclosed — nothing yet checks that the board
  renders — and it stays open until a frontend harness exists.
- **`make lint` does not run.** `atchess-1c9.20`: golangci-lint is not
  installed, so the lint gate currently passes by being absent. Several
  committed files are also not `gofmt`-clean, which means `make fmt` would
  produce diff noise attributable to no one.
