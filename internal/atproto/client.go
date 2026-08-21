package atproto

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/justinabrahms/atchess/internal/auth"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/dpop"
)

// ErrIncompleteDerivation indicates that a game's derived status (see
// GetGame's doc comment) could not be fully verified because at least one
// player's repo could not be read during the terminal-event scan. This is
// deliberately treated as a hard error rather than folded silently into
// "no terminal event found" (which would derive as active): a truncated
// scan has only proven it did not FIND a terminal event, which is a
// different claim from proving one does not exist -- it cannot distinguish
// "no resignation exists" from "could not look". Since the derived status
// is a safety gate (MakeMoveHandler and friends use it to decide whether a
// write is even allowed, not just to render a label), every caller must
// treat this error as "unproven, do not authorize" and fail closed. A
// caller that only needs a best-effort display value may still inspect the
// partial *chess.Game returned alongside this error (see
// Game.DerivationIncomplete), but no such caller exists yet -- see
// atchess-1c9.51.
var ErrIncompleteDerivation = errors.New("game status derivation incomplete: one or more repos could not be read")

// ErrRecordNotFound indicates a requested AT Protocol record genuinely
// does not exist, inferred from the PDS response body's structured
// "error" field being "RecordNotFound" -- NOT from the HTTP status code
// alone, because real AT Protocol PDS implementations disagree on which
// status they use for it (this codebase's own two in-memory PDS test
// doubles disagree too: internal/atproto/lexicon_test.go's fakePDS uses
// HTTP 400, internal/atproto/derive_status_test.go's deriveTestPDS uses
// HTTP 404), so the status code by itself is not a reliable signal. See
// isRecordNotFoundBody, and AcceptChallenge for the caller that most
// depends on this distinction (a record genuinely not existing yet is a
// normal, expected state there -- some other read failure is not).
var ErrRecordNotFound = errors.New("record not found")

// ErrRecordUnavailable indicates a record read failed for a reason OTHER
// than the record genuinely not existing: a network error, an
// unreachable PDS, a DID-resolution failure, or a non-RecordNotFound
// error response from the PDS. Callers that need to tell "this doesn't
// exist" apart from "could not tell" -- see AcceptChallenge -- must
// treat this case very differently (a transient dependency failure, not
// a verdict about what exists).
var ErrRecordUnavailable = errors.New("record temporarily unavailable")

// ErrNotChallengeParticipant indicates the authenticated caller is
// neither the challenger nor the challenged party named in a challenge
// record -- see AcceptChallenge.
var ErrNotChallengeParticipant = errors.New("caller is not a participant in this challenge")

// ErrOnlyChallengedMayAccept indicates the caller is a challenge's
// challenger rather than its challenged party. Only the challenged party
// may ever call AcceptChallenge successfully -- this is checked BEFORE
// AcceptChallenge ever looks for an existing game record, precisely so a
// challenger can never reach the idempotent read-back path either (see
// AcceptChallenge's doc comment; this was a real, reviewer-found gap in
// an earlier version of this method, fixed as part of atchess-1c9.29).
var ErrOnlyChallengedMayAccept = errors.New("only the challenged player may accept this challenge")

// ErrChallengeConflict indicates a record already exists at the
// challenge-derived game URI that does NOT genuinely belong to this
// accept: either its own "challenge" back-reference does not name this
// exact challengeURI (a forged/crafted proposedGameId colliding with an
// unrelated, pre-existing game -- see AcceptChallenge's doc comment), or
// the caller is not one of that record's two players. This is a genuine
// conflict, never treated as idempotent success: the caller must not be
// handed someone else's game, and the challenge must not be silently
// treated as consumed by MarkAccepted (internal/web's
// AcceptChallengeHandler must not call MarkAccepted when this error is
// returned).
var ErrChallengeConflict = errors.New("challenge conflicts with an existing, unrelated game record")

// ErrChallengeNotAcceptable indicates the challenge is not in an
// acceptable state, for either of two reasons: (1) the challenge record
// ITSELF says so -- its own "status" field is "declined" or "accepted",
// or its "expiresAt" is in the past; or (2) a durable decline exists as a
// SEPARATE app.atchess.challengeResponse record in the challenged
// player's own repo (see RespondToChallenge and
// getChallengeDeclineOutcome), which the challenge record's own fields
// cannot reflect (AT Protocol forbids writing into someone else's repo,
// so the decliner can never update the challenger's copy). See
// AcceptChallenge's doc comment for both checks.
var ErrChallengeNotAcceptable = errors.New("challenge is not in an acceptable state")

// ErrChallengeChallengerForged indicates a challenge record's
// self-reported "challenger" field disagrees with the DID authority of
// the at:// URI it was fetched from. AT Protocol only permits a repo
// owner to write into their own repo, so the repo hosting a record is
// its authorship proof, not any field inside it (see BuildChallengeURI's
// doc comment and getResignationOutcome's matching corroboration).
// Without this check, anyone can write an app.atchess.challenge into
// THEIR OWN repo naming an uninvolved third party as "challenger": the
// challenged party's accept then mints a public app.atchess.game
// crediting that third party as a player they never agreed to be
// (atchess-1c9.106).
var ErrChallengeChallengerForged = errors.New("challenge record's challenger field disagrees with its own repo")

// ErrGameRecordConflict indicates a game record update inside RecordMove
// failed its compare-and-swap (com.atproto.repo.putRecord's "swapRecord"
// parameter -- NOT "swapCid", which is not a real putRecord parameter and
// is silently ignored by the PDS if sent; see atchess-1c9.112) because
// another write landed on the same record first -- almost always the
// other player's move winning a race, not a server fault. Detected from
// the PDS response body's structured "error" field beginning with
// "InvalidSwap" (see isInvalidSwapBody), NEVER from the bare HTTP status
// code alone: the real AT Protocol PDS reports this as exactly
// "InvalidSwap" (verified against a live PDS, atchess-1c9.112), and a
// prefix match is used rather than an exact one so that any future
// suffixed variant (e.g. a more specific subtype) still matches. An
// unparseable or non-matching body is deliberately NOT treated as a
// conflict: RecordMove falls back to its ordinary "failed to update game
// record" error, which MakeMoveHandler maps to 500, so an ambiguous
// failure fails closed toward "server error" rather than toward "just
// retry" and silently swallowing a genuine outage.
var ErrGameRecordConflict = errors.New("game record update conflict: the game record was updated by another write first")

// swapRecordCASContract documents the actual, measured contract of
// putRecord's "swapRecord" parameter (atchess-1c9.117, qualifying
// atchess-1c9.112's fix). It is not a type or a function that anything
// calls; it exists so this fact lives next to the code instead of only in
// a bead. Read this before adding a fifth call site that passes
// "swapRecord", or before trusting ErrGameRecordConflict/isInvalidSwapBody
// to mean "any stale token is always caught".
//
// THE MEASURED CONTRACT (confirmed directly against the dual-PDS harness,
// ghcr.io/bluesky-social/pds:latest, @atproto/pds@0.5.27, 2026-08-21, on a
// throwaway collection/record, all four combinations):
//
//   - identical-value put, STALE swapRecord  -> HTTP 200. Record's CID is
//     UNCHANGED; the response carries no "commit" object at all (contrast
//     createRecord/a real update, which both return one) -- no new commit
//     is minted, so a later legitimate CAS against that same CID still
//     works.
//   - identical-value put, CORRECT swapRecord -> HTTP 200, same unchanged
//     CID (expected; included for completeness).
//   - differing-value put, STALE swapRecord   -> HTTP 400
//     {"error":"InvalidSwap","message":"Record was at <cid>"}. This is the
//     only one of the four combinations where the token is actually
//     enforced.
//   - differing-value put, CORRECT swapRecord -> HTTP 200 (not separately
//     re-verified here; this is putRecord's ordinary success path and is
//     exercised by every non-conflicting move in this codebase already).
//
// WHY: this is not a race-timing artifact, it is unconditional server
// logic. @atproto/pds@0.5.27's putRecord handler
// (dist/api/com/atproto/repo/putRecord.js) computes the CID the write
// WOULD produce, and short-circuits to a no-op ("if (current && current.cid
// === write.cid.toString()) { return {commit: null, write} }") BEFORE it
// ever calls actorTxn.repo.processWrites -- which is the only place
// swapRecord/swapCommit are compared against the stored record and a
// BadRecordSwapError (-> "InvalidSwap") can be raised. An identical-value
// write never reaches that comparison, so a stale token on it is not
// rejected; it is simply never looked at.
//
// SO: "swapRecord protects the game record" is true ONLY for a write that
// would actually change the stored value. Do not write code against this
// package that assumes a stale swapRecord is ALWAYS rejected -- it is only
// rejected when the new record differs from what's currently stored.
// swapRecord cannot be used to detect "did anything touch this record
// since I read it", only "am I about to silently overwrite a DIFFERENT
// value than the one I compare-and-swapped against". Every call site in
// this file that sets "swapRecord" today -- RecordMove,
// RespondToDrawOffer's best-effort cache refresh, ResignGame, and
// ClaimTimeVictory; audited under atchess-1c9.117 -- is
// unaffected: each only lands in the no-op case when the write it was
// about to make would not have changed the record's meaning anyway (and
// RespondToDrawOffer's call additionally never inspects the response at
// all), so none of them currently depend on the stronger, false
// assumption. If a future call site needs that stronger guarantee,
// swapRecord alone cannot provide it -- some other mechanism (e.g. a
// content hash / version field compared after the fact, or a
// deterministic rkey the way RecordMove's move records use one) is
// required.
//
// SPEC STATUS: this is an implementation detail of this PDS, not a
// documented part of the AT Protocol lexicon. com.atproto.repo.putRecord's
// generated lexicon definition (dist/lexicons/com/atproto/repo/
// putRecord.defs.js in the same package) declares "swapRecord" as an
// optional nullable CID string and lists "InvalidSwap" as the procedure's
// only defined error code; it says nothing about an identical-value
// exception, and no spec text was found anywhere in this image that
// promises one. Do not rely on this short-circuit against any other AT
// Protocol server implementation, and do not assume a future version of
// this same PDS keeps it -- treat the "safe" call sites above as safe
// because their writes are idempotent regardless of whether the
// short-circuit fires, not because the short-circuit is guaranteed.

// ErrMoveRecordConflict indicates RecordMove's move-record createRecord
// (see moveRkeyForPly) collided at its deterministic (game, ply) rkey with
// an EXISTING move record whose content genuinely differs from the move
// being written now (see moveRecordContentEqual) -- i.e. two different
// legal moves computed the same ply, which only happens when the same
// player double-submits two different moves for the one turn they are
// entitled to make (atchess-1c9.113). This is deliberately distinct from
// the ordinary "identical retry" case (a double-click / dropped-response
// retry of the SAME move), which RecordMove treats as an idempotent
// success and never surfaces as an error at all -- see RecordMove's doc
// comment. A live PDS probe (atchess-1c9.113) found that a colliding
// createRecord with an explicit rkey returns a bare, non-distinguishing
// HTTP 500 ({"error":"InternalServerError"}) rather than any structured
// "already exists" signal, so this is detected by a read-back
// (getRecordByURI) after the failure, NOT from the createRecord response
// body itself -- unlike ErrGameRecordConflict's isInvalidSwapBody, which
// can trust the PDS's own error shape.
var ErrMoveRecordConflict = errors.New("move record conflict: a different move already exists at this game's next ply")

// isInvalidSwapBody reports whether an AT Protocol error response body
// carries a structured "error" field indicating a failed
// compare-and-swap (a "swapRecord" that no longer matches the record's
// current CID) on a putRecord call. See ErrGameRecordConflict's doc
// comment for why this is a prefix match rather than an exact one, and
// why the bare HTTP status code is never used as the signal instead.
//
// A false result from this function does NOT mean the swap succeeded
// because the token was valid -- it may instead mean the swap was never
// evaluated at all. See swapRecordCASContract (atchess-1c9.117): the PDS
// short-circuits an identical-value putRecord to a no-op (HTTP 200)
// before it ever compares swapRecord, so a stale token on a write that
// wouldn't have changed anything never reaches the "InvalidSwap" path
// this function detects.
func isInvalidSwapBody(body []byte) bool {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return false
	}
	return strings.HasPrefix(e.Error, "InvalidSwap")
}

// isRecordNotFoundBody reports whether an AT Protocol error response body
// carries the structured "RecordNotFound" error code. Used instead of the
// HTTP status code alone -- see ErrRecordNotFound's doc comment for why.
// A body that isn't valid JSON, or has no matching "error" field, is not
// treated as RecordNotFound (fail closed: an unparseable/unexpected body
// is exactly the "could not tell" case ErrRecordUnavailable exists for,
// not "confirmed absent").
func isRecordNotFoundBody(body []byte) bool {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return false
	}
	return e.Error == "RecordNotFound"
}

type Client struct {
	pdsURL      string
	accessJWT   string
	refreshJWT  string
	did         string
	handle      string
	httpClient  *http.Client
	dpopManager *auth.DPoPManager
	useDPoP     bool

	// plcDirectoryURL, when non-empty, overrides DefaultPLCDirectoryURL for
	// this client's DID/handle resolution (identity.go) -- see
	// SetPLCDirectoryURL. Empty means "use the default", so the zero value
	// *Client (as produced by any existing constructor) keeps working
	// unchanged.
	plcDirectoryURL string

	// auth, if set, supplies (and refreshes) the access token for this
	// client's requests -- see NewClientFromSession. When nil, the client
	// uses the static accessJWT captured at login (NewClient/
	// NewClientWithDPoP), matching this package's original behaviour.
	auth Authenticator

	// dpopKey, if set, is the DPoP private key an OAuth-bound session
	// (session.DPoPKey) is bound to -- see NewClientFromSession. When set,
	// doRequest signs a real DPoP proof (RFC 9449) and attaches it as the
	// "DPoP" request header for every request, and makeRequest performs
	// the resource-server nonce-challenge retry (RFC 9449 s9: HTTP 401 +
	// a DPoP-Nonce response header). This is deliberately separate from
	// dpopManager/useDPoP above, which is an older, unrelated DPoP
	// mechanism for direct handle+password logins (NewClientWithDPoP) that
	// generates and rotates its own key rather than using one bound to an
	// OAuth session.
	dpopKey *ecdsa.PrivateKey
}

// Authenticator supplies the bearer token used to authenticate a Client's
// requests when it was built via NewClientFromSession (as opposed to a
// direct handle+password login), refreshing that token when necessary.
// Implementations must be safe for concurrent use -- see
// oauth.Session.EnsureFresh/ForceRefresh in internal/oauth, which
// internal/web's sessionAuthenticator is built on.
type Authenticator interface {
	// Token returns a currently-valid access token, refreshing it first if
	// it is at or near expiry.
	Token() (string, error)
	// ForceRefresh discards any local freshness assumption and refreshes
	// immediately. Called after the PDS itself rejects a request with 401,
	// in case the server invalidated the token for a reason this client
	// could not have predicted locally (e.g. revocation).
	ForceRefresh() (string, error)
}

// generateGameID creates a deterministic record key for a game based on challenge parameters
func generateGameID(challengerDID, challengedDID string, timestamp time.Time) string {
	// Create deterministic input from challenge parameters
	input := fmt.Sprintf("%s:%s:%d", challengerDID, challengedDID, timestamp.Unix())

	// Hash the input
	hash := sha256.Sum256([]byte(input))

	// Encode to base32 and take first 13 characters (similar to TID length)
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	encoded := encoder.EncodeToString(hash[:8])

	// Convert to lowercase and add prefix to distinguish from auto-generated TIDs
	return "ch" + strings.ToLower(encoded)[:11]
}

// NewClient creates a new AT Protocol client without DPoP support
func NewClient(pdsURL, handle, password string) (*Client, error) {
	return NewClientWithDPoP(pdsURL, handle, password, false)
}

