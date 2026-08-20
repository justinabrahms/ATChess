package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// acceptHandlerPDS is a minimal, in-memory, multi-repo AT Protocol stand-in
// used by AcceptChallengeHandler's tests. A single httptest server plays
// both the PLC directory (GET /did:plc:<x> resolves to itself as that
// DID's serviceEndpoint) and every repo's PDS
// (com.atproto.repo.createRecord/getRecord) -- mirroring
// internal/atproto/accept_challenge_test.go's acceptTestPDS, duplicated
// here (rather than exported/shared) because internal/atproto's version
// is intentionally test-only/unexported and this package tests the HTTP
// layer above it, not atproto.Client itself.
type acceptHandlerPDS struct {
	base string

	mu      sync.Mutex
	records map[string]map[string]map[string]storedValue // repo -> collection -> rkey -> record
	seq     int
}

type storedValue struct {
	cid   string
	value map[string]interface{}
}

func newAcceptHandlerPDS(t *testing.T) *acceptHandlerPDS {
	t.Helper()
	p := &acceptHandlerPDS{records: map[string]map[string]map[string]storedValue{}}
	// atchess-1c9.95: p.base is embedded as a DID document's
	// serviceEndpoint, which internal/atproto.parseServiceEndpoint now
	// validates -- see newFakeHTTPSEndpoint for why this can no longer be
	// the httptest server's own http://127.0.0.1:<port> address.
	srv := newFakeHTTPSEndpoint(t, http.HandlerFunc(p.handle))
	p.base = srv.URL
	return p
}

func (p *acceptHandlerPDS) nextID(prefix string) string {
	p.seq++
	return fmt.Sprintf("%s%d", prefix, p.seq)
}

