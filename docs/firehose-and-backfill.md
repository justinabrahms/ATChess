# Firehose subscription, cursor persistence, and challenge backfill

This document covers `FIREHOSE_URL`/`firehose.*` configuration, how ATChess
avoids replaying a host's entire commit history on every restart
(atchess-1c9.46), exactly what the login-time challenge backfill can and
cannot find, and (atchess-1c9.50) the durable, SQLite-backed challenge index
("AppView") both feed into and `GET /api/challenge-notifications` serves
from. It exists because challenge delivery (`app.atchess.challenge`)
depends on all of this working together: a challenge record only ever lives
in its **challenger's own repo** (AT Protocol never permits writing into a
repo that isn't your own), so the challenged player's own protocol-service
instance has to go find it.

## `FIREHOSE_ENABLED` / `FIREHOSE_URL`

```yaml
firehose:
  enabled: false
  url: wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos
  state_dir: ./data/firehose
```

Environment variables: `FIREHOSE_ENABLED`, `FIREHOSE_URL`,
`FIREHOSE_STATE_DIR` (see `internal/config.FirehoseConfig`).

- **`FIREHOSE_ENABLED`**: challenge delivery does nothing unless this is
  `true`. Off by default.
- **`FIREHOSE_URL`**: one or more `com.atproto.sync.subscribeRepos`
  websocket endpoints, **comma-separated**.
  - **Real-network use**: once this deployment sits behind a genuine,
    network-wide relay, point this at it:
    `wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos`. `bsky.network`
    is the actual public AT Protocol relay. It is **not** the same thing as
    `bsky.social`, which is a single (very large) PDS, not a relay --
    subscribing directly to `bsky.social`'s own `subscribeRepos` only sees
    commits from repos hosted on `bsky.social` itself, not the whole
    network.
  - **Single-PDS deployments** (e.g. this service's own configured account
    lives on one specific PDS and you only care about challenges from
    people on that same PDS, or from a short explicit list of PDSes): point
    this directly at that PDS's own `subscribeRepos` endpoint instead.
  - **The comma-separated multi-URL form** exists for test topologies with
    more than one PDS and no relay in front of them -- this project's local
    dual-PDS harness (`test/harness/services.go`) is the only place that
    actually needs it, since it stands up two independent PDS containers
    with no relay between them and has to watch both directly. It is not
    expected to be used against the public network.
- **`FIREHOSE_STATE_DIR`**: directory used to persist cursor state (below).
  Created automatically if missing. Must be writable by the process. Not
  committed to source control (`data/` is gitignored).

## Cursor persistence and the bounded initial backfill (atchess-1c9.46)

**The defect this fixes**: an earlier version of this service passed
`WithCursor(0)` to every firehose client on every process start, with no
persistence across restarts. Cursor `0` means "replay this host's *entire*
retained commit log from the very beginning." Against a small test-harness
PDS with a handful of records that's invisible; against a production-scale
host such as `bsky.social` (or a real relay watching it), it means every
single restart re-requests that host's whole history. That is not
"backfill on login" -- it is "replay everything, every boot."

**The fix**:

