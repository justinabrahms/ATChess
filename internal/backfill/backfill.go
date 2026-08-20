// Package backfill implements the login-time repo-read backfill for
// challenge delivery (atchess-1c9.46): on authentication, it discovers
// app.atchess.challenge records addressed to the logging-in user by
// reading repos directly (com.atproto.sync.listRepos +
// com.atproto.repo.listRecords) rather than depending solely on having
// been subscribed to the firehose the whole time.
//
// SCOPE -- READ BEFORE EXTENDING OR RELYING ON THIS FOR A NEW USE CASE.
//
// A challenge record only ever lives in its CHALLENGER's own repo (AT
// Protocol never permits writing into a repo that isn't your own), so
// "find challenges addressed to me" can only ever mean "find repos that
// contain such a record", and there is no query anywhere in the AT
// Protocol network that answers "which repos, across the whole network,
// contain an app.atchess.challenge record with challenged == me" directly
// -- that would require a purpose-built index (e.g. a custom AppView
// consuming the network-wide firehose ahead of time and maintaining
// exactly that reverse index), which this package does NOT build.
//
// What this package actually does is bounded to a CLOSED, EXPLICITLY
// CONFIGURED list of PDS hosts (the same list already passed to
// cmd/protocol/main.go's firehose subscriptions -- see
// config.FirehoseConfig.URL / config.SplitFirehoseURLs): for each host, it
// enumerates that host's hosted repos (com.atproto.sync.listRepos) and
// checks each one's app.atchess.challenge collection
// (com.atproto.repo.listRecords) for a record addressed to the logging-in
// user.
//
//   - For a small/self-hosted PDS, or this project's own local dual-PDS
//     test harness (a handful of accounts per PDS), this is exhaustive:
//     every repo on every configured host is actually checked, so this
//     finds every challenge addressed to the user that this deployment
//     could ever have discovered by any means (including the firehose
//     itself).
//   - Against a production-scale host such as bsky.social (tens of
//     millions of hosted repos), com.atproto.sync.listRepos has no
//     server-side filter for "repos that contain a challenge addressed to
//     X" -- enumerating and checking every repo is not remotely tractable
//     synchronously on login, or arguably at all without an index. This
//     package refuses to attempt an unbounded enumeration (see
//     maxReposPerHost): once a host's repo count exceeds the cap, the
//     remainder of that host's repos are NOT scanned, and the result is
//     marked Capped so the caller can log it. Even the capped subset (a
//     few hundred repos out of tens of millions) is not a meaningful
//     sample -- this package does NOT solve, and does not claim to solve,
//     "discover a challenge from an arbitrary challenger anywhere on the
//     public network" against a host at that scale. See
//     docs/firehose-and-backfill.md.
package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/rs/zerolog"
)

const (
	// defaultMaxReposPerHost bounds how many repos a single host's
	// com.atproto.sync.listRepos enumeration will walk (across pagination)
	// before this package gives up on that host rather than continuing an
	// effectively-unbounded walk. 2000 comfortably covers a small
	// self-hosted PDS or this project's local test harness, while still
	// refusing to attempt to walk a production-scale host's full account
	// population -- see the package doc comment.
	defaultMaxReposPerHost = 2000

	// listReposPageLimit is the page size requested per
	// com.atproto.sync.listRepos call (that lexicon's limit range is
	// 1-1000; 1000 minimizes round trips for hosts within
	// defaultMaxReposPerHost).
	listReposPageLimit = 1000

	// defaultPerHostTimeout bounds how long BackfillChallengesForUser will
	// spend on ANY SINGLE host (its listRepos enumeration plus every
	// per-repo listRecords call) before moving on to the next host,
	// independent of the overall context deadline the caller supplied.
	//
	// Without this, a single wedged/blackholed host (one that accepts the
	// TCP connection but never responds -- distinct from, and NOT covered
	// by, a fast-failing connection-refused host) listed first would
	// consume the ENTIRE caller-supplied budget by itself: every http
	// request in getXRPC is bound to the same parent ctx, so the request
	// only unblocks when that shared budget finally expires, and by then
	// there is nothing left for any other host -- they are never attempted
	// at all (ReposScanned:0 for every host after the wedged one). A
	// per-host timeout makes that impossible: context.WithTimeout's
	// effective deadline is always the EARLIER of the parent's deadline and
	// this value, so no host can ever consume more than
	// defaultPerHostTimeout, and the loop always has budget left in the
	// parent ctx to attempt the next host (as long as the parent's own
	// deadline hasn't separately elapsed). See
	// TestBackfillChallengesForUser_WedgedHost_DoesNotStarveOtherHosts.
	defaultPerHostTimeout = 3 * time.Second

	// listRecordsPageLimit is the page size requested per
	// com.atproto.repo.listRecords call for a single repo's
	// app.atchess.challenge collection. A repo accumulating more than this
	// many OPEN challenges addressed to anyone is not a scenario this
	// bounded backfill is trying to solve; one page is enough for the
	// deployments this package targets (see the package doc comment).
	listRecordsPageLimit = 100

	// challengeCollection is the NSID of the challenge record type,
	// duplicated here (rather than imported) because internal/firehose,
	// where isChessRecord/EventTypeChallenge live, imports internal/web,
	// and internal/web must be able to import this package -- importing
	// internal/firehose from here would be a cycle. See the SplitFirehoseURLs
	// doc comment in internal/config for the same constraint applied to
	// the PDS-URL-splitting helper.
	challengeCollection = "app.atchess.challenge"
)