func (p *acceptHandlerPDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(atproto.DIDDocument{
			ID: did,
			Service: []atproto.DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: p.base},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.repo.createRecord":
		p.handleCreate(w, r)
	case "/xrpc/com.atproto.repo.getRecord":
		p.handleGet(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (p *acceptHandlerPDS) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo       string                 `json:"repo"`
		Collection string                 `json:"collection"`
		RKey       string                 `json:"rkey"`
		Record     map[string]interface{} `json:"record"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[req.Repo] == nil {
		p.records[req.Repo] = map[string]map[string]storedValue{}
	}
	if p.records[req.Repo][req.Collection] == nil {
		p.records[req.Repo][req.Collection] = map[string]storedValue{}
	}
	rkey := req.RKey
	if rkey == "" {
		rkey = p.nextID("rkey")
	}
	if _, exists := p.records[req.Repo][req.Collection][rkey]; exists {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "InvalidSwap", "message": "record already exists"})
		return
	}
	cid := p.nextID("cid")
	p.records[req.Repo][req.Collection][rkey] = storedValue{cid: cid, value: req.Record}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri": fmt.Sprintf("at://%s/%s/%s", req.Repo, req.Collection, rkey),
		"cid": cid,
	})
}

func (p *acceptHandlerPDS) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, collection, rkey := q.Get("repo"), q.Get("collection"), q.Get("rkey")

	p.mu.Lock()
	rec, ok := p.records[repo][collection][rkey]
	p.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "RecordNotFound"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri":   fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
		"cid":   rec.cid,
		"value": rec.value,
	})
}

// putChallenge seeds an app.atchess.challenge record directly into repo's
// simulated collection, returning the CID the fake PDS assigned it.
func (p *acceptHandlerPDS) putChallenge(repo, rkey string, value map[string]interface{}) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[repo] == nil {
		p.records[repo] = map[string]map[string]storedValue{}
	}
	if p.records[repo]["app.atchess.challenge"] == nil {
		p.records[repo]["app.atchess.challenge"] = map[string]storedValue{}
	}
	cid := p.nextID("cid")
	p.records[repo]["app.atchess.challenge"][rkey] = storedValue{cid: cid, value: value}
	return cid
}

func (p *acceptHandlerPDS) gameCount(repo string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records[repo]["app.atchess.game"])
}

// putGame seeds an app.atchess.game record directly, bypassing
// AcceptChallengeHandler entirely -- used to plant a pre-existing,
// UNRELATED game at a chosen rkey for the crafted-proposedGameId
// collision test (atchess-1c9.29 review fix).
func (p *acceptHandlerPDS) putGame(repo, rkey string, value map[string]interface{}) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[repo] == nil {
		p.records[repo] = map[string]map[string]storedValue{}
	}
	if p.records[repo]["app.atchess.game"] == nil {
		p.records[repo]["app.atchess.game"] = map[string]storedValue{}
	}
	cid := p.nextID("cid")
	p.records[repo]["app.atchess.game"][rkey] = storedValue{cid: cid, value: value}
	return cid
}

const (
	whChallengerDID = "did:plc:whchallenger"
	whChallengedDID = "did:plc:whchallenged"
	whStrangerDID   = "did:plc:whstranger"
)

func seedWHChallenge(pds *acceptHandlerPDS) (challengeURI, challengeCID string) {
	return seedWHChallengeWith(pds, whChallengerDID, whChallengedDID, "chal1", "whgame1", "pending", time.Now().Add(24*time.Hour))
}

// seedWHChallengeWith seeds an app.atchess.challenge record with full
// control over status/expiresAt (relative to time.Now(), never a
// hardcoded timestamp -- a fixed past-dated fixture already broke this
// suite once when "now" caught up with it).
func seedWHChallengeWith(pds *acceptHandlerPDS, challengerDID, challengedDID, rkey, proposedGameID, status string, expiresAt time.Time) (challengeURI, challengeCID string) {
	cid := pds.putChallenge(challengerDID, rkey, map[string]interface{}{
		"$type":          "app.atchess.challenge",
		"createdAt":      time.Now().Add(-time.Hour).Format(time.RFC3339),
		"challenger":     challengerDID,
		"challenged":     challengedDID,
		"status":         status,
		"color":          "white",
		"proposedGameId": proposedGameID,
		"expiresAt":      expiresAt.Format(time.RFC3339),
	})
	return "at://" + challengerDID + "/app.atchess.challenge/" + rkey, cid
}

// sessionFor builds an *oauth.Session for did against pds, and a request
// carrying it (plus the AuthenticatedDID context value AuthMiddleware
// would normally set) for method/path, with {key} set to challengeURI's
// URL-safe-base64 encoding (AcceptChallengeHandler's route segment --
// same encoding as DeclineChallengeHandler's, see its doc comment).
func acceptRequestAs(did, challengeURI string, pds *acceptHandlerPDS) *http.Request {
	session := &oauth.Session{
		DID:                  did,
		Handle:               did,
		PDSURL:               pds.base,
		AccessToken:          "test-jwt-" + did,
		RefreshToken:         "test-refresh-" + did,
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}
	encodedKey := base64.URLEncoding.EncodeToString([]byte(challengeURI))
	req := httptest.NewRequest("POST", "/api/challenge-notifications/"+encodedKey+"/accept", nil)
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"key": encodedKey})
	return req
}

func acceptHandlerService(t *testing.T, pds *acceptHandlerPDS) (*Service, *challenge.Store) {
	t.Helper()
	store := newTestChallengeStore(t)
	svc := &Service{
		config: &config.Config{
			ATProto: config.ATProtoConfig{PLCDirectoryURL: pds.base},
		},
		challengeStore: store,
	}
	return svc, store
}

// TestAcceptChallengeHandler_Success_LinksChallengeAndMarksAccepted covers
// the endpoint's happy path end to end through the HTTP layer: 200, the
// response body decodes to the expected game, and the local
// challenge.Store index no longer lists the challenge for the challenged
// player afterward (atchess-1c9.29's server-side cleanup, replacing the
// client's old DELETE-notification call -- see web/static/index.html's
// updated acceptChallenge).
func TestAcceptChallengeHandler_Success_LinksChallengeAndMarksAccepted(t *testing.T) {
	pds := newAcceptHandlerPDS(t)
	challengeURI, challengeCID := seedWHChallenge(pds)
	svc, store := acceptHandlerService(t, pds)

	// Seed the local index too, as GetChallengeNotificationsHandler's real
	// caller (CreateChallengeHandler / the firehose / backfill) would have.
	if _, err := store.Add(&challenge.PendingChallenge{
		ChallengeURI:  challengeURI,
		ChallengeCID:  challengeCID,
		ChallengerDID: whChallengerDID,
		ChallengedDID: whChallengedDID,
		Color:         "white",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	req := acceptRequestAs(whChallengedDID, challengeURI, pds)
	w := httptest.NewRecorder()
	svc.AcceptChallengeHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}
	var game struct {
		ID    string `json:"id"`
		White string `json:"white"`
		Black string `json:"black"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &game); err != nil {
		t.Fatalf("decoding response: %v (body: %s)", err, w.Body.String())
	}
	wantID := "at://" + whChallengedDID + "/app.atchess.game/whgame1"
	if game.ID != wantID {
		t.Errorf("game.id = %q, want %q", game.ID, wantID)
	}

	remaining, err := store.ForPlayer(whChallengedDID)
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected the accepted challenge to be removed from the local index (MarkAccepted), got %d remaining", len(remaining))
	}
}