// NewClientWithDPoP creates a new AT Protocol client with optional DPoP support
func NewClientWithDPoP(pdsURL, handle, password string, useDPoP bool) (*Client, error) {
	var httpClient *http.Client
	var dpopManager *auth.DPoPManager

	if useDPoP {
		// Create DPoP manager
		manager, err := auth.NewDPoPManager()
		if err != nil {
			return nil, fmt.Errorf("failed to create DPoP manager: %w", err)
		}
		dpopManager = manager

		// Create a DPoP-enabled HTTP client
		// We'll set up the token getter after authentication
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	} else {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	// Create session
	sessionReq := map[string]interface{}{
		"identifier": handle,
		"password":   password,
	}

	reqBody, _ := json.Marshal(sessionReq)
	req, err := http.NewRequest("POST", xrpcURL(pdsURL, "com.atproto.server.createSession", nil), bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create session: HTTP %d", resp.StatusCode)
	}

	var session struct {
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
		Did        string `json:"did"`
		Handle     string `json:"handle"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("failed to decode session response: %w", err)
	}

	client := &Client{
		pdsURL:      pdsURL,
		accessJWT:   session.AccessJwt,
		refreshJWT:  session.RefreshJwt,
		did:         session.Did,
		handle:      session.Handle,
		httpClient:  httpClient,
		dpopManager: dpopManager,
		useDPoP:     useDPoP,
	}

	// If using DPoP, update the HTTP client to use the interceptor
	if useDPoP {
		client.httpClient = auth.NewDPoPClient(dpopManager, func() string {
			return client.accessJWT
		})
	}

	return client, nil
}

// GetDID returns the authenticated user's DID
func (c *Client) GetDID() string {
	return c.did
}

// GetAccessJWT returns the current AT Protocol access token (accessJwt).
// Exposed so callers (e.g. internal/web's LoginHandler) can persist it in a
// session for later use by NewClientFromSession.
func (c *Client) GetAccessJWT() string {
	return c.accessJWT
}

// GetRefreshJWT returns the AT Protocol refresh token (refreshJwt) obtained
// at login, if any.
func (c *Client) GetRefreshJWT() string {
	return c.refreshJWT
}

// NewClientFromSession builds a Client that authenticates as the identity
// described by did/handle via auth, rather than performing a fresh
// handle+password login. This is how internal/web's authenticated handlers
// act as the caller (AuthenticatedDID) instead of the protocol-service
// instance's own static configured identity (atchess-1c9.9). useDPoP
// controls whether the Authorization header is "DPoP <token>" or
// "Bearer <token>". dpopKey is the session's DPoP-bound proof key
// (oauth.Session.DPoPKey) when useDPoP is true for an OAuth session; it
// must be non-nil in that case, since a "DPoP <token>" Authorization
// header with no accompanying signed "DPoP" proof header is not a valid
// DPoP request at all (RFC 9449 s4) -- pass nil only when useDPoP is false.
func NewClientFromSession(pdsURL, did, handle string, useDPoP bool, dpopKey *ecdsa.PrivateKey, auth Authenticator) (*Client, error) {
	if auth == nil {
		return nil, fmt.Errorf("NewClientFromSession: an Authenticator is required")
	}
	if useDPoP && dpopKey == nil {
		return nil, fmt.Errorf("NewClientFromSession: useDPoP is true but no DPoP key was provided")
	}
	accessJWT, err := auth.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to obtain access token for session: %w", err)
	}
	return &Client{
		pdsURL:     pdsURL,
		accessJWT:  accessJWT,
		did:        did,
		handle:     handle,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		useDPoP:    useDPoP,
		dpopKey:    dpopKey,
		auth:       auth,
	}, nil
}

// RefreshSession exchanges a refresh token for a new access/refresh token
// pair via com.atproto.server.refreshSession. Used by internal/web to renew
// an app-password session's access token without requiring the user to
// re-enter their password.
func RefreshSession(pdsURL, refreshJWT string) (accessJWT, newRefreshJWT string, err error) {
	req, err := http.NewRequest("POST", xrpcURL(pdsURL, "com.atproto.server.refreshSession", nil), nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+refreshJWT)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("refreshSession request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("refreshSession returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to decode refreshSession response: %w", err)
	}

	return result.AccessJwt, result.RefreshJwt, nil
}

// ParseJWTExpiry extracts the "exp" claim (Unix seconds) from a JWT's
// payload without verifying its signature -- used only to decide when a
// locally-held access token is due for a proactive refresh; the PDS remains
// the sole authority on whether the token is actually still valid.
func ParseJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// makeRequest creates and executes an HTTP request with proper
// authentication, transparently retrying once on a 401.
//
// A 401 can mean two different things for a DPoP-bound (c.dpopKey != nil)
// client, and they get two different responses (atchess-1c9.12 step 3):
//
//   - A DPoP nonce challenge (RFC 9449 s9): the resource server includes a
//     DPoP-Nonce response header. The access token itself may be perfectly
//     valid -- the server just wants a proof signed with a nonce it
//     issued. This is retried EXACTLY ONCE with a freshly-signed proof
//     carrying that nonce (doRequest picks it back up from the shared
//     nonce store -- see below), WITHOUT calling c.auth.ForceRefresh: that
//     would refresh a token that was never the problem. A 401 with no
//     DPoP-Nonce header is NOT treated as a nonce challenge -- there would
//     be nothing to retry with -- and falls through to the branch below
//     instead (atchess-1c9.12 edge case: "do not loop").
//   - Anything else (including a genuinely expired/invalid token, or a
//     non-DPoP session) falls through to the pre-existing behaviour: if
//     the client was built via NewClientFromSession (c.auth != nil), force
//     a refresh and retry once.
//
// These two retry paths are mutually exclusive per call: at most one extra
// request is ever made, regardless of session type.
func (c *Client) makeRequest(method, url string, body []byte) (*http.Response, error) {
	resp, err := c.doRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if c.dpopKey != nil {
			if nonce := resp.Header.Get("DPoP-Nonce"); nonce != "" {
				resp.Body.Close()
				// doRequest re-reads the freshly-updated nonce store
				// itself (see below) -- no need to thread nonce through
				// here.
				resp, err = c.doRequest(method, url, body)
				if err != nil {
					return nil, err
				}
				return resp, nil
			}
		}

		if c.auth != nil {
			resp.Body.Close()
			if _, rerr := c.auth.ForceRefresh(); rerr != nil {
				return nil, fmt.Errorf("request unauthorized (401) and token refresh failed: %w", rerr)
			}
			resp, err = c.doRequest(method, url, body)
			if err != nil {
				return nil, err
			}
		}
	}

	return resp, nil
}

// doRequest performs a single attempt of an authenticated HTTP request. For
// a DPoP-bound client (c.dpopKey != nil), it also signs and attaches a
// fresh DPoP proof (RFC 9449) on every attempt, using whatever nonce the
// shared process-wide nonce store (dpop.DefaultNonceStore, keyed by url's
// origin) currently holds for this server -- so a server whose nonce is
// already known gets it on the first try (atchess-1c9.12 step 4: "so the
// common path is one request not two"), and any response carrying a
// DPoP-Nonce header (success or failure, satisfying atchess-1c9.12's
// nonce-rotation edge case) immediately updates the store for the next
// request, from any Client instance, to pick up.
func (c *Client) doRequest(method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	token := c.accessJWT
	if c.auth != nil {
		t, terr := c.auth.Token()
		if terr != nil {
			return nil, fmt.Errorf("failed to obtain access token: %w", terr)
		}
		token = t
	}

	// Set authorization header based on whether DPoP is enabled
	if c.useDPoP {
		req.Header.Set("Authorization", "DPoP "+token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if c.dpopKey != nil {
		origin := dpop.OriginOf(url)
		nonce := dpop.DefaultNonceStore().Get(origin)
		// htu (RFC 9449 s4.2) must not include the query string; strip it.
		htu := url
		if idx := strings.IndexByte(url, '?'); idx >= 0 {
			htu = url[:idx]
		}
		proof, perr := dpop.CreateProof(c.dpopKey, method, htu, token, nonce)
		if perr != nil {
			return nil, fmt.Errorf("failed to create DPoP proof: %w", perr)
		}
		req.Header.Set("DPoP", proof)
	}

	resp, err := c.httpClient.Do(req)
	if err == nil && resp != nil && c.dpopKey != nil {
		if nonce := resp.Header.Get("DPoP-Nonce"); nonce != "" {
			dpop.DefaultNonceStore().Set(dpop.OriginOf(url), nonce)
		}
	}
	return resp, err
}

// CreateGameFromChallenge creates a game record using a specific rkey and challenge reference
func (c *Client) CreateGameFromChallenge(ctx context.Context, opponentDID, color, rkey, challengeURI, challengeCID string) (*chess.Game, error) {
	return c.createGame(ctx, opponentDID, color, &rkey, challengeURI, challengeCID)
}

func (c *Client) CreateGame(ctx context.Context, opponentDID string, color string) (*chess.Game, error) {
	return c.createGame(ctx, opponentDID, color, nil, "", "")
}

func (c *Client) createGame(ctx context.Context, opponentDID, color string, rkey *string, challengeURI, challengeCID string) (*chess.Game, error) {
	// Determine who plays white/black
	var whiteDID, blackDID string
	if color == "white" {
		whiteDID = c.did
		blackDID = opponentDID
	} else if color == "black" {
		whiteDID = opponentDID
		blackDID = c.did
	} else {
		// Random - for now just make challenger white
		whiteDID = c.did
		blackDID = opponentDID
	}

	// Create initial game record
	gameRecord := map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1", // Starting position
		"pgn":       "",
	}

	// Add challenge reference if provided
	if challengeURI != "" {
		gameRecord["challenge"] = map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		}
	}

	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.game",
		"record":     gameRecord,
	}

	// Add explicit rkey if provided
	if rkey != nil {
		createReq["rkey"] = *rkey
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create game record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create game record: HTTP %d", resp.StatusCode)
	}

	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &chess.Game{
		ID:        createResp.URI,
		White:     whiteDID,
		Black:     blackDID,
		Status:    chess.StatusActive,
		FEN:       gameRecord["fen"].(string),
		PGN:       "",
		CreatedAt: gameRecord["createdAt"].(string),
	}, nil
}

// gameFromRecordValue builds a *chess.Game from a raw app.atchess.game
// record's decoded fields (as returned by getGameRecord) plus the
// record's own at:// URI. Used by AcceptChallenge's idempotent path to
// return an EXISTING game record in exactly the shape createGame already
// returns for a freshly-created one.
func gameFromRecordValue(uri string, value map[string]interface{}) *chess.Game {
	white, _ := value["white"].(string)
	black, _ := value["black"].(string)
	status, _ := value["status"].(string)
	fen, _ := value["fen"].(string)
	pgn, _ := value["pgn"].(string)
	createdAt, _ := value["createdAt"].(string)
	return &chess.Game{
		ID:        uri,
		White:     white,
		Black:     black,
		Status:    chess.GameStatus(status),
		FEN:       fen,
		PGN:       pgn,
		CreatedAt: createdAt,
	}
}

// gameRecordReferencesChallenge reports whether a raw app.atchess.game
// record's "challenge" strongRef names challengeURI exactly. Used by
// AcceptChallenge to verify a record found at a challenge-derived game URI
// genuinely traces back to THIS challenge, rather than being an unrelated
// pre-existing (or crafted-collision) record that merely happens to share
// the same rkey -- see AcceptChallenge's doc comment for the exploit this
// closes (atchess-1c9.29 review fix).
func gameRecordReferencesChallenge(value map[string]interface{}, challengeURI string) bool {
	ref, ok := value["challenge"].(map[string]interface{})
	if !ok {
		return false
	}
	uri, _ := ref["uri"].(string)
	return uri == challengeURI
}

// AcceptChallenge implements the accept side of a challenge exchange for
// the caller identified by c.did (an *atproto.Client built from the
// accepting request's own AT Protocol session -- see
// internal/web.Service.AcceptChallengeHandler, the only production
// caller). challengeURI is the challenge's own at:// URI
// (at://<challenger-did>/app.atchess.challenge/<rkey>).
//
// AUTHORISATION is checked against the challenge record ITSELF -- read
// directly from the challenger's repo via challengeURI -- never trusted
// from caller-supplied input. Only the challenged party may EVER
// successfully call this method: a non-participant gets
// ErrNotChallengeParticipant, and the challenger themselves (a real
// participant, but the wrong one) gets ErrOnlyChallengedMayAccept. That
// check runs BEFORE this method ever looks for an existing game record --
// see the TOCTOU note below for why an earlier version of this ordering
// was a real, reviewer-found vulnerability.
//
// The challenge record's OWN "status" and "expiresAt" fields are also
// checked: "declined" or "accepted" status, or a past expiresAt, are
// rejected with ErrChallengeNotAcceptable rather than silently accepted.
// A decline is actually recorded as a SEPARATE app.atchess.challengeResponse
// record in the DECLINING player's own repo (see RespondToChallenge), not
// as a status flip on the challenge record itself, so this field check
// alone cannot see it (atchess-1c9.29's known limitation). AFTER the role
// gate below establishes that the caller IS the challenged party,
// getChallengeDeclineOutcome performs that second, own-repo read and
// rejects with the same ErrChallengeNotAcceptable if a durable decline is
// found (atchess-1c9.91) -- see that method's doc comment for the
// fail-open policy on an unreachable repo.
//
// IDEMPOTENCY: the resulting game's rkey is the challenge's own
// proposedGameId -- deterministic, not random -- and the game always
// lives in the CHALLENGED player's own repo (createGame/
// CreateGameFromChallenge always write to the CALLING client's own repo,
// and only the challenged player is ever authorized to make that call --
// see above, and note this means the challenger can never reach the
// idempotent read-back path at all, only the challenged player can). So,
// for the challenged player only, AcceptChallenge checks whether that
// exact record already exists BEFORE attempting to create it. A record
// existing there is trusted ONLY if BOTH: (1) its own "challenge"
// strongRef names this exact challengeURI (gameRecordReferencesChallenge)
// -- otherwise a forged challenge whose proposedGameId was crafted to
// collide with an unrelated pre-existing game's rkey would silently hand
// back that unrelated game (and, worse, let the caller's
// AcceptChallengeHandler treat the crafted challenge as durably consumed
// -- see ErrChallengeConflict); and (2) the caller is one of that
// record's own white/black players -- otherwise the SAME crafted-rkey
// technique could hand a challenger a genuine but unrelated game's data
// merely because it happens to occupy the derived URI. Either failing
// returns ErrChallengeConflict, a genuine conflict, NEVER treated as
// idempotent success. Only once both hold is the record returned as-is,
// and no second game record is ever created for the same challenge. This
// makes a duplicate accept (e.g. a double-click, or a client retrying
// after a dropped response) by the challenged player safe to repeat --
// see atchess-1c9.29's orchestrator notes for why idempotent, rather than
// an error, is the right choice for THAT case. A narrow TOCTOU is still
// possible between the existence check and the create call (e.g. two
// near-simultaneous accepts from the same challenged-player session);
// CreateGameFromChallenge failing in that case is handled the same way,
// by re-reading once (and re-verifying both of the same two conditions)
// before giving up, rather than surfaced as an error.
//
// FAILURE TO READ THE CHALLENGE ITSELF is deliberately NOT treated as "it
// doesn't exist, so create the game anyway" -- that would risk creating a
// game whose challenge back-reference nobody can ever re-verify, exactly
// the dangling-reference outcome atchess-1c9.29 warns against. A
// genuinely missing challenge record (the PDS reports RecordNotFound)
// returns an error wrapping ErrRecordNotFound (callers should treat this
// as 404 -- the challenge really doesn't exist). Any OTHER read failure
// (network error, unreachable PDS, DID-resolution failure, a
// non-RecordNotFound PDS error) returns an error wrapping
// ErrRecordUnavailable (callers should treat this as 502 -- a transient
// upstream problem, not a verdict about whether the challenge exists).
func (c *Client) AcceptChallenge(ctx context.Context, challengeURI string) (*chess.Game, error) {
	cid, record, err := c.getRecordByURI(ctx, challengeURI)
	if err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return nil, fmt.Errorf("challenge %s: %w", challengeURI, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("challenge %s: %w: %v", challengeURI, ErrRecordUnavailable, err)
	}

	if typ, _ := record["$type"].(string); typ != "app.atchess.challenge" {
		return nil, fmt.Errorf("challenge %s: record is not an app.atchess.challenge (got %q)", challengeURI, typ)
	}

	challengerDID, _ := record["challenger"].(string)
	challengedDID, _ := record["challenged"].(string)
	challengerColor, _ := record["color"].(string)
	proposedGameID, _ := record["proposedGameId"].(string)
	status, _ := record["status"].(string)
	expiresAtStr, _ := record["expiresAt"].(string)

	if challengerDID == "" || challengedDID == "" {
		return nil, fmt.Errorf("challenge %s: record is missing challenger/challenged", challengeURI)
	}
	if proposedGameID == "" {
		return nil, fmt.Errorf("challenge %s: record has no proposedGameId to derive a game rkey from", challengeURI)
	}

	// Corroborate the record's self-reported "challenger" against the
	// repo it actually lives in -- AT Protocol only permits writing into
	// your own repo, so the URI's authority (not the field) is the
	// authorship proof. Without this, anyone can plant a challenge in
	// THEIR OWN repo naming an uninvolved third party as challenger; the
	// challenged party's honest accept below would otherwise mint a
	// public game crediting that third party as a player (atchess-1c9.106).
	if repoDID := recordRepo(challengeURI); repoDID != "" && repoDID != challengerDID {
		log.Warn().Str("challengeURI", challengeURI).Str("repo", repoDID).
			Str("claimedChallenger", challengerDID).
			Msg("refusing forged challenge: repo hosting the record is not the challenger it names")
		return nil, fmt.Errorf("%w: challenge %s is hosted in repo %s but names challenger %s", ErrChallengeChallengerForged, challengeURI, repoDID, challengerDID)
	}

	if c.did != challengerDID && c.did != challengedDID {
		return nil, fmt.Errorf("%w: %s is neither challenger (%s) nor challenged (%s) for %s", ErrNotChallengeParticipant, c.did, challengerDID, challengedDID, challengeURI)
	}

	// Reject a challenge whose OWN record already says it is not
	// acceptable. This cannot see a decline recorded in a separate
	// app.atchess.challengeResponse record -- that is checked separately,
	// after the role gate below (getChallengeDeclineOutcome).
	if status == "declined" || status == "accepted" {
		return nil, fmt.Errorf("%w: challenge %s has status %q", ErrChallengeNotAcceptable, challengeURI, status)
	}
	if expiresAtStr != "" {
		if expiresAt, perr := time.Parse(time.RFC3339, expiresAtStr); perr == nil && time.Now().After(expiresAt) {
			return nil, fmt.Errorf("%w: challenge %s expired at %s", ErrChallengeNotAcceptable, challengeURI, expiresAtStr)
		}
	}

	// Only the challenged party may ever proceed past this point -- this
	// MUST run before the existence check below. An earlier version of
	// this method checked existence first, which let the CHALLENGER reach
	// the idempotent read-back path too; combined with a
	// proposedGameId crafted (or merely coincidentally colliding) with an
	// unrelated pre-existing game's rkey, that let a challenger read
	// another game's data despite never being one of its players. Gating
	// on role FIRST means a challenger is rejected outright and never
	// even performs the lookup that could leak that data.
	if c.did != challengedDID {
		return nil, fmt.Errorf("%w: %s", ErrOnlyChallengedMayAccept, c.did)
	}

	// Durable-decline check (atchess-1c9.91): the field check above can
	// only see a decline expressed as a status flip on THIS record, but
	// RespondToChallenge never does that -- a decline is a SEPARATE
	// app.atchess.challengeResponse record written into the decliner's own
	// repo (AT Protocol forbids writing into someone else's). challengedDID
	// == c.did here (the role gate above just proved it), so this reads
	// the CALLER's own repo for a durable decline of this exact challenge.
	// See getChallengeDeclineOutcome's doc comment for why this is a
	// deliberate fail-open on read failure, not fail-closed.
	if declined, derr := c.getChallengeDeclineOutcome(ctx, challengeURI, cid, challengedDID); derr != nil {
		log.Warn().Err(derr).Str("challengeURI", challengeURI).Str("challengedDID", challengedDID).
			Msg("could not check for a durable challenge decline; proceeding with accept (fail-open, see getChallengeDeclineOutcome)")
	} else if declined {
		return nil, fmt.Errorf("%w: challenge %s was durably declined", ErrChallengeNotAcceptable, challengeURI)
	}

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", challengedDID, proposedGameID)

	if _, existingValue, gerr := c.getGameRecord(ctx, gameURI); gerr == nil {
		if !gameRecordReferencesChallenge(existingValue, challengeURI) {
			return nil, fmt.Errorf("%w: an existing game at the challenge-derived rkey for %s does not reference this challenge", ErrChallengeConflict, challengeURI)
		}
		white, _ := existingValue["white"].(string)
		black, _ := existingValue["black"].(string)
		if c.did != white && c.did != black {
			return nil, fmt.Errorf("%w: %s is not a player of the existing game for %s", ErrChallengeConflict, c.did, challengeURI)
		}
		return gameFromRecordValue(gameURI, existingValue), nil
	} else if !errors.Is(gerr, ErrRecordNotFound) {
		return nil, fmt.Errorf("checking for an existing game for challenge %s: %w: %v", challengeURI, ErrRecordUnavailable, gerr)
	}

	// The CALLER's own color: the mirror image of what the challenger
	// requested for themselves (challengerColor). Matches the frontend's
	// prior client-side computation in acceptChallenge()
	// (web/static/index.html) exactly, including its fallback: any value
	// other than "white" (including "black", "random", or empty) leaves
	// ourColor at its default "white".
	ourColor := "white"
	if challengerColor == "white" {
		ourColor = "black"
	}

	game, err := c.CreateGameFromChallenge(ctx, challengerDID, ourColor, proposedGameID, challengeURI, cid)
	if err != nil {
		// A concurrent accept (double-click / retried request) may have
		// created the record between the existence check above and this
		// call. Re-read once before surfacing a failure: if a game now
		// exists that genuinely traces back to this same challenge (same
		// two conditions as the primary path above), treat it as the
		// idempotent success case rather than an error.
		if _, raceValue, rerr := c.getGameRecord(ctx, gameURI); rerr == nil && gameRecordReferencesChallenge(raceValue, challengeURI) {
			white, _ := raceValue["white"].(string)
			black, _ := raceValue["black"].(string)
			if c.did == white || c.did == black {
				return gameFromRecordValue(gameURI, raceValue), nil
			}
		}
		return nil, fmt.Errorf("creating game for challenge %s: %w", challengeURI, err)
	}
	return game, nil
}

func (c *Client) RecordMove(ctx context.Context, gameURI string, move *chess.MoveResult) error {
	// Fetch the game record to get its CID and current value
	gameCID, gameValue, err := c.getGameRecord(ctx, gameURI)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}

	// Parse the game URI to get repo and rkey
	parts := strings.Split(gameURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(gameURI, "at://") {
		return fmt.Errorf("invalid game URI format: %s", gameURI)
	}

	repo := parts[2] // The DID
	rkey := parts[4] // The record key

	// Update game record FIRST (CAS-protected) to prevent race conditions.
	// Only the game owner can update the game record; if it belongs to the
	// opponent we still create the move record (the game record is a
	// denormalized cache that gets reconstructed from move records).
	if repo == c.did {
		gameValue["fen"] = move.FEN
		if move.Checkmate || move.Draw {
			if move.Checkmate {
				fenParts := strings.Split(move.FEN, " ")
				if len(fenParts) > 1 && fenParts[1] == "w" {
					gameValue["status"] = "black_won"
				} else {
					gameValue["status"] = "white_won"
				}
			} else if move.Draw {
				gameValue["status"] = "draw"
			}
		}
		gameValue["updatedAt"] = time.Now().Format(time.RFC3339)

		// swapRecordCASContract: safe against the identical-value no-op --
		// when two racing writers compute this SAME move against the same
		// starting gameCID, gameValue is mutated identically by both
		// (deterministic FEN/status; updatedAt is RFC3339 second
		// granularity, so it also matches when both land in the same
		// second), so a no-op here always means the record already holds
		// the value this write wanted anyway. A genuinely different
		// concurrent write (a different move, or landing in a different
		// second) still produces a differing record and gets the swap
		// fully evaluated, surfacing as ErrGameRecordConflict below.
		putReq := map[string]interface{}{
			"repo":       repo,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapRecord": gameCID, // Optimistic concurrency control
		}

		putReqBody, _ := json.Marshal(putReq)
		putResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), putReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer putResp.Body.Close()

		if putResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(putResp.Body)
			if isInvalidSwapBody(body) {
				return fmt.Errorf("%w: HTTP %d, body: %s", ErrGameRecordConflict, putResp.StatusCode, string(body))
			}
			return fmt.Errorf("failed to update game record: HTTP %d, body: %s", putResp.StatusCode, string(body))
		}
	}

	// CAS succeeded (or game is in opponent's repo) — now create the move
	// record. Its rkey is DETERMINISTIC, derived from (gameURI, ply) via
	// moveRkeyForPly rather than left to the PDS's server-generated TID --
	// this is what actually protects a same-player double-submit when the
	// game record above was NOT CAS-protected (repo != c.did, i.e. the
	// mover does not own the game record; atchess-1c9.113). A legal chess
	// game has exactly one move at each ply, so two createRecord calls for
	// the SAME move collide at the SAME rkey, while every other move in
	// the same game gets a distinct rkey (see moveRkeyForPly's doc
	// comment). See the collision-handling block below for what happens
	// when that collision actually occurs.
	ply, plyOK := plyFromFEN(move.FEN)
	if !plyOK {
		// Not expected in practice: move.FEN is always the notnil/chess
		// engine's own freshly-rendered Position.String(), which is
		// always a well-formed 6-field FEN (see internal/chess.Engine.
		// MakeMove). Fail closed rather than fall back to a
		// server-generated rkey, which would silently reintroduce the
		// exact bug this exists to fix.
		return fmt.Errorf("failed to derive ply from move's resultant FEN %q for game %s: cannot compute a deterministic move rkey", move.FEN, gameURI)
	}
	moveRkey := moveRkeyForPly(gameURI, ply)

	moveRecord := map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameURI,
			"cid": gameCID,
		},
		"player": c.did,
		"from":   move.From,
		"to":     move.To,
		"san":    move.SAN,
		"fen":    move.FEN,
	}

	if move.Check {
		moveRecord["check"] = true
	}
	if move.Checkmate {
		moveRecord["checkmate"] = true
	}
	if move.Draw {
		moveRecord["draw"] = true
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.move",
		"rkey":       moveRkey,
		"record":     moveRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create move record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		// A colliding createRecord at an already-occupied rkey was
		// verified live (atchess-1c9.113) to return a bare, non-
		// distinguishing HTTP 500 with no structured "already exists"
		// signal in the body -- unlike putRecord's InvalidSwap, there is
		// no response-body shape to trust here. So instead of trying to
		// classify the failure from this response, read back whatever
		// record actually landed at moveRkey in OUR OWN repo (c.did is
		// always where this create just targeted) and compare it against
		// the move being written now:
		//
		//   - matches (ignoring createdAt, which legitimately differs
		//     between two submissions of the same logical move) => this
		//     was a double-click / retried-request resubmission of the
		//     SAME move that already succeeded once; treat it as an
		//     idempotent success rather than an error.
		//   - present but does NOT match => two DIFFERENT moves computed
		//     the same ply, which only happens when the same player
		//     double-submits two different moves for one turn; exactly
		//     one of them may stand, and this one lost -- ErrMoveRecordConflict.
		//   - the read-back itself fails (network error, genuinely absent
		//     despite the create failing, etc.) => cannot tell what
		//     happened; fail closed with the original ambiguous failure
		//     rather than guess.
		moveURI := fmt.Sprintf("at://%s/app.atchess.move/%s", c.did, moveRkey)
		if _, existing, rerr := c.getRecordByURI(ctx, moveURI); rerr == nil {
			if moveRecordContentEqual(existing, moveRecord) {
				return nil
			}
			return fmt.Errorf("%w: game %s ply %d: HTTP %d, body: %s", ErrMoveRecordConflict, gameURI, ply, resp.StatusCode, string(body))
		}

		return fmt.Errorf("failed to create move record: HTTP %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}

// CreateChallenge writes an app.atchess.challenge record into the CALLER's
// OWN repository (c.did) naming opponentDID as the challenged party. It
// deliberately does NOT attempt to write anything into the challenged
// player's repository -- AT Protocol never permits writing into a repo that
// isn't your own with your own session credentials, so an attempt to do that
// (the retired CreateChallengeNotification, see atchess-1c9.11) always
// failed in federation and has been removed entirely rather than retried.
//
// Delivery to the challenged player is instead the responsibility of the
// discovery mechanism described in internal/challenge, internal/firehose,
// and internal/backfill (see docs/firehose-and-backfill.md for the full
// picture): the challenged player's own protocol-service instance
// discovers this record two ways -- (1) subscribing to the firehose of any
// PDS that might host a challenger (see cmd/protocol/main.go) and indexing
// app.atchess.challenge commits whose "challenged" field matches its own
// authenticated users, with cursor persistence across restarts
// (atchess-1c9.46) rather than a full-log replay on every boot; and (2) a
// login-time repo-read backfill (internal/backfill), run synchronously on
// every login, that queries known PDSes' repos directly for challenges
// issued while that player's session wasn't around -- see that package's
// doc comment for exactly what it can and cannot find. challengerHandle is
// embedded directly in the record so a subscriber can display it without a
// second DID resolution round trip.
func (c *Client) CreateChallenge(ctx context.Context, opponentDID, color, message string) (*chess.Challenge, error) {
	createdAt := time.Now()
	proposedGameID := generateGameID(c.did, opponentDID, createdAt)

	challengeRecord := map[string]interface{}{
		"$type":            "app.atchess.challenge",
		"createdAt":        createdAt.Format(time.RFC3339),
		"challenger":       c.did,
		"challengerHandle": c.handle,
		"challenged":       opponentDID,
		"status":           "pending",
		"color":            color,
		"proposedGameId":   proposedGameID,
		"message":          message,
		"expiresAt":        createdAt.Add(24 * time.Hour).Format(time.RFC3339),
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challenge",
		"record":     challengeRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create challenge record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create challenge record: HTTP %d", resp.StatusCode)
	}

	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &chess.Challenge{
		ID:             createResp.URI,
		CID:            createResp.CID,
		Challenger:     c.did,
		Challenged:     opponentDID,
		Status:         "pending",
		Color:          color,
		ProposedGameId: proposedGameID,
		Message:        message,
		CreatedAt:      challengeRecord["createdAt"].(string),
		ExpiresAt:      challengeRecord["expiresAt"].(string),
	}, nil
}

// RespondToChallenge records a decline of a pending challenge by writing an
// app.atchess.challengeResponse record into the CALLER's (the responding
// player's) OWN repository -- never into the challenger's, which AT
// Protocol does not permit. It references the original challenge by
// strongRef (challengeURI + challengeCID) rather than being co-located with
// it. Acceptance is not expressed through this method: an accepted
// challenge is instead represented by the app.atchess.game record the
// accepting player creates (see CreateGameFromChallenge), which already
// carries the same challenge strongRef.
//
// response is currently always "declined" -- the parameter exists (rather
// than a bool) so the lexicon's enum can grow without an API change.
func (c *Client) RespondToChallenge(ctx context.Context, challengeURI, challengeCID, response string) error {
	if response != "declined" {
		return fmt.Errorf("unsupported challenge response %q (only \"declined\" is currently supported)", response)
	}

	record := map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": response,
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challengeResponse",
		"record":     record,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create challenge response record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create challenge response record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// getGameRecord fetches a game record and returns its CID and value
func (c *Client) getGameRecord(ctx context.Context, gameURI string) (string, map[string]interface{}, error) {
	// Parse the AT Protocol URI to extract repo and rkey
	// Format: at://did:plc:USER/app.atchess.game/RKEY
	parts := strings.Split(gameURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(gameURI, "at://") {
		return "", nil, fmt.Errorf("invalid AT Protocol URI format: %s", gameURI)
	}

	repo := parts[2] // The DID
	rkey := parts[4] // The record key

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get game record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.game"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get game record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if isRecordNotFoundBody(body) {
			return "", nil, fmt.Errorf("game record %s: %w", gameURI, ErrRecordNotFound)
		}
		return "", nil, fmt.Errorf("failed to get game record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var getResp struct {
		URI   string                 `json:"uri"`
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return "", nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return getResp.CID, getResp.Value, nil
}

// getRecordByURI fetches an arbitrary record given its full at:// URI
// (parsing repo/collection/rkey out of it) and returns its CID and value.
// Unlike getGameRecord it is not specific to app.atchess.game -- it exists
// so a strongRef (uri+cid) embedded in one record can be verified to
// actually point at a real record before being trusted, rather than taking
// the embedding record's word for it. See getDrawAcceptOutcome, which uses
// this to confirm a "drawResponse: accepted" record references a
// drawOffer that genuinely exists in the other player's repo -- without
// this, any player could fabricate an acceptance out of thin air.
func (c *Client) getRecordByURI(ctx context.Context, atURI string) (string, map[string]interface{}, error) {
	parts := strings.Split(atURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(atURI, "at://") {
		return "", nil, fmt.Errorf("invalid AT Protocol URI format: %s", atURI)
	}

	repo := parts[2]
	collection := parts[3]
	rkey := parts[4]

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve repo for %s: %w", atURI, err)
	}
	params := url.Values{"repo": {repo}, "collection": {collection}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get record %s: %w", atURI, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if isRecordNotFoundBody(body) {
			return "", nil, fmt.Errorf("record %s: %w", atURI, ErrRecordNotFound)
		}
		return "", nil, fmt.Errorf("failed to get record %s: HTTP %d - %s", atURI, resp.StatusCode, string(body))
	}

	var getResp struct {
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return "", nil, fmt.Errorf("failed to decode record %s: %w", atURI, err)
	}

	return getResp.CID, getResp.Value, nil
}

type moveRecord struct {
	FEN       string
	Checkmate bool
	Draw      bool
	CreatedAt time.Time
	// rkey is the move record's own AT-URI record key (a TID -- see
	// recordKey/moveIsAfter). CreatedAt (an RFC3339 timestamp, second
	// resolution) is not fine-grained enough to order two moves recorded
	// within the same second -- which cross-repo moves routinely are, once
	// atchess-1c9.10 makes it possible to read them at all. rkey is kept as
	// a last-resort fallback tiebreak (see moveIsAfter), but the PRIMARY
	// cross-repo tiebreak for two move records is now FEN, via
	// moveRecordIsAfter/plyFromFEN -- see atchess-1c9.100. rkey comparison
	// across repos is NOT chronologically meaningful (AT Protocol TIDs are
	// only monotonic per-repo; their low-order bits are a random
	// per-process tiebreak, not a synchronized clock), which is exactly why
	// this codebase had a deterministic-but-WRONG answer that could
	// permanently wedge a real cross-PDS game.
	rkey string
}

// recordKey extracts the trailing record key (TID) from an at:// record
// URI, e.g. "at://did:plc:x/app.atchess.move/3mthkghep7k2k" -> "3mthkghep7k2k".
func recordKey(atURI string) string {
	idx := strings.LastIndex(atURI, "/")
	if idx < 0 {
		return atURI
	}
	return atURI[idx+1:]
}

// recordRepo extracts the repo DID from an at:// record URI, e.g.
// "at://did:plc:x/app.atchess.move/3mthkghep7k2k" -> "did:plc:x". Returns
// "" if atURI is not a well-formed at:// record URI.
func recordRepo(atURI string) string {
	parts := strings.Split(atURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(atURI, "at://") {
		return ""
	}
	return parts[2]
}

// moveIsAfter reports whether the event identified by (t, rkey) should be
// considered strictly more recent than (otherT, otherRkey), breaking a
// CreatedAt tie (same second -- RFC3339 is only second-resolution, and
// cross-repo ties are ordinary, not exotic) by comparing rkey strings.
//
// atchess-1c9.100 CORRECTION: this rkey comparison is NOT, and never was,
// a sound chronological tiebreak across repos. AT Protocol TIDs are only
// "monotonic per repo" (https://atproto.com/specs/tid) -- their low-order
// clock-identifier bits are a random-per-process tiebreak, not a
// synchronized counter, so comparing TIDs minted by two DIFFERENT PDS
// processes carries no chronological guarantee whatsoever. A prior version
// of this doc comment incorrectly claimed otherwise, and that mistake is
// exactly what let a real cross-PDS move (Ke7, played later) lose a
// same-second tie to an earlier move (Qh5) and wedge the game permanently
// -- see atchess-1c9.100's measured evidence.
//
// For MOVE records specifically, callers must use moveRecordIsAfter
// instead: a move's own resultant FEN gives a domain-correct, cross-repo-
// comparable ply count (plyFromFEN), which is what actually fixes that
// bug. This function remains the tiebreak for every OTHER terminalEvent
// source (resignation, timeViolation, drawAccept) and for merging
// heterogeneous terminal-event candidates in latestTerminalEvent: none of
// those record types carries a board position, so there is no domain-
// correct answer to "which of two same-second claims came first" the way
// there is for a chess move -- rkey comparison here is retained ONLY for
// its one remaining true property, full determinism (the same two records
// compare the same way on every call, forever), not for any claim of
// chronological accuracy. These are also lower-stakes than the move case:
// in a well-behaved client at most one terminal-event source is ever
// non-nil for a given game, so a same-second tie among them is already a
// pathological/adversarial situation, not the ordinary gameplay this bug
// was about.
func moveIsAfter(t time.Time, rkey string, otherT time.Time, otherRkey string) bool {
	if t.After(otherT) {
		return true
	}
	if t.Before(otherT) {
		return false
	}
	return rkey > otherRkey
}

// plyFromFEN returns the number of half-moves (plies) that had been played
// to reach the position described by fen -- e.g. 1 after White's first
// move, 2 after Black's reply, 3 after White's second move, and so on.
// Standard FEN's active-color and fullmove-number fields (the 2nd and 6th
// of its 6 space-separated fields) make this a cheap, pure parse -- no
// chess-legality simulation is needed:
//
//	ply = 2*(fullmove-1) + (1 if Black is to move next i.e. White just
//	      moved, else 0 if White is to move next i.e. Black just moved or
//	      the game hasn't started)
//
// ok is false if fen does not have enough fields, or its active-color or
// fullmove-number fields are not well-formed ("w"/"b" and a positive
// integer respectively) -- callers must fall back to a different ordering
// signal rather than trust a zero value in that case.
func plyFromFEN(fen string) (ply int, ok bool) {
	fields := strings.Fields(fen)
	if len(fields) < 6 {
		return 0, false
	}
	activeColor := fields[1]
	if activeColor != "w" && activeColor != "b" {
		return 0, false
	}
	fullmove, err := strconv.Atoi(fields[5])
	if err != nil || fullmove < 1 {
		return 0, false
	}
	ply = 2 * (fullmove - 1)
	if activeColor == "b" {
		ply++
	}
	return ply, true
}

// moveRkeyForPly derives a deterministic rkey for a move record from the
// game it belongs to (gameURI) and the ply it represents (plyFromFEN),
// exactly mirroring generateGameID's own hash-then-base32 construction
// above (see its doc comment). Two properties this must hold, both of
// which the hash gives for free:
//
//   - DETERMINISTIC for the SAME move: two createRecord calls writing the
//     same logical move (a double-click, or a client retrying a request
//     whose response was dropped) always hash the same (gameURI, ply)
//     input and land on the same rkey, so the second collides at the PDS
//     instead of minting a second, independent move record. This is what
//     actually closes atchess-1c9.113: the game record's CAS
//     (RecordMove's "if repo == c.did" block above) only ever protects
//     the game OWNER's writes, but every move -- owner or not -- goes
//     through this rkey.
//   - DISTINCT across different moves in the same game: gameURI is fixed
//     per game and a legal chess game has exactly one move at each ply
//     (see moveRecordIsAfter's doc comment for the same fact used
//     elsewhere), so hashing in ply gives every move its own rkey. The
//     one case where two DIFFERENT moves hash to the SAME rkey is exactly
//     the other half of atchess-1c9.113's bug: the same player
//     double-submitting two DIFFERENT moves for the one ply they are
//     entitled to play -- that collision is deliberate, not a defect (see
//     RecordMove's post-failure read-back and ErrMoveRecordConflict).
//
// The input hashed is the full gameURI (not just its rkey) so that two
// games which happen to share an rkey on different repos -- or a
// malformed/adversarial gameURI, see atchess-1c9.92's note that
// proposedGameId is not syntax-checked -- can never collide with each
// other; hashing also means the OUTPUT is always drawn from a fixed,
// syntactically-safe alphabet regardless of what characters gameURI
// itself contains, exactly as generateGameID already relies on for game
// rkeys.
//
// The output is always valid AT Protocol record-key syntax
// ([a-zA-Z0-9._:~-]{1,512}, never "." or ".."): "mv" followed by 11
// lowercase base32 characters (alphabet "a-z2-7"), all of which are
// within that set.
func moveRkeyForPly(gameURI string, ply int) string {
	input := fmt.Sprintf("%s:%d", gameURI, ply)
	hash := sha256.Sum256([]byte(input))
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	encoded := strings.ToLower(encoder.EncodeToString(hash[:8]))
	return "mv" + encoded[:11]
}

// moveRecordContentEqual reports whether two app.atchess.move record
// values describe the SAME logical move, ignoring "createdAt" -- the one
// field that legitimately differs between two createRecord submissions of
// what is otherwise the identical move (a double-click, or a client retry
// after a dropped response, each stamps its own request-time timestamp).
// Used by RecordMove's post-collision read-back to tell an idempotent
// resubmission of the SAME move (safe to treat as success) apart from a
// genuine collision between two DIFFERENT moves at the same ply
// (ErrMoveRecordConflict) -- see moveRkeyForPly's doc comment for why that
// second case can happen at all.
//
// Every OTHER field is compared for exact equality, deliberately including
// the nested "game" strongRef (uri+cid): if the game record's cid embedded
// in the two submissions differs, they were not actually resubmissions of
// the same request against the same observed game state, and must not be
// folded together silently.
//
// DELIBERATELY derived from the UNION of both records' own keys, rather
// than a hand-written field list: a hardcoded list silently drifts out of
// sync the moment moveRecord (above) gains a new field, and the failure
// direction is the dangerous one -- an unlisted field would be excluded
// from the comparison, so two submissions differing ONLY in that new field
// would compare equal and be folded together as an idempotent resubmission
// instead of surfacing as ErrMoveRecordConflict. Deriving from the maps
// themselves makes that class of drift structurally impossible: any key
// added to moveRecord is automatically part of this comparison. "createdAt"
// is the one deliberate, visible exception (see doc comment above).
//
// atchess-1c9.116: "a" is always built directly in Go (moveRecord's own
// numeric/bool literals keep their Go types -- an int stays an int), while
// "b" -- or vice versa, this is called with either side in either
// position -- can be a record just decoded from JSON by getRecordByURI,
// where encoding/json's decode-into-map[string]interface{} turns EVERY
// JSON number into a float64 regardless of whether it was written as "5"
// or "5.0". Comparing those two representations directly with
// reflect.DeepEqual would treat a same-valued int(5) and float64(5) as
// unequal -- harmless today because moveRecord has no numeric field yet,
// but the moment one is added (app.atchess.move's lexicon already
// declares "moveNumber", currently unwritten) an identical resubmission's
// Go-typed int would stop matching its own just-written, JSON-decoded
// float64 and RecordMove would wrongly return ErrMoveRecordConflict
// instead of treating it as an idempotent success.
//
// Fixed by normalizing BOTH sides through the same JSON marshal-then-
// unmarshal round trip before comparing (normalizeRecordForComparison,
// below) rather than, say, hardcoding int/float coercion rules: it makes
// both maps' types converge on whatever encoding/json itself would
// produce -- exactly what getRecordByURI already does to "existing" -- so
// there is nothing further for this function to special-case as moveRecord
// gains new field types over time. It costs one cheap marshal/unmarshal
// pair per call, which is negligible next to the network round trip
// RecordMove already just made.
func moveRecordContentEqual(a, b map[string]interface{}) bool {
	na, aErr := normalizeRecordForComparison(a)
	nb, bErr := normalizeRecordForComparison(b)
	if aErr != nil || bErr != nil {
		// Neither map should ever fail to round-trip through JSON: "a"
		// and "b" are always either built from this package's own
		// string/bool/nested-string-map literals (moveRecord, above) or
		// already came FROM a successful json.Decode in getRecordByURI.
		// Not expected in practice, but fail closed (not equal) rather
		// than guess, matching RecordMove's own stated fail-closed
		// philosophy for its post-collision read-back.
		return false
	}

	keys := make(map[string]struct{}, len(na)+len(nb))
	for k := range na {
		keys[k] = struct{}{}
	}
	for k := range nb {
		keys[k] = struct{}{}
	}

	for k := range keys {
		if k == "createdAt" {
			continue
		}
		va, aOK := na[k]
		vb, bOK := nb[k]
		// Presence is checked explicitly (aOK != bOK), rather than
		// relying on Go's zero-value-on-missing-key map access, so a key
		// present with a nil/JSON-null value on one side and absent
		// entirely on the other is correctly unequal in BOTH directions
		// -- the pre-atchess-1c9.116 version of this function instead
		// read a missing key's zero value as an implicit nil, which
		// happened to match an explicit nil on the other side (equal)
		// when walking a's keys, but did NOT match when walking b's keys
		// (unequal), an order-dependent asymmetry. Not reachable today
		// (moveRecord never sets a nil value), but fixed here rather than
		// left latent alongside the numeric-type fix above.
		if aOK != bOK {
			return false
		}
		if aOK && !reflect.DeepEqual(va, vb) {
			return false
		}
	}
	return true
}

// normalizeRecordForComparison round-trips a move record map through
// encoding/json (marshal, then unmarshal back into a fresh
// map[string]interface{}) so that moveRecordContentEqual can compare two
// maps built by DIFFERENT paths -- one constructed directly in Go
// (moveRecord's own literals) and one decoded from JSON by
// getRecordByURI -- without either path's type choices (int vs float64,
// in particular) affecting the result. See moveRecordContentEqual's doc
// comment for why that matters.
func normalizeRecordForComparison(m map[string]interface{}) (map[string]interface{}, error) {
	buf, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("normalizing move record for comparison: %w", err)
	}
	var normalized map[string]interface{}
	if err := json.Unmarshal(buf, &normalized); err != nil {
		return nil, fmt.Errorf("normalizing move record for comparison: %w", err)
	}
	return normalized, nil
}

// moveRecordIsAfter reports whether the move record (fen, t, rkey) should
// be considered strictly more recent than (otherFEN, otherT, otherRkey).
// This is the atchess-1c9.100 fix: unlike moveIsAfter's generic tiebreak
// (see its doc comment), a MOVE record's own resultant FEN encodes exactly
// how many plies of the game had been played to reach it (plyFromFEN) -- a
// value that is domain-correct and safely comparable across repos
// regardless of which repo wrote the record or when its TID was minted,
// because a legal chess game has exactly one move at each ply. This is
// what actually resolves the bug's concrete example: alice's Qh5 (ply 5)
// and bob's later Ke7 (ply 8), rkeys "3mtkgxtb22726"/"3mtkgxt76jn2y" --
// lexicographically the WRONG way round -- now order correctly by ply
// regardless of TID or which repo each was read from.
//
// Every app.atchess.move record ever written has always carried a "fen"
// field (it predates this fix, and nothing about it changes what gets
// written), so this applies immediately to LEGACY records with no
// migration needed: a game wedged by this bug before the fix is unwedged
// by this function alone, the next time its moves are read.
//
// If either FEN cannot be parsed (plyFromFEN fails -- not expected for any
// record this codebase itself wrote, but a defensive fallback for
// malformed/legacy data) or the two plies are equal (e.g. a duplicate or
// forged claim at the same ply -- not the bug this exists to fix, since a
// legitimate game never produces two distinct move records at one ply),
// this falls back to moveIsAfter's createdAt+TID tiebreak, so behaviour
// for whatever residual case reaches it stays exactly as deterministic as
// before.
//
// TRUST NOTE: a malicious repo owner still fully controls what FEN they
// write into their OWN move record, exactly as today -- deriving ply from
// that FEN adds NO new trust beyond what getLatestMoveForGame already
// placed in that same FEN field to determine the game's visible board
// position (game.FEN = latestMove.FEN). It is not, and does not claim to
// be, a defense against a malicious PDS operator fabricating an entire
// fake game history -- nothing in this package validates a full move
// chain's legality, and doing so would be a separate, larger piece of
// work (see atchess-1c9.100's report). This only fixes the proven,
// non-malicious same-second cross-repo tie.
func moveRecordIsAfter(fen string, t time.Time, rkey string, otherFEN string, otherT time.Time, otherRkey string) bool {
	ply, ok := plyFromFEN(fen)
	otherPly, otherOK := plyFromFEN(otherFEN)
	if ok && otherOK && ply != otherPly {
		return ply > otherPly
	}
	return moveIsAfter(t, rkey, otherT, otherRkey)
}

// StoredMove represents a move record retrieved from a player's PDS repository.
type StoredMove struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	SAN       string    `json:"san"`
	FEN       string    `json:"fen"`
	Player    string    `json:"player"`
	Check     bool      `json:"check"`
	Checkmate bool      `json:"checkmate"`
	Draw      bool      `json:"draw"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListMovesForGame fetches all app.atchess.move records from this client's
// repository that belong to the given game URI.
func (c *Client) ListMovesForGame(ctx context.Context, gameURI string) ([]StoredMove, error) {
	records, err := c.listAllRecords(ctx, c.pdsURL, true, c.did, "app.atchess.move")
	if err != nil {
		return nil, fmt.Errorf("failed to list move records: %w", err)
	}

	var moves []StoredMove
	for _, record := range records {
		var value struct {
			Game struct {
				URI string `json:"uri"`
			} `json:"game"`
			From      string `json:"from"`
			To        string `json:"to"`
			SAN       string `json:"san"`
			FEN       string `json:"fen"`
			Player    string `json:"player"`
			Check     bool   `json:"check"`
			Checkmate bool   `json:"checkmate"`
			Draw      bool   `json:"draw"`
			CreatedAt string `json:"createdAt"`
		}
		if err := json.Unmarshal(record.Value, &value); err != nil {
			// atchess-1c9.119 fix-pass: deliberately skip-and-log a single
			// malformed record instead of failing this whole scan closed (a
			// whole-page decode failure used to do exactly that, before
			// pagination). This is a granularity improvement -- one malformed
			// record can no longer deny service to this entire read -- but it
			// must never be silent, so it is logged rather than just dropped.
			log.Warn().Str("repo", c.did).Str("collection", "app.atchess.move").Str("recordURI", record.URI).Err(err).
				Msg("skipping malformed move record: value did not decode into the expected shape")
			continue
		}
		if value.Game.URI != gameURI {
			continue
		}
		t, err := time.Parse(time.RFC3339, value.CreatedAt)
		if err != nil {
			continue
		}
		moves = append(moves, StoredMove{
			From:      value.From,
			To:        value.To,
			SAN:       value.SAN,
			FEN:       value.FEN,
			Player:    value.Player,
			Check:     value.Check,
			Checkmate: value.Checkmate,
			Draw:      value.Draw,
			CreatedAt: t,
		})
	}

	return moves, nil
}

// deriveTerminalFlagsFromFEN derives whether the given board position is a
// checkmate or a rules-forced draw directly from the FEN, rather than
// trusting a move record's self-reported checkmate/draw flags (see
// atchess-1c9.108). atchess-1c9.100 already established that a move
// record's FEN is the trusted board state (game.FEN = latestMove.FEN), so
// deriving these flags from that same FEN adds no new trust surface -- it
// removes one: a forged record can no longer claim a terminal outcome its
// own FEN does not actually reach.
//
// notnil/chess can evaluate a bare FEN (no move history) for every
// AUTOMATIC outcome that is fully determined by a single position
// snapshot: checkmate, stalemate, insufficient material, and the
// (halfmove-clock-driven) seventy-five-move rule -- confirmed empirically
// against the loaded chess engine before writing this. The one automatic
// draw method that is NOT derivable from a bare FEN is fivefold
// repetition, since that requires the full position history a lone FEN
// snapshot doesn't carry. A genuine fivefold-repetition draw will
// therefore not be recognized as terminal here -- a false negative (an
// honest draw goes unrecognized), never a false positive (nothing can be
// forged INTO recognition), so this is the safe direction to fail in.
// Non-automatic outcomes (draw by agreement, resignation, time violation)
// are not move-record concerns at all -- they are handled by their own
// dedicated record types and getXOutcome functions elsewhere in this
// file, each with their own authorship corroboration.
//
// An invalid or unparseable FEN is treated as non-terminal (both false).
func deriveTerminalFlagsFromFEN(fen string) (checkmate, draw bool) {
	engine, err := chess.NewEngineFromFEN(fen)
	if err != nil {
		return false, false
	}
	switch engine.GetStatus() {
	case chess.StatusWhiteWon, chess.StatusBlackWon:
		return true, false
	case chess.StatusDraw:
		return false, true
	default:
		return false, false
	}
}

// getLatestMoveForGame fetches moves from both players' repos and returns
// the latest move for the given game. This is the source of truth for game state.
func (c *Client) getLatestMoveForGame(ctx context.Context, gameURI string, whiteDID, blackDID string) (*moveRecord, error) {
	var latest *moveRecord
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		records, err := c.listAllRecords(ctx, base, ownRepo, playerDID, "app.atchess.move")
		if err != nil {
			errs = append(errs, fmt.Errorf("list moves for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		for _, record := range records {
			var value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				FEN       string `json:"fen"`
				Player    string `json:"player"`
				Checkmate bool   `json:"checkmate"`
				Draw      bool   `json:"draw"`
				CreatedAt string `json:"createdAt"`
			}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				// atchess-1c9.119 fix-pass: skip-and-log this one malformed
				// record rather than failing the whole scan closed -- see
				// ListMovesForGame's identical comment for the full
				// rationale.
				log.Warn().Str("repo", playerDID).Str("collection", "app.atchess.move").Str("recordURI", record.URI).Err(err).
					Msg("skipping malformed move record: value did not decode into the expected shape")
				continue
			}
			if value.Game.URI != gameURI {
				continue
			}
			// atchess-1c9.108: the hosting repo IS the authorship proof --
			// AT Protocol only permits writing into your own repo -- so a
			// record listed from playerDID's repo that claims a DIFFERENT
			// player made the move is forged. Mirrors getLastMove's
			// corroboration (atchess-1c9.104). Compare against playerDID,
			// the repo CURRENTLY being read in this loop iteration, not a
			// fixed DID -- comparing against the wrong side here would
			// silently drop every legitimate move instead of only forged
			// ones.
			if value.Player != playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
					Str("claimedPlayer", value.Player).
					Str("recordURI", record.URI).
					Msg("ignoring forged move record: repo owner is not the player it names as mover")
				continue
			}
			t, err := time.Parse(time.RFC3339, value.CreatedAt)
			if err != nil {
				continue
			}
			rkey := recordKey(record.URI)
			if latest == nil || moveRecordIsAfter(value.FEN, t, rkey, latest.FEN, latest.CreatedAt, latest.rkey) {
				// atchess-1c9.108: derive checkmate/draw from the FEN
				// itself rather than trusting the record's self-reported
				// flags -- see deriveTerminalFlagsFromFEN's doc comment.
				// atchess-1c9.100 already established the FEN as the
				// trusted board state (game.FEN = latestMove.FEN), so this
				// adds no new trust surface; it removes one.
				checkmate, draw := deriveTerminalFlagsFromFEN(value.FEN)
				latest = &moveRecord{
					FEN:       value.FEN,
					Checkmate: checkmate,
					rkey:      rkey,
					Draw:      draw,
					CreatedAt: t,
				}
			}
		}
	}

	return latest, errors.Join(errs...)
}

// terminalEvent is one candidate final outcome for a game, sourced from a
// move (checkmate/draw), a resignation, a time-violation claim, or an
// accepted draw-offer response. GetGame merges candidates from every
// source -- each itself read across BOTH players' repos, since a terminal
// event is always written into the triggering player's own repo, which may
// not be the repo that owns the shared (and otherwise possibly stale)
// app.atchess.game record -- and applies whichever is most recent.
//
// at/rkey/kind feed latestTerminalEvent's ordering, since createdAt alone
// is only second-resolution and cross-repo ties are ordinary:
//
//   - for a checkmate/draw event (kind == terminalEventFromMove), the
//     at/rkey fields are populated from a moveRecord that was ALREADY
//     correctly ordered against every OTHER move by moveRecordIsAfter (see
//     getLatestMoveForGame), so a move-vs-move tie can never reach this
//     struct in a way that matters -- it carries the domain-correct
//     per-move outcome forward even though this struct itself has no FEN
//     to re-derive ply from;
//   - resignation/timeViolation/drawAccept events have no board position
//     at all, so there is no ply-like signal for THOSE to carry;
//   - what remains, and what atchess-1c9.101 fixed, is a same-second tie
//     BETWEEN a move event and a resignation/drawAccept event (proven the
//     only reachable cross-kind tie -- see latestTerminalEvent's doc
//     comment). kind is what lets that be resolved by chess rule rather
//     than by moveIsAfter's TID tiebreak, which atchess-1c9.100 already
//     proved is not a chronological signal across repos. moveIsAfter's TID
//     tiebreak is retained ONLY as the last-resort, kind-blind fallback
//     for every OTHER cross-kind pairing (e.g. resignation vs. drawAccept).
//     Those pairings were NOT ANALYSED for reachability -- which is not the
//     same as being unreachable, and resignation vs. drawAccept in
//     particular is reachable by the same concurrent-write mechanism as the
//     tie fixed here. They are merely lower-stakes: both candidates are
//     terminal and the result is deterministic either way. See moveIsAfter's
//     doc comment for exactly what its tiebreak does and does not
//     guarantee.
type terminalEvent struct {
	status chess.GameStatus
	at     time.Time
	rkey   string
	kind   terminalEventKind
}

// terminalEventKind classifies a terminalEvent by the record type it was
// derived from, so latestTerminalEvent can break a same-second tie between
// two DIFFERENT kinds by a domain rule instead of by TID (see
// terminalEventIsAfter).
type terminalEventKind int

const (
	// terminalEventUnknown is the ZERO VALUE on purpose. terminalEventIsAfter
	// gives move-sourced events precedence, so if the dominant kind sat at
	// iota 0 a future construction site that forgot to set kind would
	// silently win every same-second tie. An unset kind must be the inert
	// one, not the decisive one.
	terminalEventUnknown terminalEventKind = iota

	// terminalEventFromMove is a checkmate or rules-forced draw (e.g.
	// stalemate) carried forward from getLatestMoveForGame's already
	// ply-ordered result.
	terminalEventFromMove
	terminalEventFromResignation
	terminalEventFromTimeViolation
	terminalEventFromDrawAccept
)

// latestTerminalEvent returns whichever of the given candidate terminal
// events (any of which may be nil, meaning "that source found nothing") is
// most recent, or nil if none were found. In a well-behaved client at most
// one of these should ever be non-nil for a given game, but ties are
// broken deterministically rather than left to map/slice iteration order --
// see terminalEventIsAfter for exactly how.
func latestTerminalEvent(events ...*terminalEvent) *terminalEvent {
	var latest *terminalEvent
	for _, e := range events {
		if e == nil {
			continue
		}
		if latest == nil || terminalEventIsAfter(e, latest) {
			latest = e
		}
	}
	return latest
}

// terminalEventIsAfter reports whether candidate should be considered
// strictly more recent than current, for latestTerminalEvent's purpose of
// picking a game's single final outcome among heterogeneous terminal-event
// sources.
//
// atchess-1c9.101: of the cross-kind pairings its reachability analysis
// examined, only ONE is reachable in practice -- a terminal move (checkmate
// or a rules-forced draw) tying, in the same wall-clock second, with a
// resignation or an accepted draw offer. (move-vs-timeViolation is
// unreachable because getTimeViolationOutcome rejects any claim less than a
// full time-limit period -- at least a day -- after the last move; a
// non-terminal move never becomes a moveEvent candidate at all; see the
// bead for the full proof. Pairings among resignation/timeViolation/
// drawAccept themselves were not part of that analysis and are left to the
// fallback below, unchanged.) For that one reachable pairing, the two
// candidates are not symmetric under chess rules: a checkmate or
// forced-draw board position is final the instant it is reached -- mate is
// not negotiable -- while a resignation or draw-agreement is a voluntary
// claim by a player that a game (which may, unbeknownst to them, have
// already ended) is over. A resignation/drawAccept landing in the same
// recorded second as the mating move cannot retroactively un-happen that
// move, so the move-sourced candidate always wins such a tie, in EITHER
// argument order -- this function does not care which candidate is "latest
// so far" and which is newly compared.
//
// Every other cross-kind pairing (e.g. resignation vs. drawAccept) has no
// such domain rule available -- neither candidate's kind is more
// "decisive" than the other's, and in a well-behaved client the two
// sources are mutually exclusive for a live game in the first place -- so
// it falls back to moveIsAfter's (createdAt, TID) tiebreak: fully
// deterministic, but (per moveIsAfter's own doc comment) not a
// chronological guarantee. That fallback is unchanged from before this fix
// and is not what atchess-1c9.101 addresses.
func terminalEventIsAfter(candidate, current *terminalEvent) bool {
	if !candidate.at.Equal(current.at) {
		return candidate.at.After(current.at)
	}
	if candidate.kind == terminalEventFromMove && current.kind != terminalEventFromMove {
		return true
	}
	if current.kind == terminalEventFromMove && candidate.kind != terminalEventFromMove {
		return false
	}
	return moveIsAfter(candidate.at, candidate.rkey, current.at, current.rkey)
}

// getResignationOutcome scans app.atchess.resignation records in BOTH
// players' repos for gameURI and returns the most recent one as a
// terminalEvent (the resigning player's opponent wins), or nil if none
// exists. ResignGame (this file) always writes the resignation record into
// the RESIGNING player's own repo, which is not necessarily the repo that
// owns the shared app.atchess.game record, so it must be discoverable
// regardless of who created that record -- the same reason
// getLatestMoveForGame reads both repos for moves.
//
// Authorship is checked against the repo the record was actually read
// from: a record found in playerDID's own repo may only assert that
// playerDID resigned. Without this, black could write a resignation
// record into black's OWN repo naming white as the resigningPlayer and
// unilaterally declare a black_won outcome -- nobody's repo-write
// permissions stop them writing arbitrary field values into their own
// records, so the repo boundary is the only thing that can be trusted,
// never a same-record field (atchess-1c9.48 review).
func (c *Client) getResignationOutcome(ctx context.Context, gameURI, whiteDID, blackDID string) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		records, err := c.listAllRecords(ctx, base, ownRepo, playerDID, "app.atchess.resignation")
		if err != nil {
			errs = append(errs, fmt.Errorf("list resignations for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		for _, record := range records {
			var value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				ResigningPlayer string `json:"resigningPlayer"`
				CreatedAt       string `json:"createdAt"`
			}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				// atchess-1c9.119 fix-pass: skip-and-log -- see
				// ListMovesForGame's comment for the full rationale.
				log.Warn().Str("repo", playerDID).Str("collection", "app.atchess.resignation").Str("recordURI", record.URI).Err(err).
					Msg("skipping malformed resignation record: value did not decode into the expected shape")
				continue
			}
			if value.Game.URI != gameURI {
				continue
			}
			if value.ResigningPlayer != playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
					Str("claimedResigningPlayer", value.ResigningPlayer).
					Str("recordURI", record.URI).
					Msg("ignoring forged resignation record: repo owner is not the player it names as resigning")
				continue
			}
			t, err := time.Parse(time.RFC3339, value.CreatedAt)
			if err != nil {
				continue
			}
			status := chess.StatusWhiteWon
			if value.ResigningPlayer == whiteDID {
				status = chess.StatusBlackWon
			}
			candidate := &terminalEvent{status: status, at: t, rkey: recordKey(record.URI), kind: terminalEventFromResignation}
			if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
				latest = candidate
			}
		}
	}

	return latest, errors.Join(errs...)
}