// HostResult records the outcome of backfilling one PDS host.
type HostResult struct {
	// Host is the HTTP(S) base URL of the PDS (derived from the
	// subscribeRepos websocket URL passed in).
	Host string
	// ReposScanned is how many repos on Host actually had their
	// app.atchess.challenge collection checked.
	ReposScanned int
	// ChallengesFound is how many NEW (not already present in the shared
	// challenge.Store) challenges addressed to the target user were
	// discovered on this host.
	ChallengesFound int
	// Capped is true if Host had more repos than defaultMaxReposPerHost
	// and enumeration was stopped early -- see the package doc comment.
	// ReposScanned in that case is the (partial) number actually checked,
	// not the host's true repo count.
	Capped bool
	// Err is set if listing this host's repos failed outright (e.g.
	// network error, non-2xx response), OR if the per-host context expired
	// PARTWAY through this host's per-repo loop after listRepos already
	// succeeded (ctx.Err(), typically context.DeadlineExceeded) -- so a
	// single log line (ReposScanned:N, Capped, Err) tells an operator
	// whether a host was truncated by its timeout or finished cleanly,
	// rather than requiring the separate Warn line to carry that
	// distinction alone. A per-repo listRecords failure does NOT set Err
	// here -- it is logged and that one repo is skipped, since one
	// unreachable/misbehaving repo should not abort the rest of the
	// host's backfill.
	Err error
}

// Result is the outcome of one BackfillChallengesForUser call, one
// HostResult per PDS host attempted, in the order given.
type Result struct {
	Hosts []HostResult
}

// TotalFound sums ChallengesFound across all hosts.
func (r Result) TotalFound() int {
	total := 0
	for _, h := range r.Hosts {
		total += h.ChallengesFound
	}
	return total
}

// Backfiller performs login-time repo-read challenge backfills. Safe for
// concurrent use (holds no mutable state beyond its dependencies, which are
// themselves safe for concurrent use).
type Backfiller struct {
	httpClient      *http.Client
	store           *challenge.Store
	logger          zerolog.Logger
	maxReposPerHost int
	// perHostTimeout bounds each individual host attempt -- see
	// defaultMaxReposPerHost's neighbor, defaultPerHostTimeout, for why.
	perHostTimeout time.Duration
}

// New creates a Backfiller that indexes discovered challenges into store
// (the same shared cache internal/firehose.EventProcessor populates from
// the live subscription -- see that package's doc comment), logging via
// logger.
func New(store *challenge.Store, logger zerolog.Logger) *Backfiller {
	return &Backfiller{
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		store:           store,
		logger:          logger,
		maxReposPerHost: defaultMaxReposPerHost,
		perHostTimeout:  defaultPerHostTimeout,
	}
}