1. **Cursor persistence.** Each watched host's last-processed sequence
   number is written to `<state_dir>/cursors.json` periodically (every 5s;
   see `firehoseCursorPersistInterval` in `cmd/protocol/main.go`) and once
   more during graceful shutdown. On the next start, if a cursor is stored
   for a host, the subscription resumes from exactly that sequence instead
   of starting over.
   - **First run / no stored cursor**: the subscription starts at the
     *live tip* (no cursor at all), never at `0`. History is instead
     covered by the login-time repo-read backfill below, which is targeted
     at a specific user rather than a full replay for everyone.
   - **Corrupt cursor file**: logged as a warning, the bad file is
     preserved alongside the working one (`cursors.json.corrupt`) for
     inspection, and the store starts empty -- equivalent to "no prior
     cursor," not a crash.
   - **Host rejects the stored cursor** (`FutureCursor` error frame, e.g.
     a stale cursor after a host's sequence counter was reset): the
     client resets to "no cursor" and reconnects at the live tip; that
     reset is itself persisted on the next flush, so the next restart
     doesn't immediately hit the same rejection again.
   - **Cursor older than the host's retention window** (`#info
     OutdatedCursor`): the host doesn't error, it just starts streaming
     from the earliest point it still has. Logged loudly (`Warn`) because
     it means some events between the old cursor and that floor were
     unrecoverably missed by the *subscription* -- see the backfill below
     for the intended mitigation.
2. **Login-time repo-read backfill** (`internal/backfill`) covers the
   historical gap instead of a full-log replay -- see the next section.

None of this requires a database; state lives in one small JSON file under
`FIREHOSE_STATE_DIR`.

## Login-time challenge backfill: what it can and cannot find

On every successful login (password or OAuth), `internal/backfill` runs a
**repo-read backfill** for the newly authenticated user, bounded by an
overall 8s timeout (`loginBackfillTimeout`, `internal/web/service.go`),
before the login response is returned. It does **not** use the firehose at
all -- it queries `com.atproto.sync.listRepos` and
`com.atproto.repo.listRecords` directly.

**The 8s budget is a ceiling across *all* configured hosts, not a
per-host allowance** -- and every host is attempted **serially**. Each host
is additionally bounded by its own `defaultPerHostTimeout`
(`internal/backfill`, currently 3s): without that per-host bound, a single
wedged/blackholed host (one that accepts the TCP connection but never
responds -- not merely a fast connection-refused failure, which was already
handled) listed first would consume the *entire* 8s budget by itself, and
every host listed after it would never be attempted at all
(`ReposScanned: 0`, indistinguishable from that host also being broken).
With the per-host bound, each host gets its own capped attempt carved out
of the shared 8s ceiling, so a wedged host can only ever cost up to
`defaultPerHostTimeout`, leaving the remaining budget for the hosts after
it. This is a deliberate **bounded-and-blocking** choice, not
bounded-and-async: the login handler still runs the backfill synchronously,
before returning its response, so a challenge this backfill can find is
guaranteed visible via `GET /api/challenge-notifications` the moment login
succeeds -- no polling race between "login succeeded" and "backfill
finished" for callers to work around. Moving this off the response path
(fire-and-forget) would remove that guarantee in exchange for a login that
never waits on the backfill at all; that tradeoff was not taken here
because the immediacy guarantee was itself a deliberate earlier design
decision (this backfill's whole reason to run synchronously) and per-host
bounding already fixes the starvation defect without giving it up.

None of this changes the *scale* problem described below: against a
production-scale host such as `bsky.social`, even one host's full
`defaultMaxReposPerHost`-capped scan can itself approach the per-host
budget (see the numbers in the next section) -- the per-host timeout
protects *other configured hosts* from a wedged one, it does not make the
capped scan of a huge host itself fast.

**Scope, stated plainly**: a challenge can only be found by reading the
repo that contains it, and there is no query anywhere in the AT Protocol
network that answers "which repos, across the whole network, have written
a challenge addressed to me" directly -- that would require a
purpose-built index (e.g. a custom AppView consuming the network-wide
firehose ahead of time), which this backfill does **not** build.

What it actually does is bounded to the **same closed list of PDS hosts**
already given to the firehose subscriptions (`FIREHOSE_URL`). For each
host, it lists that host's hosted repos and checks each one's
`app.atchess.challenge` collection for a record addressed to the logging-in
user.

- **Small / self-hosted PDS, or this project's local test harness** (a
  handful of accounts per PDS): this is exhaustive. Every repo on every
  configured host is actually checked, so this finds every challenge
  addressed to the user that this deployment could ever discover by any
  means, immediately on login -- no dependency on the firehose having been
  running the whole time.
- **Production-scale host (e.g. `bsky.social`, tens of millions of hosted
  repos)**: `com.atproto.sync.listRepos` has no server-side filter for
  "repos that have challenged me." Enumerating and checking every repo on
  a host that size is not tractable synchronously on login. This backfill
  refuses to attempt an unbounded walk -- past `internal/backfill`'s
  `defaultMaxReposPerHost` cap, the remainder of that host's repos are
  **not** scanned, and the result is marked `Capped` (logged as a
  warning). Even the capped subset is such a small fraction of a
  production-scale host's population that it is not a meaningful sample.
  **This backfill does not solve "discover a challenge from an arbitrary
  challenger anywhere on the public network"** against a host at that
  scale, and does not claim to.

If you need that general case, it requires a separate indexing service
consuming the relevant firehose ahead of time and maintaining a `challenged
DID -> challenge records` index. atchess-1c9.50, covered next, builds
exactly that -- read it before assuming it solves the production-scale
problem above, because it does not.

## The challenge index (AppView): what's discoverable now, and what still isn't (atchess-1c9.50)

Everything above this section (the firehose subscription and the login
backfill) are the two *ingestion* paths. Historically, this project had no
durable *query* side: `internal/challenge.Store` was an in-memory cache, so
"who has challenged me" was only ever answerable for however long this
process happened to have been running continuously, and a decline could be
un-done by a restart (atchess-1c9.47). atchess-1c9.50 replaced that cache
with a durable, SQLite-backed index (`internal/challenge.Store`, one file
on the same box -- see `challenge.db_path` / `CHALLENGE_DB_PATH`, following
the exact same "no separate database server" pattern as
`FIREHOSE_STATE_DIR` above; the driver is `modernc.org/sqlite`, a pure-Go,
cgo-free implementation, specifically because this project builds with
`CGO_ENABLED=0`). This is what every real AT Protocol app does to answer
"who wrote a record addressed to me": bsky.app itself is an AppView over
the same protocol.

Both ingestion paths above write into this same index, idempotently: the
firehose subscription's `EventProcessor` on every `create`/`update` (and,
new in atchess-1c9.50, `delete` -- a challenge record removed from the
challenger's repo is tombstoned rather than left to linger as open), and
the login backfill on every repo it reads. Replaying the same event, or
re-running the backfill, can never duplicate a row or resurrect a
previously-declined or previously-removed challenge (`Store.Add`'s `ON
CONFLICT(uri) DO NOTHING` combined with a `status` column that a decline or
a delete sets and a later replay never overwrites) -- this is the
atchess-1c9.47 regression, closed for good now that the data lives in a
file survivable across a restart rather than in memory.

**What this makes newly discoverable**: a challenge issued while THIS
instance was down -- including for *longer than a relay's ~72h retention
window*, the exact gap atchess-1c9.50 exists to close -- is discoverable
once either (a) this instance's firehose subscription resumes (from its
persisted cursor, or via the login backfill) and observes it, or (b) a
login backfill run actually reads the challenger's repo and finds it. Once
either of those has happened even once, the challenge is in the index
permanently (until declined, removed, or pruned as expired), queryable in
constant time by `GET /api/challenge-notifications`, independent of
whether this process has restarted since, or how long ago the ingestion
happened.

**What remains genuinely undiscoverable, stated plainly -- do not read
atchess-1c9.50 as having solved general network-wide discovery, because it
has not**:

1. **An AppView that was itself never running to see the event, and whose
   backfill never covered it either, still has nothing.** If this specific
   deployment was down (or not yet deployed) for the *entire* window during
   which a challenge existed on the firehose -- longer than the relay's
   retention floor -- **and** the challenger's repo falls outside backfill's
   bounded, closed list of configured hosts (`FIREHOSE_URL`) by the time
   anyone next logs in, there is no remaining path to that challenge. This
   is not a defect specific to this implementation; it is the fundamental
   limitation of *every* AT Protocol AppView: you can only index what you
   actually observed live, or what you explicitly went and fetched. An
   AppView cannot retroactively see an event it was never subscribed for
   and never backfilled.
2. **Backfill's scale limits are unchanged by the index existing.** The
   index is only ever as complete as what its two ingestion paths actually
   fed it. Backfill is still bounded to the same closed, explicitly
   configured `FIREHOSE_URL` host list, and still capped per host at
   `defaultMaxReposPerHost` (2000) repos -- see the section above. A
   challenger on a PDS this deployment has never configured, or buried past
   the cap on a host that size, is not found by backfill, and therefore
   never enters the index via that path either. Adding the durable index
   did not add a way to discover challengers outside this deployment's
   configured, bounded view of the network.
3. **This is not a general-purpose, network-wide `app.atchess.challenge`
   AppView.** It indexes exactly what this deployment's own two ingestion
   paths (a specific, operator-configured list of watched PDS/relay
   endpoints, plus a bounded backfill against that same list) have actually
   seen -- nothing more. A stranger on a PDS never configured into
   `FIREHOSE_URL`, challenging a user of this deployment, is invisible to
   both mechanisms and therefore to the index, regardless of how long the
   challenge has existed or how long this process has been running.
4. **What a user should actually do about an undiscovered challenge**: ask
   the challenger to re-send it -- a fresh `create` commit is immediately
   visible to a live firehose subscription that is currently up and
   watching the challenger's host. If you operate this deployment, the
   structural fix is adding the challenger's PDS host to `FIREHOSE_URL` (or
   sitting behind a genuine network-wide relay, `wss://bsky.network/...`,
   rather than a single PDS -- see above), so both the firehose subscription
   and the login backfill start covering it going forward. There is no
   retroactive fix for a challenge from a host this deployment was never
   watching and never configured to backfill.