// defaultDaysPerMove is the correspondence time limit applied wherever a
// game's time control is absent, or its daysPerMove is unset/zero on what
// is (or defaults to) a correspondence game. See resolveTimeControl's doc
// comment: this is the ONLY named default in the package, and every
// caller must resolve through that function rather than re-declaring this
// literal itself (atchess-1c9.88).
const defaultDaysPerMove = 3

// resolveTimeControl returns the EFFECTIVE time-control type/daysPerMove
// that governs a game, given whatever was actually persisted for it
// (rawType/rawDaysPerMove -- a game's own "timeControl" field, or its
// challenge's). This is the single place the policy question "what does
// an absent or zero daysPerMove mean?" is decided:
//
//   - an absent type (rawType == "", i.e. no timeControl was ever
//     persisted at all -- the only case that occurs today, since nothing
//     in this codebase writes timeControl yet: atchess-1c9.88/.90)
//     resolves to "correspondence" with defaultDaysPerMove days;
//   - a non-positive daysPerMove on a type that is (or has just been
//     defaulted to) "correspondence" also resolves to defaultDaysPerMove
//     days -- correspondence play cannot have a zero-day deadline, so
//     zero is treated the same as absent;
//   - any other explicitly-set, non-correspondence type (e.g. "rapid") is
//     left exactly as given, including a zero/unused daysPerMove -- this
//     function only ever asserts the correspondence default, it never
//     invents a type that wasn't there.
//
// This deliberately PRESERVES today's effective behaviour (ClaimTimeVictory,
// via CheckTimeViolation, has always defaulted an unconfigured game to a
// 3-day correspondence limit and awarded time-violation wins on that
// basis) rather than changing what a player experiences: atchess-1c9.88
// narrowly fixes the fact that getTimeViolationOutcome (reached via
// GetGame) used to disagree with that default -- treating an absent
// timeControl as "no timeout is ever possible" instead -- which meant a
// player could be timed out of a game GetGame's own derived status
// insisted was still active. Whether time controls should be a supported,
// persisted feature at all is a separate, deliberately deferred product
// question (atchess-1c9.90).
//
// Every caller that reads a game's or challenge's time control and needs
// to reason about whether a timeout is possible -- getTimeViolationOutcome
// (via GetGame), CheckTimeViolation (via ClaimTimeVictory), and
// GetTimeRemaining -- MUST call this function rather than re-implementing
// the default inline. Two copies of the same literal is exactly how this
// package previously ended up with getTimeViolationOutcome and
// ClaimTimeVictory silently disagreeing about what an absent time control
// means.
func resolveTimeControl(rawType string, rawDaysPerMove int) (timeControlType string, daysPerMove int) {
	timeControlType, daysPerMove = rawType, rawDaysPerMove
	if timeControlType == "" {
		timeControlType = "correspondence"
	}
	if timeControlType == "correspondence" && daysPerMove <= 0 {
		daysPerMove = defaultDaysPerMove
	}
	return timeControlType, daysPerMove
}

