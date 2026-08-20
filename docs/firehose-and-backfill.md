# Firehose subscription, cursor persistence, and challenge backfill

This document covers `FIREHOSE_URL`/`firehose.*` configuration, how ATChess
avoids replaying a host's entire commit history on every restart
(atchess-1c9.46), and exactly what the login-time challenge backfill can and
cannot find. It exists because challenge delivery
(`app.atchess.challenge`) depends on all of this working together: a
challenge record only ever lives in its **challenger's own repo** (AT
Protocol never permits writing into a repo that isn't your own), so the
challenged player's own protocol-service instance has to go find it.

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
(consuming the relevant firehose ahead of time and maintaining a
`challenged DID -> challenge records` index) -- out of scope for this
mechanism.