// BackfillChallengesForUser enumerates repos on each host named in
// pdsSubscribeReposURLs (com.atproto.sync.subscribeRepos websocket URLs, in
// the exact form used for FIREHOSE_URL / config.FirehoseConfig.URL -- see
// config.SplitFirehoseURLs) and indexes any app.atchess.challenge record
// addressed to userDID into the shared challenge.Store. See the package
// doc comment for exactly what this can and cannot find.
//
// Best-effort and non-fatal by design: a failure against one host (or one
// repo within a host) is logged and does not abort the rest of the
// backfill or propagate as an error to the caller -- a broken/unreachable
// PDS must not block a user's login. Bounded by ctx; callers should attach
// a deadline (login must not hang indefinitely on a slow/unreachable PDS).
//
// Each host additionally gets its OWN bounded attempt (b.perHostTimeout,
// default defaultPerHostTimeout) carved out of ctx: a single wedged host
// cannot consume the entire caller-supplied budget and starve every other
// configured host of ever being attempted at all -- see
// defaultPerHostTimeout's doc comment for the full rationale and why a
// PER-HOST bound was chosen over moving this off the login response path
// entirely.
func (b *Backfiller) BackfillChallengesForUser(ctx context.Context, userDID string, pdsSubscribeReposURLs []string) Result {
	var result Result

	perHostTimeout := b.perHostTimeout
	if perHostTimeout <= 0 {
		perHostTimeout = defaultPerHostTimeout
	}

	for _, wsURL := range pdsSubscribeReposURLs {
		if ctx.Err() != nil {
			b.logger.Warn().Str("url", wsURL).Msg("login backfill: context deadline exceeded before this host was attempted; skipping remaining hosts")
			result.Hosts = append(result.Hosts, HostResult{Host: wsURL, Err: ctx.Err()})
			continue
		}

		base, err := httpBaseFromSubscribeReposURL(wsURL)
		if err != nil {
			b.logger.Warn().Err(err).Str("url", wsURL).Msg("login backfill: could not derive an HTTP base URL for this host; skipping")
			result.Hosts = append(result.Hosts, HostResult{Host: wsURL, Err: err})
			continue
		}

		// hostCtx's effective deadline is the EARLIER of ctx's own
		// deadline and perHostTimeout from now (context.WithTimeout always
		// intersects with the parent), so this can only ever tighten the
		// budget for this one host, never extend it past what the caller
		// (ctx) allows overall.
		hostCtx, hostCancel := context.WithTimeout(ctx, perHostTimeout)
		hr := b.backfillHost(hostCtx, base, userDID)
		hostCancel()

		result.Hosts = append(result.Hosts, hr)
	}

	return result
}

func (b *Backfiller) backfillHost(ctx context.Context, base, userDID string) HostResult {
	hr := HostResult{Host: base}

	dids, capped, err := b.listRepoDIDs(ctx, base)
	hr.Capped = capped
	if err != nil {
		hr.Err = err
		b.logger.Warn().Err(err).Str("host", base).Msg("login backfill: failed to list repos on host")
		return hr
	}
	if capped {
		b.logger.Warn().Str("host", base).Int("cap", b.maxReposPerHost).Msg("login backfill: host has more repos than this bounded backfill will scan; only a partial, non-representative subset was checked (see internal/backfill's package doc comment) -- this host is NOT fully covered by this backfill")
	}

	for _, repoDID := range dids {
		if ctx.Err() != nil {
			// Record on hr.Err too (atchess-1c9.46 review fix), not just
			// the Warn line below: without this, the single structured
			// summary line logged by internal/web's
			// backfillChallengesOnLogin (reposScanned/capped/err) shows
			// err:"" for a host truncated mid-scan, indistinguishable
			// from one that simply finished cleanly with fewer repos.
			hr.Err = ctx.Err()
			b.logger.Warn().Str("host", base).Msg("login backfill: context deadline exceeded mid-host; remaining repos on this host were not checked")
			break
		}

		found, err := b.listChallengesForRepo(ctx, base, repoDID, userDID)
		if err != nil {
			b.logger.Warn().Err(err).Str("host", base).Str("repo", repoDID).Msg("login backfill: failed to list challenge records for repo; skipping this repo")
			continue
		}
		hr.ReposScanned++

		for _, pc := range found {
			if b.store.Add(pc) {
				hr.ChallengesFound++
			}
		}
	}

	return hr
}