// getTimeViolationOutcome scans app.atchess.timeViolation records in BOTH
// players' repos for gameURI and returns the most recent one as a
// terminalEvent (the violating player's opponent wins), or nil if none
// exists. See getResignationOutcome's doc comment for why both repos must
// be read.
//
// A timeViolation record is a one-sided claim -- "my opponent didn't move
// in time" -- and nothing stops the claiming player from writing one the
// instant after their own move, or with a fabricated violatingPlayer. So,
// unlike a resignation (self-report, trivially checked against the repo it
// came from) a timeViolation claim is only trusted here if it can be
// checked against something the claimer does NOT control:
//   - authorship: found in repo X, the record's claimingPlayer must be X
//     (this mirrors ClaimTimeVictory, which always writes into its own
//     caller's repo) and its violatingPlayer must be the OTHER player, not
//     X itself;
//   - timing: lastActivityAt (the real timestamp of the game's last move,
//     or its creation if there is none yet -- supplied by the caller,
//     which already has this from the move-record scan every GetGame call
//     does anyway, so this costs no extra round trip) plus the game's
//     actual daysPerMove time control must already have elapsed as of the
//     claim's own createdAt. A claim made before its own deadline had
//     passed is rejected outright.
//
// For any time control type other than "correspondence" this package has
// no sound way to verify elapsed time server-side (see CheckTimeViolation's
// own TODO -- rapid/blitz/bullet per-player clocks are not tracked at
// all), so such a claim can never be verified here; rather than silently
// trusting it, it is treated as advisory only and excluded from the
// authoritative merge (logged, not applied).
func (c *Client) getTimeViolationOutcome(ctx context.Context, gameURI, whiteDID, blackDID string, timeControlType string, daysPerMove int, lastActivityAt time.Time, lastActivityKnown bool) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		records, err := c.listAllRecords(ctx, base, ownRepo, playerDID, "app.atchess.timeViolation")
		if err != nil {
			errs = append(errs, fmt.Errorf("list timeViolations for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		for _, record := range records {
			var value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				ClaimingPlayer  string `json:"claimingPlayer"`
				ViolatingPlayer string `json:"violatingPlayer"`
				CreatedAt       string `json:"createdAt"`
			}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				// atchess-1c9.119 fix-pass: skip-and-log -- see
				// ListMovesForGame's comment for the full rationale.
				log.Warn().Str("repo", playerDID).Str("collection", "app.atchess.timeViolation").Str("recordURI", record.URI).Err(err).
					Msg("skipping malformed timeViolation record: value did not decode into the expected shape")
				continue
			}
			if value.Game.URI != gameURI {
				continue
			}

			if value.ClaimingPlayer != playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
					Str("claimedClaimingPlayer", value.ClaimingPlayer).
					Str("recordURI", record.URI).
					Msg("ignoring forged timeViolation record: repo owner is not the player it names as claiming")
				continue
			}
			if value.ViolatingPlayer != whiteDID && value.ViolatingPlayer != blackDID {
				continue
			}
			if value.ViolatingPlayer == playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).Str("recordURI", record.URI).
					Msg("ignoring nonsensical timeViolation record: claimant named themselves as the violator")
				continue
			}

			t, err := time.Parse(time.RFC3339, value.CreatedAt)
			if err != nil {
				continue
			}

			if timeControlType != "correspondence" {
				// Cannot be soundly verified -- advisory only, never
				// authoritative. See doc comment above.
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("timeControlType", timeControlType).
					Msg("timeViolation record for a non-correspondence time control cannot be verified server-side; treating as advisory and excluding it from derived game status")
				continue
			}
			if !lastActivityKnown {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).
					Msg("timeViolation record cannot be verified without a known last-activity timestamp; treating as advisory and excluding it from derived game status")
				continue
			}
			// daysPerMove is expected to already be
			// resolveTimeControl's EFFECTIVE value by the time it
			// reaches here -- GetGame resolves the game's raw
			// timeControl through resolveTimeControl before calling
			// this function, so for a "correspondence" record (the
			// only kind that reaches this point; see the type check
			// above) it should never legitimately be <= 0. This is no
			// longer where "absent/zero means no timeout is possible"
			// is decided -- that policy lives solely in
			// resolveTimeControl now (atchess-1c9.88). What's left
			// here is a defensive fallback: if daysPerMove somehow
			// still is <= 0, timeLimit would be zero/negative and
			// every claim would trivially satisfy "elapsed", turning
			// a bad value into an automatic win for the claimant --
			// the opposite of safe -- so it fails closed rather than
			// trusting its caller unconditionally.
			if daysPerMove <= 0 {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Int("daysPerMove", daysPerMove).
					Msg("ignoring timeViolation record: daysPerMove is not positive even after resolution -- refusing to treat a non-positive limit as an automatic violation")
				continue
			}
			// Reject a claim made before its own deadline had
			// actually elapsed -- this is the check that verifies
			// TIMING (see doc comment above); it is unrelated to the
			// defaulting concern above it and must be preserved
			// exactly as-is.
			timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
			if t.Sub(lastActivityAt) < timeLimit {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).
					Time("lastActivityAt", lastActivityAt).Time("claimedAt", t).Int("daysPerMove", daysPerMove).
					Msg("ignoring premature/forged timeViolation record: claimed before its own deadline had actually elapsed")
				continue
			}

			// Mirrors ClaimTimeVictory's own winner determination:
			// the violating player loses.
			status := chess.StatusWhiteWon
			if value.ViolatingPlayer == whiteDID {
				status = chess.StatusBlackWon
			}
			candidate := &terminalEvent{status: status, at: t, rkey: recordKey(record.URI), kind: terminalEventFromTimeViolation}
			if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
				latest = candidate
			}
		}
	}

	return latest, errors.Join(errs...)
}