## Jetstream transport (atchess-1c9.49): connecting to a public instance, and the live-verified numbers

`internal/firehose` originally spoke only real AT Protocol
`com.atproto.sync.subscribeRepos` CBOR frames. Pointed at the actual
network-wide relay (`wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos`,
see above), that means decoding **every** commit on the entire Bluesky
network -- every post, like, follow, etc. -- just to discard essentially all
of it client-side. On the target deployment box (a DigitalOcean droplet,
~2GB RAM / 1 vCPU), that is not viable: it would saturate the single core
continuously decoding CBOR for events this service will never use.

[Jetstream](https://github.com/bluesky-social/jetstream) is a public
service (operated by Bluesky; instances include `jetstream1.us-east`,
`jetstream2.us-east`, and `-west` equivalents, all under
`*.bsky.network`) that consumes the relay itself and re-emits a filtered,
JSON-encoded subset over its own `/subscribe` websocket endpoint, filtered
server-side by the `wantedCollections` query parameter (repeated once per
NSID). **This bead is about connecting to a public Jetstream instance as a
client** -- it is not self-hosting one. atchess-1c9.11 evaluated (and
correctly rejected as infeasible) self-hosting a Jetstream instance, which
needs a crawlable relay upstream that this project's bare two-PDS test
harness cannot provide; that conclusion does not apply here.

`internal/firehose.Client` now supports both transports
(`firehose.TransportSubscribeRepos`, the original default, and
`firehose.TransportJetstream`), selected automatically from the configured
URL's shape (`DetectTransport`: a path ending in `/subscribe` is
Jetstream) or forced via `FIREHOSE_TRANSPORT` /
`FirehoseConfig.Transport` (`"jetstream"`, or `"subscribeRepos"`/`"cbor"`
to force the original transport regardless of URL). The Jetstream
`wantedCollections` sent are the closed list in
`firehose.WantedCollections` -- every `app.atchess.*` NSID this deployment
actually writes (derived from `internal/atproto/client.go`), so a new
lexicon can't be silently left unfiltered.