// httpBaseFromSubscribeReposURL converts a com.atproto.sync.subscribeRepos
// websocket URL (wss://host/xrpc/com.atproto.sync.subscribeRepos or ws://...)
// into that same PDS's HTTP(S) base URL, for issuing ordinary XRPC GET
// requests against it. Inverse of
// test/harness/services.go's pdsSubscribeReposURL.
func httpBaseFromSubscribeReposURL(wsURL string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parsing %q: %w", wsURL, err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "https", "http":
		// already an HTTP(S) URL; tolerate it
	default:
		return "", fmt.Errorf("unrecognized scheme %q in firehose URL %q (expected ws/wss/http/https)", u.Scheme, wsURL)
	}
	u.Path = ""
	u.RawQuery = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// listRepoDIDs enumerates every repo hosted on base via
// com.atproto.sync.listRepos, paginating until either the host reports no
// further cursor or b.maxReposPerHost is reached (in which case capped is
// true and dids contains only the partial subset actually fetched -- see
// the package doc comment).
func (b *Backfiller) listRepoDIDs(ctx context.Context, base string) (dids []string, capped bool, err error) {
	cursor := ""
	for {
		params := url.Values{"limit": {strconv.Itoa(listReposPageLimit)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		var page struct {
			Cursor string `json:"cursor"`
			Repos  []struct {
				DID string `json:"did"`
			} `json:"repos"`
		}
		if err := b.getXRPC(ctx, base, "com.atproto.sync.listRepos", params, &page); err != nil {
			return dids, capped, err
		}

		for _, r := range page.Repos {
			if r.DID == "" {
				continue
			}
			dids = append(dids, r.DID)
			if len(dids) >= b.maxReposPerHost {
				return dids, true, nil
			}
		}

		if page.Cursor == "" || len(page.Repos) == 0 {
			return dids, false, nil
		}
		cursor = page.Cursor
	}
}

// listChallengesForRepo fetches repoDID's app.atchess.challenge collection
// from base and returns the (decoded) records whose "challenged" field is
// userDID.
func (b *Backfiller) listChallengesForRepo(ctx context.Context, base, repoDID, userDID string) ([]*challenge.PendingChallenge, error) {
	params := url.Values{
		"repo":       {repoDID},
		"collection": {challengeCollection},
		"limit":      {strconv.Itoa(listRecordsPageLimit)},
	}

	var page struct {
		Records []struct {
			URI   string                 `json:"uri"`
			CID   string                 `json:"cid"`
			Value map[string]interface{} `json:"value"`
		} `json:"records"`
	}
	if err := b.getXRPC(ctx, base, "com.atproto.repo.listRecords", params, &page); err != nil {
		return nil, err
	}

	var out []*challenge.PendingChallenge
	for _, rec := range page.Records {
		challenged, _ := rec.Value["challenged"].(string)
		if challenged != userDID {
			continue
		}
		rkey := rkeyFromURI(rec.URI)
		if rkey == "" {
			continue
		}
		out = append(out, challenge.FromChallengeRecord(repoDID, rkey, rec.CID, rec.Value))
	}
	return out, nil
}

// rkeyFromURI extracts the trailing record key from an at:// URI
// (at://did/collection/rkey).
func rkeyFromURI(uri string) string {
	idx := strings.LastIndex(uri, "/")
	if idx == -1 || idx == len(uri)-1 {
		return ""
	}
	return uri[idx+1:]
}

// getXRPC issues an unauthenticated GET against method on base (these
// com.atproto.sync.* / com.atproto.repo.listRecords endpoints are public
// reads -- no session/token is available or needed here, since this
// backfill deliberately reads OTHER users' repos, not the logging-in
// user's own) and decodes a 200 JSON response into out.
func (b *Backfiller) getXRPC(ctx context.Context, base, method string, params url.Values, out interface{}) error {
	u := strings.TrimRight(base, "/") + "/xrpc/" + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", u, err)
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", u, err)
	}
	return nil
}