// getDrawAcceptOutcome scans app.atchess.drawResponse records in BOTH
// players' repos for gameURI and returns the most recent "accepted" one as
// a terminalEvent (status=draw), or nil if none exists. RespondToDrawOffer
// always writes the response into the RESPONDING player's own repo (never
// the offering player's -- AT Protocol forbids that cross-repo write), so
// it must be discoverable regardless of who created it or who owns the
// game record.
//
// An "accepted" drawResponse is otherwise a completely unilateral claim: a
// player could write one into their own repo with no corresponding offer
// at all and, absent a check, derive a draw out of thin air. So a
// candidate is only trusted here once its drawOffer strongRef is verified
// to actually resolve, in the OTHER player's repo, to a real, matching
// app.atchess.drawOffer for this same game -- "no offer, no draw"
// (atchess-1c9.48 review). This costs one extra getRecord round trip, but
// only for records that pass the cheap in-memory checks first (game URI,
// response == "accepted", and respondedBy authorship), so it is paid at
// most once per game, right when a draw is actually being accepted -- not
// on the hot per-move path.
func (c *Client) getDrawAcceptOutcome(ctx context.Context, gameURI, whiteDID, blackDID string) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		records, err := c.listAllRecords(ctx, base, ownRepo, playerDID, "app.atchess.drawResponse")
		if err != nil {
			errs = append(errs, fmt.Errorf("list drawResponses for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		for _, record := range records {
			var value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				DrawOffer struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				} `json:"drawOffer"`
				RespondedBy string `json:"respondedBy"`
				Response    string `json:"response"`
				CreatedAt   string `json:"createdAt"`
			}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				// atchess-1c9.119 fix-pass: skip-and-log -- see
				// ListMovesForGame's comment for the full rationale.
				log.Warn().Str("repo", playerDID).Str("collection", "app.atchess.drawResponse").Str("recordURI", record.URI).Err(err).
					Msg("skipping malformed drawResponse record: value did not decode into the expected shape")
				continue
			}
			if value.Game.URI != gameURI || value.Response != "accepted" {
				continue
			}
			if value.RespondedBy != playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
					Str("claimedRespondedBy", value.RespondedBy).Str("recordURI", record.URI).
					Msg("ignoring forged drawResponse record: repo owner is not the player it names as responding")
				continue
			}

			offerURI := value.DrawOffer.URI
			offerRepo := recordRepo(offerURI)
			if offerRepo == "" || offerRepo == playerDID {
				log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).Str("recordURI", record.URI).Str("offerURI", offerURI).
					Msg("ignoring drawResponse record: its drawOffer strongRef does not point at the other player's repo")
				continue
			}
			if offerRepo != whiteDID && offerRepo != blackDID {
				continue
			}

			offerCID, offerValue, err := c.getRecordByURI(ctx, offerURI)
			if err != nil {
				log.Warn().Err(err).Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
					Msg("ignoring drawResponse record: its referenced drawOffer could not be read (no offer, no draw)")
				continue
			}
			if offerCID != value.DrawOffer.CID {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
					Str("wantCID", value.DrawOffer.CID).Str("gotCID", offerCID).
					Msg("ignoring drawResponse record: its drawOffer strongRef CID does not match the current offer record")
				continue
			}
			offerGameRef, _ := offerValue["game"].(map[string]interface{})
			offerGameURI, _ := offerGameRef["uri"].(string)
			if offerGameURI != gameURI {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerGameURI", offerGameURI).
					Msg("ignoring drawResponse record: its drawOffer belongs to a different game")
				continue
			}
			offeredBy, _ := offerValue["offeredBy"].(string)
			if offeredBy != offerRepo {
				log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
					Msg("ignoring drawResponse record: its drawOffer's offeredBy does not match the repo it was found in")
				continue
			}

			t, err := time.Parse(time.RFC3339, value.CreatedAt)
			if err != nil {
				continue
			}
			candidate := &terminalEvent{status: chess.StatusDraw, at: t, rkey: recordKey(record.URI), kind: terminalEventFromDrawAccept}
			if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
				latest = candidate
			}
		}
	}

	return latest, errors.Join(errs...)
}