// TestAcceptChallengeHandler_NonParticipant_Forbidden is the HTTP-layer
// half of the ownership-check property: a caller who is neither the
// challenger nor the challenged party must get 403, not the game --
// exercised here after the challenged player has already accepted, which
// is exactly the information-leak scenario the orchestrator's notes on
// atchess-1c9.29 call out (a guessable challenge-derived rkey must not
// let a random caller retrieve the resulting game).
func TestAcceptChallengeHandler_NonParticipant_Forbidden(t *testing.T) {
	pds := newAcceptHandlerPDS(t)
	challengeURI, _ := seedWHChallenge(pds)
	svc, _ := acceptHandlerService(t, pds)

	okReq := acceptRequestAs(whChallengedDID, challengeURI, pds)
	okW := httptest.NewRecorder()
	svc.AcceptChallengeHandler(okW, okReq)
	if okW.Code != http.StatusOK {
		t.Fatalf("setup: challenged player's own accept should succeed, got %d: %s", okW.Code, okW.Body.String())
	}

	req := acceptRequestAs(whStrangerDID, challengeURI, pds)
	w := httptest.NewRecorder()
	svc.AcceptChallengeHandler(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 for a non-participant, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAcceptChallengeHandler_NotChallengedPlayer_Rejected is the HTTP-layer
// half of "only the challenged player may accept": the CHALLENGER --  a
// real participant, but the wrong one -- must be rejected, not handed a
// freshly-created game.
func TestAcceptChallengeHandler_NotChallengedPlayer_Rejected(t *testing.T) {
	pds := newAcceptHandlerPDS(t)
	challengeURI, _ := seedWHChallenge(pds)
	svc, _ := acceptHandlerService(t, pds)

	req := acceptRequestAs(whChallengerDID, challengeURI, pds)
	w := httptest.NewRecorder()
	svc.AcceptChallengeHandler(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 (only the challenged player may originate an accept), got %d: %s", w.Code, w.Body.String())
	}
	if n := pds.gameCount(whChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created, got %d", n)
	}
}

// TestAcceptChallengeHandler_DuplicateAccept_SameGameNoSecondRecord covers
// the design decision directly through the HTTP layer: two accepts by the
// challenged player both return HTTP 200 with the same game, and exactly
// one game record is ever created.
func TestAcceptChallengeHandler_DuplicateAccept_SameGameNoSecondRecord(t *testing.T) {
	pds := newAcceptHandlerPDS(t)
	challengeURI, _ := seedWHChallenge(pds)
	svc, _ := acceptHandlerService(t, pds)

	var ids [2]string
	for i := 0; i < 2; i++ {
		req := acceptRequestAs(whChallengedDID, challengeURI, pds)
		w := httptest.NewRecorder()
		svc.AcceptChallengeHandler(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("accept #%d: expected HTTP 200, got %d: %s", i+1, w.Code, w.Body.String())
		}
		var game struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &game); err != nil {
			t.Fatalf("accept #%d: decoding response: %v", i+1, err)
		}
		ids[i] = game.ID
	}

	if ids[0] != ids[1] {
		t.Errorf("duplicate accept returned different games: %q vs %q", ids[0], ids[1])
	}
	if n := pds.gameCount(whChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record after a duplicate accept, got %d", n)
	}
}

// TestAcceptChallengeHandler_ChallengeNotFound covers the "challenge
// record unreadable because it genuinely does not exist" case at the HTTP
// layer: 404, not a 500 or a silently-created dangling game.
func TestAcceptChallengeHandler_ChallengeNotFound(t *testing.T) {
	pds := newAcceptHandlerPDS(t)
	svc, _ := acceptHandlerService(t, pds)

	missingURI := "at://" + whChallengerDID + "/app.atchess.challenge/doesnotexist"
	req := acceptRequestAs(whChallengedDID, missingURI, pds)
	w := httptest.NewRecorder()
	svc.AcceptChallengeHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404, got %d: %s", w.Code, w.Body.String())
	}
	if n := pds.gameCount(whChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created for a nonexistent challenge, got %d", n)
	}
}

// TestAcceptChallengeHandler_CraftedProposedGameId_ConflictNotAccepted is
// the HTTP-layer regression test for the reviewer-found vulnerability: a
// challenge whose proposedGameId collides with an unrelated, pre-existing
// game in the challenged (victim) player's own repo must return 409, must
// NOT hand back that unrelated game, must NOT create a new game record,
// and -- the damaging half of the original bug -- must NOT be marked
// accepted in the local challenge.Store index (which would have
// permanently, silently "consumed" the challenge without ever producing
// a real game).
func TestAcceptChallengeHandler_CraftedProposedGameId_ConflictNotAccepted(t *testing.T) {
	pds := newAcceptHandlerPDS(t)

	pds.putGame(whChallengedDID, "existinggame", map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Add(-time.Hour).Format(time.RFC3339),
		"white":     whChallengedDID,
		"black":     "did:plc:whunrelatedthirdparty",
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
	})

	challengeURI, challengeCID := seedWHChallengeWith(pds, whChallengerDID, whChallengedDID, "craftedchal", "existinggame", "pending", time.Now().Add(24*time.Hour))
	svc, store := acceptHandlerService(t, pds)
	if _, err := store.Add(&challenge.PendingChallenge{
		ChallengeURI:  challengeURI,
		ChallengeCID:  challengeCID,
		ChallengerDID: whChallengerDID,
		ChallengedDID: whChallengedDID,
		Color:         "white",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	req := acceptRequestAs(whChallengedDID, challengeURI, pds)
	w := httptest.NewRecorder()
	svc.AcceptChallengeHandler(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409, got %d: %s", w.Code, w.Body.String())
	}
	if n := pds.gameCount(whChallengedDID); n != 1 {
		t.Errorf("expected the victim's repo to still contain exactly the 1 pre-existing (unrelated) game, got %d", n)
	}

	remaining, err := store.ForPlayer(whChallengedDID)
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected the crafted challenge to remain PENDING (not marked accepted) after the conflict, got %d remaining", len(remaining))
	}
}