**Cursor semantics differ by transport and are not interchangeable.**
`subscribeRepos` cursors are small, host-local sequence numbers;
Jetstream cursors are unix **microseconds** (`time_us`). `CursorStore`
tags every persisted cursor with the transport it was recorded under and
refuses to hand a `subscribeRepos` cursor back to a `Jetstream` connection
(or vice versa) -- see `internal/firehose/cursorstore.go` and its tests.

**zstd compression**: Jetstream optionally supports zstd-compressed
frames. This bead does **not** implement that -- the client only speaks
plain JSON frames. Given the observed near-zero `app.atchess.*` volume
below, the bandwidth zstd would save is not worth the added complexity
for this deployment; revisit if that changes.

### Live-verified numbers (2026-08-20)

Measured with a throwaway probe (not committed) connected to
`wss://jetstream1.us-east.bsky.network/subscribe`, using the real,
public instance -- not a mock.

**Chess run** -- `wantedCollections` set to all 8 NSIDs in
`firehose.WantedCollections` (`app.atchess.challenge`,
`challengeResponse`, `drawOffer`, `drawResponse`, `game`, `move`,
`resignation`, `timeViolation`), observed for 90.0s:

- **0** `app.atchess.*` commit events received (expected -- this is a
  brand-new, low-traffic app; this is the entire point of the filter).
- **45** total websocket messages received in 90.0s (~0.5 msg/s) -- **all
  45 were `kind:"identity"`/`kind:"account"` events, zero were
  `kind:"commit"`** (confirmed by inspecting raw frames). This is an
  important caveat to the "near-idle websocket" claim: **Jetstream's
  `wantedCollections` filter does not apply to `identity`/`account`
  events at all** -- those are only filterable via the separate
  `wantedDids` parameter, which this deployment cannot practically set in
  advance (an opponent's DID isn't known until they challenge). So even
  with a tight `wantedCollections` filter, a Jetstream connection still
  receives a network-wide trickle of identity/account churn (observed
  here at roughly one message every ~2s). This is real but small relative
  to raw commit-firehose volume (see the control below), and does not
  change the core conclusion: server-side `wantedCollections` filtering
  still eliminates the overwhelming majority of traffic a subscribeRepos
  connection to a relay would otherwise have to decode and discard
  client-side.

**Control run** -- same instance, same client, `wantedCollections` set to
`app.bsky.feed.post` (a high-traffic collection) instead, observed for
30.0s, to prove the near-zero chess number above reflects the filter
working rather than a broken/silently-failed client:

- **1253** total messages in 30.0s, of which **1221** were
  `app.bsky.feed.post` commit events -- **~41 events/sec**.
- Confirms the same client, dialed the same way, against the same
  instance, receives substantial traffic for a collection that actually
  has it. The chess run's near-zero count is therefore attributable to
  real (lack of) traffic on `app.atchess.*`, not a broken connection,
  wrong query params, or a client that silently stopped reading.

These are one-off, single-sample measurements from a development sandbox,
not a long-running/statistically rigorous benchmark -- they are recorded
here specifically to replace an *estimate* ("near-idle websocket") with an
*observation*, per atchess-1c9.49's done-criteria. Re-verify against
production traffic if these numbers become load-bearing for a capacity
decision.