// getChallengeDeclineOutcome reports whether challengeURI/challengeCID has
// been durably declined, by looking for a matching
// app.atchess.challengeResponse record (response == "declined") in
// challengedDID's OWN repo -- the only repo RespondToChallenge could ever
// have written such a record into (AT Protocol forbids writing into
// someone else's repo, atchess-1c9.11). AcceptChallenge's role gate (see
// its doc comment) already requires c.did == challengedDID for every
// caller reaching this check, so challengedDID here is provably the SAME
// identity as the one now asking to accept -- there is no cross-party
// attack this defends against (atchess-1c9.91's step-zero finding: only
// the challenged player can ever decline OR accept a given challenge),
// only a stale-tab/double-submit self-override, where a player who
// already declined tries to accept the same challenge anyway.
//
// A candidate record is trusted only once its "challenge" strongRef
// (uri+cid) is confirmed to name THIS EXACT challengeURI/challengeCID
// pair -- the same "corroborate the claim, don't just trust the field"
// pattern as getDrawAcceptOutcome's drawOffer verification and
// AcceptChallenge's own challenger-forgery check -- rather than trusting
// any response=="declined" record that merely happens to sit in the right
// repo. Because the read is scoped to exactly challengedDID's own repo (we
// choose the "repo" listRecords parameter, not any field inside a
// record), a decline record planted in the WRONG repo (e.g. the
// challenger's own, or an uninvolved third party's) is never even
// examined -- it cannot block a legitimate accept.
//
// FAILURE TO READ challengedDID's repo is a deliberate FAIL-OPEN, unlike
// ErrIncompleteDerivation's fail-closed policy for cross-player game
// status derivation (atchess-1c9.103 and friends): this read is not a
// cross-party safety gate -- decliner and accepter are provably the same
// identity (see above) -- and it reads from the SAME repo/PDS this very,
// already-authenticated accept request just came through, so an
// unreachable read here is a transient blip in the requester's OWN
// dependency, not a third party's. Failing closed would trade a rare,
// self-inflicted, easily-retried bug (a stale decline silently
// overridden) for a routine availability regression on every legitimate
// accept whenever this one collection listing hiccups. The caller
// (AcceptChallenge) logs and proceeds on error; it never surfaces this as
// a failure. This does add one listRecords round trip to every accept.
func (c *Client) getChallengeDeclineOutcome(ctx context.Context, challengeURI, challengeCID, challengedDID string) (declined bool, err error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, challengedDID)
	if err != nil {
		return false, fmt.Errorf("resolve read endpoint for %s: %w", challengedDID, err)
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, challengedDID, "app.atchess.challengeResponse")
	if err != nil {
		return false, fmt.Errorf("list challengeResponses for %s: %w", challengedDID, err)
	}

	for _, record := range records {
		var value struct {
			Challenge struct {
				URI string `json:"uri"`
				CID string `json:"cid"`
			} `json:"challenge"`
			Response string `json:"response"`
		}
		if err := json.Unmarshal(record.Value, &value); err != nil {
			// atchess-1c9.119 fix-pass: skip-and-log -- see
			// ListMovesForGame's comment for the full rationale.
			log.Warn().Str("repo", challengedDID).Str("collection", "app.atchess.challengeResponse").Str("recordURI", record.URI).Err(err).
				Msg("skipping malformed challengeResponse record: value did not decode into the expected shape")
			continue
		}
		if value.Response != "declined" {
			continue
		}
		if value.Challenge.URI != challengeURI {
			continue
		}
		if value.Challenge.CID != challengeCID {
			log.Warn().Str("challengeURI", challengeURI).Str("repo", challengedDID).Str("recordURI", record.URI).
				Str("wantCID", challengeCID).Str("gotCID", value.Challenge.CID).
				Msg("ignoring challengeResponse record: its challenge strongRef CID does not match the current challenge record")
			continue
		}
		return true, nil
	}
	return false, nil
}

func (c *Client) GetGame(ctx context.Context, gameURI string) (*chess.Game, error) {
	// Parse the AT Protocol URI to extract repo and rkey
	// Example URI: at://did:plc:example/app.atchess.game/3k2uv5...
	// We need to call com.atproto.repo.getRecord

	// Parse the URI to extract components
	// Format: at://did:plc:USER/app.atchess.game/RKEY
	parts := strings.Split(gameURI, "/")
	if len(parts) < 4 || !strings.HasPrefix(gameURI, "at://") {
		return nil, fmt.Errorf("invalid AT Protocol URI format: %s", gameURI)
	}

	repo := parts[2] // The DID
	rkey := parts[4] // The record key

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.game"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get game record: HTTP %d", resp.StatusCode)
	}

	var getResp struct {
		Value struct {
			Type        string `json:"$type"`
			CreatedAt   string `json:"createdAt"`
			White       string `json:"white"`
			Black       string `json:"black"`
			Status      string `json:"status"`
			FEN         string `json:"fen"`
			PGN         string `json:"pgn"`
			TimeControl *struct {
				Type        string `json:"type"`
				Initial     int    `json:"initial"`
				Increment   int    `json:"increment"`
				DaysPerMove int    `json:"daysPerMove"`
			} `json:"timeControl"`
		} `json:"value"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var timeControl *chess.TimeControl
	if getResp.Value.TimeControl != nil {
		timeControl = &chess.TimeControl{
			Type:        getResp.Value.TimeControl.Type,
			DaysPerMove: getResp.Value.TimeControl.DaysPerMove,
			Initial:     getResp.Value.TimeControl.Initial,
			Increment:   getResp.Value.TimeControl.Increment,
		}
	}

	game := &chess.Game{
		ID:          gameURI,
		White:       getResp.Value.White,
		Black:       getResp.Value.Black,
		Status:      chess.GameStatus(getResp.Value.Status),
		FEN:         getResp.Value.FEN,
		PGN:         getResp.Value.PGN,
		TimeControl: timeControl,
		CreatedAt:   getResp.Value.CreatedAt,
	}

	// Reconstruct current state from records across BOTH players' repos.
	// The game record is a denormalized cache that may be stale -- not just
	// for ordinary moves (e.g. when the opponent made the last move and
	// couldn't update this repo's record) but for every terminal event
	// (checkmate, resignation, time violation, accepted draw offer): each
	// is written by whichever player triggered it, into THEIR OWN repo,
	// which may not be the repo that owns this app.atchess.game record. So
	// status is derived by merging candidate terminal events from every
	// source, each read across both repos, and taking whichever is most
	// recent (see terminalEvent/latestTerminalEvent). FEN always comes from
	// the latest move, since none of the other event types change the
	// board position.
	//
	// This is real network cost -- up to 4 sources x 2 repos = 8 XRPC calls
	// on top of the game record fetch above, half of them typically to a
	// remote PDS. It is deliberately paid on every call (including from the
	// move-submission path -- MakeMoveHandler must reject a move into an
	// already-terminal game, see atchess-1c9.48) rather than trusted from
	// this repo's own possibly-stale/forgeable "status" cache field. To
	// keep the wall-clock cost sane despite the round-trip count being
	// unavoidable for correctness, the three sources that don't depend on
	// each other's results (moves, resignation, accepted draws) run
	// concurrently; getTimeViolationOutcome needs the real last-move
	// timestamp from the move scan to verify a claim (see its doc comment),
	// so it runs once that finishes rather than being a fourth independent
	// branch.
	var latestMove *moveRecord
	var moveErr error
	var resignationEvent, drawAcceptEvent *terminalEvent
	var resignationErr, drawAcceptErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		latestMove, moveErr = c.getLatestMoveForGame(ctx, gameURI, game.White, game.Black)
	}()
	go func() {
		defer wg.Done()
		resignationEvent, resignationErr = c.getResignationOutcome(ctx, gameURI, game.White, game.Black)
	}()
	drawAcceptDone := make(chan struct{})
	go func() {
		defer close(drawAcceptDone)
		drawAcceptEvent, drawAcceptErr = c.getDrawAcceptOutcome(ctx, gameURI, game.White, game.Black)
	}()
	wg.Wait()

	var moveEvent *terminalEvent
	if moveErr == nil && latestMove != nil {
		game.FEN = latestMove.FEN
		if latestMove.Checkmate {
			fenParts := strings.Split(latestMove.FEN, " ")
			status := chess.StatusWhiteWon
			if len(fenParts) > 1 && fenParts[1] == "w" {
				status = chess.StatusBlackWon
			}
			moveEvent = &terminalEvent{status: status, at: latestMove.CreatedAt, rkey: latestMove.rkey, kind: terminalEventFromMove}
		} else if latestMove.Draw {
			moveEvent = &terminalEvent{status: chess.StatusDraw, at: latestMove.CreatedAt, rkey: latestMove.rkey, kind: terminalEventFromMove}
		}
	}

	// The real timestamp a timeViolation claim must be checked against:
	// the last actual move, or (if there have been none) the game's own
	// creation time.
	var lastActivityAt time.Time
	lastActivityKnown := false
	if latestMove != nil {
		lastActivityAt = latestMove.CreatedAt
		lastActivityKnown = true
	} else if t, err := time.Parse(time.RFC3339, getResp.Value.CreatedAt); err == nil {
		lastActivityAt = t
		lastActivityKnown = true
	}

	timeControlType := ""
	daysPerMove := 0
	if timeControl != nil {
		timeControlType = timeControl.Type
		daysPerMove = timeControl.DaysPerMove
	}
	// Resolve to the EFFECTIVE time control before asking
	// getTimeViolationOutcome to verify a claim against it -- see
	// resolveTimeControl's doc comment. Without this, an absent
	// timeControl here (timeControlType == "") disagreed with
	// CheckTimeViolation/ClaimTimeVictory, which already defaulted an
	// absent time control to correspondence/defaultDaysPerMove days, about
	// whether a timeout was even possible at all (atchess-1c9.88).
	timeControlType, daysPerMove = resolveTimeControl(timeControlType, daysPerMove)
	timeViolationEvent, timeViolationErr := c.getTimeViolationOutcome(ctx, gameURI, game.White, game.Black, timeControlType, daysPerMove, lastActivityAt, lastActivityKnown)

	<-drawAcceptDone

	if final := latestTerminalEvent(moveEvent, resignationEvent, timeViolationEvent, drawAcceptEvent); final != nil {
		game.Status = final.status
	}

	// Fail closed (see ErrIncompleteDerivation's doc comment): if ANY of the
	// four scans above could not read every repo, Status (and possibly FEN)
	// is unproven, not just "possibly stale". The partial *chess.Game is
	// still returned -- a caller that has deliberately opted into a
	// degraded read-only view could use it -- but every caller in this
	// codebase currently treats a non-nil error here as authoritative and
	// must reject any write it was about to authorize (atchess-1c9.51).
	if derivationErr := errors.Join(moveErr, resignationErr, timeViolationErr, drawAcceptErr); derivationErr != nil {
		game.DerivationIncomplete = true
		return game, fmt.Errorf("%w: %v", ErrIncompleteDerivation, derivationErr)
	}

	return game, nil
}

func (c *Client) GetHandle() string {
	return c.handle
}

func (c *Client) GetPDSURL() string {
	return c.pdsURL
}

// SetPLCDirectoryURL overrides the PLC directory this client's DID and
// (as a last resort) handle resolution uses in place of
// DefaultPLCDirectoryURL -- see config.ATProtoConfig.PLCDirectoryURL. Needed
// so the local dual-PDS test harness (which runs its own hermetic did:plc
// server, since its accounts' DIDs do not exist on the public
// https://plc.directory) can be pointed at that server instead. Must be
// called before any resolution-dependent call (GetGame, ResolveHandle,
// etc.); it is not safe to call concurrently with those.
func (c *Client) SetPLCDirectoryURL(plcDirectoryURL string) {
	c.plcDirectoryURL = plcDirectoryURL
}

// resolver returns the identityResolver this client uses for DID->PDS and
// PLC-export handle resolution, shared (by plcDirectoryURL) across every
// *Client pointed at the same directory -- see getIdentityResolver for why
// that sharing matters given how short-lived *Client instances are in
// internal/web.
func (c *Client) resolver() *identityResolver {
	return getIdentityResolver(c.plcDirectoryURL)
}

// resolveReadEndpoint returns the base PDS URL to read repoDID's records
// from: this client's own PDS when repoDID is empty or c.did, otherwise the
// PDS resolved from repoDID's DID document. ownRepo tells getXRPC whether it
// is safe to attach this client's own bearer token to the request -- see
// getXRPC.
func (c *Client) resolveReadEndpoint(ctx context.Context, repoDID string) (base string, ownRepo bool, err error) {
	if repoDID == "" || repoDID == c.did {
		return c.pdsURL, true, nil
	}
	endpoint, err := c.resolver().resolvePDS(ctx, repoDID)
	if err != nil {
		return "", false, fmt.Errorf("resolving PDS for repo %s: %w", repoDID, err)
	}
	return endpoint, false, nil
}

// getXRPC issues an (optionally query-parameterised) GET against method on
// base. When ownRepo is true, base is this client's own authenticated PDS:
// the request goes through makeRequest, attaching the Bearer/DPoP
// Authorization header (with its 401-triggered refresh-and-retry). When
// ownRepo is false, base is a DID-resolved, possibly-foreign PDS: this
// client's access token is NEVER sent there -- it is only valid at
// c.pdsURL, and sending it to a third-party origin would leak it -- so the
// request is unauthenticated, matching these com.atproto.repo.* /
// com.atproto.identity.* endpoints being public reads.
func (c *Client) getXRPC(ctx context.Context, base string, ownRepo bool, method string, params url.Values) (*http.Response, error) {
	u := xrpcURL(base, method, params)
	if ownRepo {
		return c.makeRequest("GET", u, nil)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	return c.httpClient.Do(req)
}

// listRecordsPageLimit is the page size requested from
// com.atproto.repo.listRecords. 100 is the PDS's own maximum page size;
// requesting it explicitly (rather than omitting "limit" and taking
// whatever default the PDS chooses) keeps page count, and therefore
// round-trip count, predictable.
const listRecordsPageLimit = "100"

// maxListRecordsPages bounds how many pages listAllRecords will fetch for
// a single repo/collection scan before failing closed (atchess-1c9.119).
// At 100 records/page (listRecordsPageLimit) this allows scanning up to
// 20,000 records per call. That is comfortably above any real player's
// per-collection record count today -- the bug this constant guards
// against was first observed in the hundreds of records -- while still
// giving every listAllRecords call a fixed, reviewable worst case instead
// of "however many records the repo happens to hold." A repo that
// legitimately exceeds this is treated as an error condition to raise
// the cap deliberately, not as a truncation to paper over silently: see
// listAllRecords's doc comment for why silent truncation is exactly the
// bug this exists to prevent.
//
// A var, not a const, solely so tests can shrink it (see
// TestListAllRecords_PageCapEnforced) to exercise the cap-exceeded path
// without needing to seed the tens of thousands of records the real
// default requires. Production code never assigns to this.
var maxListRecordsPages = 200

// errListRecordsPageCapExceeded is returned by listAllRecords when a
// repo/collection scan is not exhausted within maxListRecordsPages pages.
var errListRecordsPageCapExceeded = errors.New("listRecords: page cap exceeded")

// pdsListRecord is the per-record envelope shared by every
// com.atproto.repo.listRecords response, with Value left as raw JSON so
// each caller can decode it into its own collection-specific struct.
type pdsListRecord struct {
	URI   string          `json:"uri"`
	CID   string          `json:"cid"`
	Value json.RawMessage `json:"value"`
}

// listAllRecords fully paginates com.atproto.repo.listRecords for
// repo/collection against base, following the response's cursor until
// the collection is exhausted, and returns every record found across all
// pages.
//
// THIS IS THE SHARED PAGINATION HELPER for atchess-1c9.119: every call
// site in this package that lists a collection and then reasons about
// "the latest" record or "does a matching record exist" MUST route
// through this function rather than issuing its own single-page
// listRecords call. com.atproto.repo.listRecords caps each response at
// listRecordsPageLimit records; a caller that reads only the first page
// and stops is silently blind to everything past it.
//
// Do not assume page order correlates with recency for ANY collection
// this package reads. As of atchess-1c9.113, app.atchess.move record
// keys are deterministic hashes of (gameURI, ply) rather than
// PDS-assigned TIDs, so listRecords order carries no chronological
// information at all for that collection -- "read the newest page and
// stop" is not a valid shortcut there even in principle. The other
// collections this package reads (app.atchess.resignation,
// app.atchess.timeViolation, app.atchess.drawResponse,
// app.atchess.challengeResponse, app.atchess.drawOffer) still use
// PDS-assigned TID rkeys, but atchess-1c9.100 already established that a
// TID minted by one repo's PDS is not a trustworthy recency signal
// against a TID minted by ANOTHER repo's PDS either -- and every one of
// these scans reads across (or targets) repos whose TID clocks are not
// mutually ordered. So for every collection here, full pagination is the
// only correct option; there is no page to stop early on.
//
// Fails closed: if the collection is not exhausted within
// maxListRecordsPages pages, this returns errListRecordsPageCapExceeded
// wrapped with context, instead of silently returning the partial result
// gathered so far. A truncated scan is exactly the atchess-1c9.119 bug --
// a caller reasoning about "the latest" or "does X exist" over a partial
// view can silently reach the wrong answer -- so raising the cap only
// moves the cliff; it must never be papered over by truncating quietly.
func (c *Client) listAllRecords(ctx context.Context, base string, ownRepo bool, repo, collection string) ([]pdsListRecord, error) {
	var all []pdsListRecord
	cursor := ""
	for pageNum := 0; ; pageNum++ {
		if pageNum >= maxListRecordsPages {
			return nil, fmt.Errorf("%w: repo=%s collection=%s after %d pages (%d records read)",
				errListRecordsPageCapExceeded, repo, collection, maxListRecordsPages, len(all))
		}

		params := url.Values{"repo": {repo}, "collection": {collection}, "limit": {listRecordsPageLimit}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
		if err != nil {
			return nil, fmt.Errorf("list %s for %s: %w", collection, repo, err)
		}

		var page struct {
			Records []pdsListRecord `json:"records"`
			Cursor  string          `json:"cursor"`
		}
		decodeErr := func() error {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("list %s for %s: HTTP %d: %s", collection, repo, resp.StatusCode, string(body))
			}
			return json.NewDecoder(resp.Body).Decode(&page)
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}

		all = append(all, page.Records...)

		// No cursor means the PDS has no further pages. An empty page
		// with a cursor still present is treated as "keep following it"
		// rather than "done", since a PDS is free to return a page
		// narrower than the requested limit for reasons unrelated to
		// exhaustion (e.g. a page's records straddling an internal
		// boundary) -- only an EMPTY cursor is the documented
		// end-of-collection signal.
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	return all, nil
}

// ResolveHandle resolves a handle to a DID. A handle can be hosted on any
// PDS, not necessarily this client's own (c.pdsURL) -- see atchess-1c9.10 --
// so this tries, in order:
//  1. this client's own PDS's com.atproto.identity.resolveHandle (fast path
//     for handles it hosts locally; also the only path that works against a
//     PDS with no real public DNS at all, e.g. a fully offline dev setup);
//  2. the standard AT Protocol DNS TXT record (_atproto.<handle>);
//  3. the standard AT Protocol HTTPS well-known endpoint
//     (https://<handle>/.well-known/atproto-did);
//  4. a bounded PLC-export scan (see resolveHandleViaPLCExport), as a last
//     resort for handles that are real accounts but not DNS-resolvable at
//     all -- notably the local dual-PDS test harness's ".test" handles
//     (RFC 2606: permanently reserved, never publicly resolvable).
//
// Returns an error naming the handle and every resolver tried, with each
// one's own failure reason, if all of them fail.
func (c *Client) ResolveHandle(ctx context.Context, handle string) (string, error) {
	// If it's already a DID, return it
	if strings.HasPrefix(handle, "did:") {
		return handle, nil
	}

	// Validate (and normalize to lowercase) BEFORE any resolution strategy
	// runs -- see normalizeAndValidateHandle's doc comment. This is the
	// single choke point every strategy below shares, so none of them ever
	// builds a URL/hostname/DNS query from an unvalidated handle
	// (atchess-1c9.69).
	validated, err := normalizeAndValidateHandle(handle)
	if err != nil {
		return "", err
	}
	handle = validated

	var attempts []string

	if did, err := c.resolveHandleSamePDS(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("same-PDS resolveHandle (%s): %v", c.pdsURL, err))
	}

	if did, err := resolveHandleViaDNS(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("DNS TXT: %v", err))
	}

	// Deliberately c.resolver().httpClient here, NOT c.httpClient: this
	// fetch's target host is the caller-supplied handle itself (an
	// attacker-named domain, not this client's own already-resolved PDS),
	// so it must go through the SAME redirect-refusing client every other
	// identity-resolution fetch uses (see refuseIdentityFetchRedirect's doc
	// comment, atchess-1c9.94) rather than the general-purpose XRPC client,
	// which follows redirects normally and is used only against
	// already-resolved PDS endpoints.
	if did, err := resolveHandleViaWellKnown(ctx, c.resolver().httpClient, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("HTTPS well-known: %v", err))
	}

	if did, err := c.resolver().resolveHandleViaPLCExport(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("PLC export: %v", err))
	}

	return "", fmt.Errorf("failed to resolve handle %q via any resolver: %s", handle, strings.Join(attempts, "; "))
}

// resolveHandleSamePDS asks this client's own PDS to resolve handle via
// com.atproto.identity.resolveHandle. This only succeeds for handles that
// PDS itself hosts (or otherwise already knows how to resolve); a handle
// hosted elsewhere returns an XRPC InvalidRequest error, which the caller
// (ResolveHandle) treats as "try the next resolver", not a hard failure.
func (c *Client) resolveHandleSamePDS(ctx context.Context, handle string) (string, error) {
	params := url.Values{"handle": {handle}}
	resp, err := c.getXRPC(ctx, c.pdsURL, true, "com.atproto.identity.resolveHandle", params)
	if err != nil {
		return "", fmt.Errorf("failed to resolve handle: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.DID == "" {
		return "", fmt.Errorf("response had no did")
	}

	return result.DID, nil
}

// currentGameStatus returns gameURI's authoritative, derived status (see
// GetGame's doc comment) -- never the raw, possibly-stale-or-forged
// "status" field cached on the app.atchess.game record itself. On error it
// returns ("", err); the error may wrap ErrIncompleteDerivation (one or
// more repos could not be read while scanning for terminal events). Every
// caller of this method uses the result to decide whether to AUTHORIZE a
// write (resign, offer/accept a draw, claim a time violation), so callers
// MUST fail closed on a non-nil error -- i.e. reject the write -- rather
// than treating "could not verify" the same as "verified active". Treating
// an unreadable repo as equivalent to an active game is exactly the bug
// atchess-1c9.51 fixed: an opponent-PDS outage must not silently reopen a
// game that has already ended. Do not restore the old "err == nil &&
// status != active" fail-open pattern here.
func (c *Client) currentGameStatus(ctx context.Context, gameURI string) (chess.GameStatus, error) {
	g, err := c.GetGame(ctx, gameURI)
	if err != nil {
		return "", err
	}
	return g.Status, nil
}

// currentGameStatusAndFEN returns gameURI's authoritative, derived status
// AND FEN together (see GetGame's doc comment) -- never the raw,
// possibly-stale "fen"/"status" fields cached on the app.atchess.game
// record itself. The cached "fen" is stale after every move made by the
// player who does not own the game record, since RecordMove
// (client.go:820) only refreshes it when the mover owns the record;
// GetGame already compensates for this by deriving FEN from the latest
// move record (or, if no move records exist yet, correctly falling back
// to the cached FEN, since nobody has moved). CheckTimeViolation used to
// read gameValue["fen"] raw despite already calling currentGameStatus,
// which pays for this exact derivation and discards the FEN -- this
// function reuses that same derivation to get both values for the price
// of one, instead of triggering a second, independent
// getLatestMoveForGame call for data GetGame already computed
// (atchess-1c9.103). Like currentGameStatus, it fails closed: on error
// (which may wrap ErrIncompleteDerivation) callers MUST reject whatever
// write they were about to authorize rather than guessing from unverified
// data -- see currentGameStatus's doc comment.
func (c *Client) currentGameStatusAndFEN(ctx context.Context, gameURI string) (chess.GameStatus, string, error) {
	g, err := c.GetGame(ctx, gameURI)
	if err != nil {
		return "", "", err
	}
	return g.Status, g.FEN, nil
}

// OfferDraw creates a draw offer record for a game
func (c *Client) OfferDraw(ctx context.Context, gameID string, message string) (*DrawOffer, error) {
	// First, fetch the game record to get its CID
	gameCID, _, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}

	// Verify the game is active. Uses the derived status (GetGame), not the
	// raw cached gameValue["status"] field, so this is consistent with
	// terminal events the game's own repo owner may not know about yet
	// (atchess-1c9.48 review). Fail closed if the status could not be
	// verified at all (atchess-1c9.51) -- an unreadable repo must not be
	// treated as "active".
	status, err := c.currentGameStatus(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("cannot verify game is still active: %w", err)
	}
	if status != chess.StatusActive {
		return nil, fmt.Errorf("cannot offer draw in a game with status: %s", status)
	}

	// Create draw offer record
	drawOfferRecord := map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"offeredBy": c.did,
		"status":    "pending",
	}

	// Add optional message
	if message != "" {
		drawOfferRecord["message"] = message
	}

	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.drawOffer",
		"record":     drawOfferRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create draw offer record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create draw offer record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &DrawOffer{
		URI:       createResp.URI,
		CID:       createResp.CID,
		CreatedAt: drawOfferRecord["createdAt"].(string),
		GameURI:   gameID,
		GameCID:   gameCID,
		OfferedBy: c.did,
		Message:   message,
		Status:    "pending",
	}, nil
}

// RespondToDrawOffer accepts or declines a draw offer
func (c *Client) RespondToDrawOffer(ctx context.Context, drawOfferURI string, accept bool) error {
	// Parse the draw offer URI to extract repo and rkey
	parts := strings.Split(drawOfferURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(drawOfferURI, "at://") {
		return fmt.Errorf("invalid draw offer URI format: %s", drawOfferURI)
	}

	repo := parts[2] // The DID
	rkey := parts[4] // The record key

	// Get the draw offer record
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return fmt.Errorf("failed to get draw offer record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.drawOffer"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return fmt.Errorf("failed to get draw offer record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get draw offer record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var getResp struct {
		URI   string                 `json:"uri"`
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	// Verify the draw offer is still pending
	if status, ok := getResp.Value["status"].(string); ok && status != "pending" {
		return fmt.Errorf("draw offer is not pending, current status: %s", status)
	}

	// Get the game reference
	gameRef, ok := getResp.Value["game"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid game reference in draw offer")
	}
	gameURI, ok := gameRef["uri"].(string)
	if !ok {
		return fmt.Errorf("missing game URI in draw offer")
	}
	// Fallback game CID already embedded in the offer's own strongRef --
	// possibly stale by the time of this response (more moves may have
	// happened since), but good enough for a decline (see below).
	offerGameCID, _ := gameRef["cid"].(string)

	// Verify the game hasn't already reached a terminal state by some
	// OTHER event (e.g. the opponent resigned, or the game timed out)
	// since this offer was made. Unlike OfferDraw/ResignGame/
	// CheckTimeViolation, this check used to be missing entirely here --
	// RespondToDrawOffer only ever looked at the offer record's own
	// "status" field, never the derived game status -- so a draw could be
	// accepted into a game that had already ended, producing two
	// competing terminal events (atchess-1c9.56). Uses the derived status
	// (GetGame), not the raw cached game record status -- see OfferDraw's
	// comment (atchess-1c9.48 review). Fail closed if the status could
	// not be verified at all (atchess-1c9.51).
	if status, statusErr := c.currentGameStatus(ctx, gameURI); statusErr != nil {
		return fmt.Errorf("cannot verify game is still active: %w", statusErr)
	} else if status != chess.StatusActive {
		return fmt.Errorf("cannot respond to draw offer in a game with status: %s", status)
	}

	// Record the response. AT Protocol never permits writing into another
	// account's repository, and the draw offer record (drawOfferURI) lives
	// in the OFFERING player's repo -- which, for the ordinary case of the
	// OTHER player responding, is not c.did. Mutating it via putRecord (as
	// this used to do) always failed in real federation with HTTP 403
	// AccountNotFound, exactly like the retired cross-repo challenge
	// notification write (atchess-1c9.11). Instead, write an
	// app.atchess.drawResponse record into the CALLER's OWN repo,
	// referencing both the offer and the game by strongRef. GetGame
	// derives "draw" status by reading these across both players' repos
	// (getDrawAcceptOutcome), the same pattern already used for moves.
	response := "accepted"
	if !accept {
		response = "declined"
	}

	// An accept is a meaningful state transition, so fetch the freshest
	// possible game CID and fail loudly if that's not possible. A decline
	// is comparatively low-stakes -- it never updates the game record
	// (below) -- so it must not hard-fail just because the game record
	// happens to be momentarily unreadable; fall back to the CID already
	// recorded on the offer itself instead (atchess-1c9.48 review: this
	// getGameRecord call used to be accept-only, and unconditionally
	// requiring it here regressed declines to hard-fail on the same
	// condition an accept does).
	var gameCID string
	if accept {
		gameCID, _, err = c.getGameRecord(ctx, gameURI)
		if err != nil {
			return fmt.Errorf("failed to get game record for draw response: %w", err)
		}
	} else if fresh, _, err := c.getGameRecord(ctx, gameURI); err == nil {
		gameCID = fresh
	} else {
		gameCID = offerGameCID
	}

	drawResponseRecord := map[string]interface{}{
		"$type":     "app.atchess.drawResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"drawOffer": map[string]interface{}{
			"uri": getResp.URI,
			"cid": getResp.CID,
		},
		"game": map[string]interface{}{
			"uri": gameURI,
			"cid": gameCID,
		},
		"respondedBy": c.did,
		"response":    response,
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.drawResponse",
		"record":     drawResponseRecord,
	}

	createReqBody, _ := json.Marshal(createReq)
	createResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), createReqBody)
	if err != nil {
		return fmt.Errorf("failed to create draw response record: %w", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createResp.Body)
		return fmt.Errorf("failed to create draw response record: HTTP %d - %s", createResp.StatusCode, string(body))
	}

	// Best-effort cache refresh: if this caller happens to also own the
	// shared app.atchess.game record (repo == c.did), update its status
	// field too. This is never required for correctness -- GetGame always
	// derives the authoritative status from drawResponse records across
	// both repos regardless of who owns the game record -- but keeps the
	// cached record from looking obviously stale to any reader that
	// doesn't go through derivation.
	if accept {
		gameParts := strings.Split(gameURI, "/")
		if len(gameParts) >= 5 && gameParts[2] == c.did {
			// swapRecordCASContract: this is doubly unaffected by the
			// identical-value no-op -- the response isn't even inspected
			// (see comment above), and this write is a best-effort cache
			// refresh GetGame never depends on anyway.
			gCID, gameValue, err := c.getGameRecord(ctx, gameURI)
			if err == nil {
				gameValue["status"] = "draw"
				gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
				gameRkey := gameParts[4]
				updateGameReq := map[string]interface{}{
					"repo":       c.did,
					"collection": "app.atchess.game",
					"rkey":       gameRkey,
					"record":     gameValue,
					"swapRecord": gCID,
				}
				updateGameReqBody, _ := json.Marshal(updateGameReq)
				if updateGameResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateGameReqBody); err == nil {
					updateGameResp.Body.Close()
				}
			}
		}
	}

	return nil
}

// ResignGame creates a resignation record and updates the game status
func (c *Client) ResignGame(ctx context.Context, gameID string, reason string) error {
	// First, fetch the game record to get its CID and current state
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}

	// Verify the game is active. Uses the derived status (GetGame), not the
	// raw cached gameValue["status"] field -- see OfferDraw's comment
	// (atchess-1c9.48 review). Fail closed if the status could not be
	// verified at all (atchess-1c9.51).
	if status, statusErr := c.currentGameStatus(ctx, gameID); statusErr != nil {
		return fmt.Errorf("cannot verify game is still active: %w", statusErr)
	} else if status != chess.StatusActive {
		return fmt.Errorf("cannot resign from a game with status: %s", status)
	}

	// Determine who won based on who is resigning
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)

	var newStatus string
	if c.did == whiteDID {
		newStatus = "black_won"
	} else if c.did == blackDID {
		newStatus = "white_won"
	} else {
		return fmt.Errorf("player is not part of this game")
	}

	// Create resignation record
	resignationRecord := map[string]interface{}{
		"$type":     "app.atchess.resignation",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"resigningPlayer": c.did,
	}

	// Add optional reason
	if reason != "" {
		resignationRecord["reason"] = reason
	}

	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.resignation",
		"record":     resignationRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create resignation record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create resignation record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	// Update the game status if we own the game record
	parts := strings.Split(gameID, "/")
	if len(parts) >= 5 && parts[2] == c.did {
		gameValue["status"] = newStatus
		gameValue["updatedAt"] = time.Now().Format(time.RFC3339)

		// Update the game record
		//
		// swapRecordCASContract: safe against the identical-value no-op --
		// newStatus is only reached from an active game (checked above),
		// so this write always changes "status" away from "active"; two
		// racing resignations by the SAME player land in the no-op case
		// only when they'd write the SAME final status anyway, and a
		// racing DIFFERENT status (e.g. the other player also resigning)
		// still differs and gets the swap fully evaluated below.
		rkey := parts[4]
		updateReq := map[string]interface{}{
			"repo":       c.did,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapRecord": gameCID,
		}

		updateReqBody, _ := json.Marshal(updateReq)
		updateResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer updateResp.Body.Close()

		if updateResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(updateResp.Body)
			return fmt.Errorf("failed to update game record: HTTP %d - %s", updateResp.StatusCode, string(body))
		}
	}

	return nil
}

// GetDrawOffers retrieves pending draw offers for a game
func (c *Client) GetDrawOffers(ctx context.Context, gameID string) ([]*DrawOffer, error) {
	// List draw offer records
	records, err := c.listAllRecords(ctx, c.pdsURL, true, c.did, "app.atchess.drawOffer")
	if err != nil {
		return nil, fmt.Errorf("failed to list draw offers: %w", err)
	}

	// Filter for the specific game and pending status
	var offers []*DrawOffer
	for _, record := range records {
		var value struct {
			Type      string `json:"$type"`
			CreatedAt string `json:"createdAt"`
			Game      struct {
				URI string `json:"uri"`
				CID string `json:"cid"`
			} `json:"game"`
			OfferedBy   string `json:"offeredBy"`
			MoveNumber  int    `json:"moveNumber"`
			Message     string `json:"message"`
			Status      string `json:"status"`
			RespondedAt string `json:"respondedAt"`
			RespondedBy string `json:"respondedBy"`
		}
		if err := json.Unmarshal(record.Value, &value); err != nil {
			// atchess-1c9.119 fix-pass: skip-and-log -- see
			// ListMovesForGame's comment for the full rationale.
			log.Warn().Str("repo", c.did).Str("collection", "app.atchess.drawOffer").Str("recordURI", record.URI).Err(err).
				Msg("skipping malformed drawOffer record: value did not decode into the expected shape")
			continue
		}
		if value.Game.URI == gameID && value.Status == "pending" {
			offer := &DrawOffer{
				URI:         record.URI,
				CID:         record.CID,
				CreatedAt:   value.CreatedAt,
				GameURI:     value.Game.URI,
				GameCID:     value.Game.CID,
				OfferedBy:   value.OfferedBy,
				MoveNumber:  value.MoveNumber,
				Message:     value.Message,
				Status:      value.Status,
				RespondedAt: value.RespondedAt,
				RespondedBy: value.RespondedBy,
			}
			offers = append(offers, offer)
		}
	}

	return offers, nil
}

// DrawOffer represents a draw offer record
type DrawOffer struct {
	URI         string
	CID         string
	CreatedAt   string
	GameURI     string
	GameCID     string
	OfferedBy   string
	MoveNumber  int
	Message     string
	Status      string
	RespondedAt string
	RespondedBy string
}

// TimeViolation represents a time violation claim record
type TimeViolation struct {
	URI               string
	CID               string
	CreatedAt         string
	GameURI           string
	GameCID           string
	ClaimingPlayer    string
	ViolatingPlayer   string
	LastMoveTimestamp string
	TimeControlType   string
	DaysPerMove       int
	TimeRemaining     int
}

// CheckTimeViolation checks if the current player has violated time control in a game
func (c *Client) CheckTimeViolation(ctx context.Context, gameID string) (bool, *TimeViolation, error) {
	// Get the game record to check status and players
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get game record: %w", err)
	}

	// Check if game is still active, AND derive the current FEN, together.
	// Both use the derived values (GetGame), not the raw cached
	// gameValue["status"]/gameValue["fen"] fields -- see OfferDraw's
	// comment (atchess-1c9.48 review) for status, and
	// currentGameStatusAndFEN's doc comment (atchess-1c9.103) for FEN: the
	// cached fen is one ply stale after every move by the player who
	// doesn't own the game record, which used to name the WRONG player as
	// the one to move (and therefore the one exclude'd from getLastMove
	// below, making the false-violation window LONGER, not shorter). Fail
	// closed if either could not be verified at all (atchess-1c9.51): this
	// result also gates ClaimTimeVictory's write, so "could not verify"
	// must not be treated as "still active" or guessed at.
	status, derivedFEN, statusErr := c.currentGameStatusAndFEN(ctx, gameID)
	if statusErr != nil {
		return false, nil, fmt.Errorf("cannot verify game is still active: %w", statusErr)
	}
	if status != chess.StatusActive {
		return false, nil, nil // Game is not active, no time violation possible
	}

	// Get players
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)

	// Determine whose turn it is from the derived FEN (not the raw cached
	// gameValue["fen"] -- see above).
	fenParts := strings.Split(derivedFEN, " ")
	if len(fenParts) < 2 {
		return false, nil, fmt.Errorf("invalid FEN format")
	}

	var currentPlayerDID string
	if fenParts[1] == "w" {
		currentPlayerDID = whiteDID
	} else {
		currentPlayerDID = blackDID
	}

	// Get the challenge reference to access time control settings
	var timeControlType string
	var daysPerMove int

	if challengeRef, ok := gameValue["challenge"].(map[string]interface{}); ok {
		challengeURI, _ := challengeRef["uri"].(string)
		if challengeURI != "" {
			// Get the challenge record to access time control
			challengeParts := strings.Split(challengeURI, "/")
			if len(challengeParts) >= 5 {
				challengeRepo := challengeParts[2]
				challengeRkey := challengeParts[4]

				base, ownRepo, resolveErr := c.resolveReadEndpoint(ctx, challengeRepo)
				var resp *http.Response
				var err error
				if resolveErr == nil {
					params := url.Values{"repo": {challengeRepo}, "collection": {"app.atchess.challenge"}, "rkey": {challengeRkey}}
					resp, err = c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
				} else {
					err = resolveErr
				}
				if err == nil && resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()

					var challengeResp struct {
						Value struct {
							TimeControl map[string]interface{} `json:"timeControl"`
						} `json:"value"`
					}

					if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err == nil {
						if tc := challengeResp.Value.TimeControl; tc != nil {
							if tcType, ok := tc["type"].(string); ok {
								timeControlType = tcType
							}
							if days, ok := tc["daysPerMove"].(float64); ok {
								daysPerMove = int(days)
							}
						}
					}
				}
			}
		}
	}

	// Resolve to the EFFECTIVE time control -- see resolveTimeControl's
	// doc comment. This is the single place the "absent means
	// correspondence/defaultDaysPerMove days" policy is decided, shared
	// with getTimeViolationOutcome via GetGame (atchess-1c9.88): without
	// going through the same function, this could silently drift from
	// what GetGame's derived status considers a valid timeout again.
	timeControlType, daysPerMove = resolveTimeControl(timeControlType, daysPerMove)

	// For correspondence games, check the last move timestamp
	if timeControlType == "correspondence" {
		// Get the most recent move
		lastMove, err := c.getLastMove(ctx, gameID, currentPlayerDID)
		if err != nil {
			return false, nil, fmt.Errorf("failed to get last move: %w", err)
		}

		// If no moves yet, use game creation time
		var lastMoveTime time.Time
		if lastMove != nil {
			lastMoveTime, err = time.Parse(time.RFC3339, lastMove.CreatedAt)
			if err != nil {
				return false, nil, fmt.Errorf("failed to parse move timestamp: %w", err)
			}
		} else {
			// Use game creation time
			if createdAt, ok := gameValue["createdAt"].(string); ok {
				lastMoveTime, err = time.Parse(time.RFC3339, createdAt)
				if err != nil {
					return false, nil, fmt.Errorf("failed to parse game creation timestamp: %w", err)
				}
			} else {
				return false, nil, fmt.Errorf("game missing createdAt timestamp")
			}
		}

		// Check if time has expired
		timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
		if time.Since(lastMoveTime) > timeLimit {
			// Time violation detected
			violation := &TimeViolation{
				GameURI:           gameID,
				GameCID:           gameCID,
				ClaimingPlayer:    c.did,
				ViolatingPlayer:   currentPlayerDID,
				LastMoveTimestamp: lastMoveTime.Format(time.RFC3339),
				TimeControlType:   timeControlType,
				DaysPerMove:       daysPerMove,
			}
			return true, violation, nil
		}
	}

	// TODO: Implement for other time control types (rapid, blitz, bullet)
	// These would require tracking time remaining per player

	return false, nil, nil
}

// getLastMove retrieves the most recent move in a game, excluding moves by
// excludePlayerDID. Ordering uses moveRecordIsAfter (atchess-1c9.100/.102):
// a bare CreatedAt comparison is only second-resolution and, worse, had no
// tiebreak at all here, so two moves recorded in the same second could
// order arbitrarily -- which fed CheckTimeViolation's attribution of WHICH
// player violated the time control. moveRecordIsAfter orders by the ply
// derived from each record's own FEN instead, which is domain-correct and
// cross-repo-comparable; see its doc comment for why that is sound where a
// bare timestamp/TID tiebreak was not. FEN and rkey are read here only to
// feed that comparison -- callers still only read CreatedAt/Player.
func (c *Client) getLastMove(ctx context.Context, gameID string, excludePlayerDID string) (*struct {
	CreatedAt string
	Player    string
	FEN       string
	rkey      string
}, error) {
	// List moves for both players
	players := []string{}

	// Parse game URI to get players
	gameParts := strings.Split(gameID, "/")
	if len(gameParts) >= 5 {
		gameRepo := gameParts[2]
		players = append(players, gameRepo)
	}

	// Get game record to find the other player
	_, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return nil, err
	}

	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)

	// Add the other player if different from repo owner
	if whiteDID != players[0] {
		players = append(players, whiteDID)
	}
	if blackDID != players[0] && blackDID != whiteDID {
		players = append(players, blackDID)
	}

	var lastMove *struct {
		CreatedAt string
		Player    string
		FEN       string
		rkey      string
	}
	var lastMoveTime time.Time
	var errs []error

	// Check moves from all players. atchess-1c9.119 fix-pass: a per-player
	// read failure here used to be silently skipped ("continue"), which
	// made this function indistinguishable from "this player genuinely has
	// no moves" -- and BOTH of this function's callers (CheckTimeViolation,
	// GetTimeRemaining) treat a nil lastMove as "use the game's createdAt
	// as last-activity". A skipped repo (e.g. one that now legitimately
	// trips listAllRecords's page cap, or is merely unreachable) could
	// therefore silently fall back to a stale createdAt timestamp and
	// fabricate a time-violation forfeit against a player whose real,
	// unreadable last move would have reset the clock. This mirrors
	// getLatestMoveForGame's fail-closed contract instead: every read
	// failure is accumulated and returned via errors.Join, and BOTH
	// callers already check this function's error return before trusting
	// a nil lastMove (see CheckTimeViolation/GetTimeRemaining), so no
	// caller-side change was needed to make them fail closed too.
	for _, playerDID := range players {
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		records, err := c.listAllRecords(ctx, base, ownRepo, playerDID, "app.atchess.move")
		if err != nil {
			errs = append(errs, fmt.Errorf("list moves for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		// Find the most recent move for this game
		for _, record := range records {
			var value struct {
				CreatedAt string `json:"createdAt"`
				Game      struct {
					URI string `json:"uri"`
				} `json:"game"`
				Player string `json:"player"`
				FEN    string `json:"fen"`
			}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				// atchess-1c9.119 fix-pass: skip-and-log -- see
				// ListMovesForGame's comment for the full rationale.
				log.Warn().Str("repo", playerDID).Str("collection", "app.atchess.move").Str("recordURI", record.URI).Err(err).
					Msg("skipping malformed move record: value did not decode into the expected shape")
				continue
			}
			if value.Game.URI != gameID {
				continue
			}
			if value.Player != playerDID {
				log.Warn().Str("gameURI", gameID).Str("repo", playerDID).
					Str("claimedPlayer", value.Player).
					Str("recordURI", record.URI).
					Msg("ignoring forged move record: repo owner is not the player it names as mover")
				continue
			}
			if value.Player != excludePlayerDID {
				moveTime, err := time.Parse(time.RFC3339, value.CreatedAt)
				if err != nil {
					continue
				}
				rkey := recordKey(record.URI)

				if lastMove == nil || moveRecordIsAfter(value.FEN, moveTime, rkey, lastMove.FEN, lastMoveTime, lastMove.rkey) {
					lastMoveTime = moveTime
					lastMove = &struct {
						CreatedAt string
						Player    string
						FEN       string
						rkey      string
					}{
						CreatedAt: value.CreatedAt,
						Player:    value.Player,
						FEN:       value.FEN,
						rkey:      rkey,
					}
				}
			}
		}
	}

	return lastMove, errors.Join(errs...)
}

// ClaimTimeVictory claims victory due to opponent's time violation
func (c *Client) ClaimTimeVictory(ctx context.Context, gameID string) error {
	// First check if there's actually a time violation
	hasViolation, violation, err := c.CheckTimeViolation(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to check time violation: %w", err)
	}

	if !hasViolation {
		return fmt.Errorf("no time violation detected")
	}

	// Get the game record
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}

	// Verify the claiming player is part of the game
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)

	if c.did != whiteDID && c.did != blackDID {
		return fmt.Errorf("you are not a player in this game")
	}

	// Create time violation record
	violationRecord := map[string]interface{}{
		"$type":     "app.atchess.timeViolation",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"claimingPlayer":    violation.ClaimingPlayer,
		"violatingPlayer":   violation.ViolatingPlayer,
		"lastMoveTimestamp": violation.LastMoveTimestamp,
		"timeControlType":   violation.TimeControlType,
	}

	if violation.DaysPerMove > 0 {
		violationRecord["daysPerMove"] = violation.DaysPerMove
	}
	if violation.TimeRemaining > 0 {
		violationRecord["timeRemaining"] = violation.TimeRemaining
	}

	// Create the violation record
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.timeViolation",
		"record":     violationRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create time violation record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create time violation record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	// Update game status if we own the game record
	parts := strings.Split(gameID, "/")
	if len(parts) >= 5 && parts[2] == c.did {
		// Determine winner (the player who didn't violate time)
		var newStatus string
		if violation.ViolatingPlayer == whiteDID {
			newStatus = "black_won"
		} else {
			newStatus = "white_won"
		}

		gameValue["status"] = newStatus
		gameValue["updatedAt"] = time.Now().Format(time.RFC3339)

		// Update the game record
		//
		// swapRecordCASContract: same reasoning as ResignGame's matching
		// update above -- newStatus always moves the record away from
		// "active", so an identical-value no-op here only occurs when a
		// racing write would have produced the SAME status anyway; a
		// genuinely different racing outcome still differs and gets the
		// swap fully evaluated.
		rkey := parts[4]
		updateReq := map[string]interface{}{
			"repo":       c.did,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapRecord": gameCID,
		}

		updateReqBody, _ := json.Marshal(updateReq)
		updateResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer updateResp.Body.Close()

		if updateResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(updateResp.Body)
			return fmt.Errorf("failed to update game record: HTTP %d - %s", updateResp.StatusCode, string(body))
		}
	}

	return nil
}

// GetTimeRemaining calculates time remaining for the current player in a game
func (c *Client) GetTimeRemaining(ctx context.Context, gameID string) (time.Duration, error) {
	// Get the game record
	_, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return 0, fmt.Errorf("failed to get game record: %w", err)
	}

	// Check if game is still active
	if status, ok := gameValue["status"].(string); ok && status != "active" {
		return 0, fmt.Errorf("game is not active")
	}

	// Get players
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)

	// Determine whose turn it is from the derived FEN, not the raw cached
	// gameValue["fen"] field -- that cache is one ply stale after every
	// move by the player who doesn't own the game record (see
	// currentGameStatusAndFEN's doc comment, atchess-1c9.103). This reuses
	// the same derivation path as GetGame/CheckTimeViolation
	// (getLatestMoveForGame) rather than inventing a third way to work out
	// the current position. If no move records exist yet, the cached FEN
	// is correct (nobody has moved) and is used as-is. If derivation
	// itself fails (e.g. the opponent's PDS is unreachable), fail closed
	// rather than falling back to the possibly-stale cached FEN: unlike
	// CheckTimeViolation this doesn't already pay for a GetGame call, so
	// this does add network round trips (up to one listRecords call per
	// player) to a function that previously made none for this purpose.
	cachedFEN, _ := gameValue["fen"].(string)
	latestMove, moveErr := c.getLatestMoveForGame(ctx, gameID, whiteDID, blackDID)
	if moveErr != nil {
		return 0, fmt.Errorf("cannot verify current position: %w", moveErr)
	}
	fen := cachedFEN
	if latestMove != nil {
		fen = latestMove.FEN
	}
	fenParts := strings.Split(fen, " ")
	if len(fenParts) < 2 {
		return 0, fmt.Errorf("invalid FEN format")
	}

	var currentPlayerDID string
	if fenParts[1] == "w" {
		currentPlayerDID = whiteDID
	} else {
		currentPlayerDID = blackDID
	}

	// Get time control settings from challenge
	var timeControlType string
	var daysPerMove int

	if challengeRef, ok := gameValue["challenge"].(map[string]interface{}); ok {
		challengeURI, _ := challengeRef["uri"].(string)
		if challengeURI != "" {
			challengeParts := strings.Split(challengeURI, "/")
			if len(challengeParts) >= 5 {
				challengeRepo := challengeParts[2]
				challengeRkey := challengeParts[4]

				base, ownRepo, resolveErr := c.resolveReadEndpoint(ctx, challengeRepo)
				var resp *http.Response
				var err error
				if resolveErr == nil {
					params := url.Values{"repo": {challengeRepo}, "collection": {"app.atchess.challenge"}, "rkey": {challengeRkey}}
					resp, err = c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
				} else {
					err = resolveErr
				}
				if err == nil && resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()

					var challengeResp struct {
						Value struct {
							TimeControl map[string]interface{} `json:"timeControl"`
						} `json:"value"`
					}

					if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err == nil {
						if tc := challengeResp.Value.TimeControl; tc != nil {
							if tcType, ok := tc["type"].(string); ok {
								timeControlType = tcType
							}
							if days, ok := tc["daysPerMove"].(float64); ok {
								daysPerMove = int(days)
							}
						}
					}
				}
			}
		}
	}

	// Resolve to the EFFECTIVE time control -- see resolveTimeControl's
	// doc comment (atchess-1c9.88): the same single default used by
	// CheckTimeViolation/GetGame, so this display-only calculation can
	// never quietly drift from what actually governs the game.
	timeControlType, daysPerMove = resolveTimeControl(timeControlType, daysPerMove)

	// For correspondence games, calculate time remaining
	if timeControlType == "correspondence" {
		// Get the most recent move
		lastMove, err := c.getLastMove(ctx, gameID, currentPlayerDID)
		if err != nil {
			return 0, fmt.Errorf("failed to get last move: %w", err)
		}

		var lastMoveTime time.Time
		if lastMove != nil {
			lastMoveTime, err = time.Parse(time.RFC3339, lastMove.CreatedAt)
			if err != nil {
				return 0, fmt.Errorf("failed to parse move timestamp: %w", err)
			}
		} else {
			// Use game creation time
			if createdAt, ok := gameValue["createdAt"].(string); ok {
				lastMoveTime, err = time.Parse(time.RFC3339, createdAt)
				if err != nil {
					return 0, fmt.Errorf("failed to parse game creation timestamp: %w", err)
				}
			} else {
				return 0, fmt.Errorf("game missing createdAt timestamp")
			}
		}

		// Calculate time remaining
		timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
		elapsed := time.Since(lastMoveTime)
		remaining := timeLimit - elapsed

		if remaining < 0 {
			return 0, nil // Time has expired
		}

		return remaining, nil
	}

	// TODO: Implement for other time control types
	return 0, fmt.Errorf("time control type %s not yet implemented", timeControlType)
}
