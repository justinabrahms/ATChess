package atproto

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

// atchess-1c9.8: prove that every AT Protocol record internal/atproto's
// Client actually POSTs conforms to its lexicon definition in lexicons/.
//
// Approach: an in-memory fake PDS (fakePDS below) stands in for a real
// com.atproto.repo.* server, so every writer method can be exercised
// end-to-end (including whatever reads it needs to reach its write, e.g.
// currentGameStatus's GetGame derivation) with no live PDS and no network.
// Every createRecord/putRecord body the fake PDS receives is captured
// verbatim (after exactly the JSON round-trip a real wire call would
// impose) and validated against the lexicon file matching its "$type".
//
// RESOLVED DRIFT (history, not a live finding): this suite originally
// caught two fields the client writes that their lexicons did not declare
// -- app.atchess.game's "updatedAt" (set by RecordMove, ResignGame,
// ClaimTimeVictory, and an accepted RespondToDrawOffer whenever they
// mutate an existing game record) and app.atchess.move's "draw" (set by
// RecordMove on a drawing move, alongside the already-declared "check" and
// "checkmate" -- "draw" was plainly forgotten when those two were added).
// Both were resolved the same way, by the operator: declare the field in
// its lexicon (lexicons/app.atchess.game.json, lexicons/app.atchess.move.json),
// since the client genuinely, deliberately emits it. The standing rule for
// this class of drift is exactly that choice, in this order: (1) if the
// field is real and intentional, declare it in the lexicon; (2) if it
// isn't, stop the writer from emitting it. Never paper over it by adding
// an exemption, loosening validateValue, or special-casing the field name
// anywhere in this file -- that would defeat the suite's purpose. The
// "draw" gap also demonstrates why every conditionally-emitted field needs
// a fixture that actually triggers it: the original single quiet-move
// fixture left check/checkmate/draw all false, so "draw" being undeclared
// was invisible until a checkmate/draw fixture was added (see
// recordMoveCases below).

// ---------------------------------------------------------------------
// Lexicon loading (dynamic -- walks lexicons/*.json, never hardcoded)
// ---------------------------------------------------------------------

// lexObjectSchema is a minimal, generic representation of a JSON-schema-ish
// AT Protocol object definition (a lexicon's defs.main.record, or any
// nested "type": "object" property, or a resolved "type": "ref").
type lexObjectSchema struct {
	Required   []string
	Properties map[string]lexPropSchema
}

// lexPropSchema describes one property's declared shape.
type lexPropSchema struct {
	Type   string // "string", "integer", "boolean", "object", "array", "ref"
	Format string // e.g. "did", "datetime"
	Enum   []string
	Ref    string           // for Type == "ref": the referenced lexicon id
	Object *lexObjectSchema // for Type == "object"
	Items  *lexPropSchema   // for Type == "array"
}

// knownRefs resolves "type": "ref" properties this codebase's lexicons
// actually use. com.atproto.repo.strongRef is a core AT Protocol lexicon,
// not one of our own files in lexicons/, so its shape is hardcoded here
// rather than loaded. Any lexicon property that refs something NOT in this
// map fails validation loudly (see validateValue's "ref" case) rather than
// being silently skipped -- that is itself a useful drift check.
var knownRefs = map[string]lexObjectSchema{
	"com.atproto.repo.strongRef": {
		Required: []string{"uri", "cid"},
		Properties: map[string]lexPropSchema{
			"uri": {Type: "string"},
			"cid": {Type: "string"},
		},
	},
}

// lexiconsDir locates the repo-root lexicons/ directory relative to this
// source file (not the test's working directory), so it works regardless
// of how `go test` is invoked.
func lexiconsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine lexicon_test.go's own path via runtime.Caller")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "lexicons")
}

// loadLexicons walks lexicons/*.json (os.ReadDir, not a hardcoded file
// list) and returns each lexicon's record schema keyed by its lexicon id.
func loadLexicons(t *testing.T) map[string]lexObjectSchema {
	t.Helper()
	dir := lexiconsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading lexicons dir %s: %v", dir, err)
	}

	result := map[string]lexObjectSchema{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading lexicon file %s: %v", path, err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("parsing lexicon file %s: %v", path, err)
		}
		id, _ := raw["id"].(string)
		if id == "" {
			t.Fatalf("lexicon file %s has no top-level \"id\"", e.Name())
		}
		defs, _ := raw["defs"].(map[string]interface{})
		main, _ := defs["main"].(map[string]interface{})
		record, _ := main["record"].(map[string]interface{})
		if record == nil {
			t.Fatalf("lexicon %s (%s): defs.main.record is missing", id, e.Name())
		}
		if _, dup := result[id]; dup {
			t.Fatalf("lexicon id %q is declared by more than one file in %s", id, dir)
		}
		result[id] = parseObjectSchema(record)
	}
	return result
}

func parseObjectSchema(raw map[string]interface{}) lexObjectSchema {
	var required []string
	if req, ok := raw["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	props := map[string]lexPropSchema{}
	if p, ok := raw["properties"].(map[string]interface{}); ok {
		for name, v := range p {
			if vm, ok := v.(map[string]interface{}); ok {
				props[name] = parsePropSchema(vm)
			}
		}
	}
	return lexObjectSchema{Required: required, Properties: props}
}

func parsePropSchema(raw map[string]interface{}) lexPropSchema {
	p := lexPropSchema{}
	if t, ok := raw["type"].(string); ok {
		p.Type = t
	}
	if f, ok := raw["format"].(string); ok {
		p.Format = f
	}
	if enumRaw, ok := raw["enum"].([]interface{}); ok {
		for _, e := range enumRaw {
			if s, ok := e.(string); ok {
				p.Enum = append(p.Enum, s)
			}
		}
	}
	if r, ok := raw["ref"].(string); ok {
		p.Ref = r
	}
	switch p.Type {
	case "object":
		obj := parseObjectSchema(raw)
		p.Object = &obj
	case "array":
		if items, ok := raw["items"].(map[string]interface{}); ok {
			ip := parsePropSchema(items)
			p.Items = &ip
		}
	}
	return p
}

// ---------------------------------------------------------------------
// Record validation
// ---------------------------------------------------------------------

// validateAgainstSchema checks a decoded JSON object against schema:
// every required property present, no undeclared properties, and each
// declared property's value individually type-checked.
func validateAgainstSchema(t *testing.T, path string, schema lexObjectSchema, data map[string]interface{}) {
	t.Helper()
	for _, req := range schema.Required {
		if _, present := data[req]; !present {
			t.Errorf("%s: missing required property %q", path, req)
		}
	}
	for key, val := range data {
		prop, declared := schema.Properties[key]
		if !declared {
			t.Errorf("%s: unknown property %q is not declared in the lexicon", path, key)
			continue
		}
		validateValue(t, path+"."+key, prop, val)
	}
}

func validateValue(t *testing.T, path string, prop lexPropSchema, val interface{}) {
	t.Helper()
	switch prop.Type {
	case "string":
		s, ok := val.(string)
		if !ok {
			t.Errorf("%s: expected a string, got %T (%v)", path, val, val)
			return
		}
		// EDGE CASE: datetime string format -- must be a real RFC3339
		// timestamp, not merely present.
		if prop.Format == "datetime" {
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				t.Errorf("%s: expected an RFC3339 datetime string, got %q: %v", path, s, err)
			}
		}
		if prop.Format == "did" && !strings.HasPrefix(s, "did:") {
			t.Errorf("%s: expected a DID string (\"did:...\"), got %q", path, s)
		}
		if len(prop.Enum) > 0 {
			found := false
			for _, e := range prop.Enum {
				if e == s {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: value %q is not one of the allowed enum values %v", path, s, prop.Enum)
			}
		}
	case "integer":
		f, ok := val.(float64)
		if !ok {
			t.Errorf("%s: expected an integer, got %T (%v)", path, val, val)
			return
		}
		if f != math.Trunc(f) {
			t.Errorf("%s: expected an integer, got a non-integral number %v", path, f)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			t.Errorf("%s: expected a boolean, got %T (%v)", path, val, val)
		}
	case "object":
		m, ok := val.(map[string]interface{})
		if !ok {
			t.Errorf("%s: expected an object, got %T (%v)", path, val, val)
			return
		}
		if prop.Object != nil {
			// EDGE CASE: nested objects such as timeControl are validated
			// recursively (required/unknown/type-checked within the
			// nested object too), not merely checked for "is an object".
			validateAgainstSchema(t, path, *prop.Object, m)
		}
	case "array":
		arr, ok := val.([]interface{})
		if !ok {
			t.Errorf("%s: expected an array, got %T (%v)", path, val, val)
			return
		}
		if prop.Items != nil {
			for i, item := range arr {
				validateValue(t, fmt.Sprintf("%s[%d]", path, i), *prop.Items, item)
			}
		}
	case "ref":
		m, ok := val.(map[string]interface{})
		if !ok {
			t.Errorf("%s: expected an object (ref %s), got %T (%v)", path, prop.Ref, val, val)
			return
		}
		refSchema, known := knownRefs[prop.Ref]
		if !known {
			t.Errorf("%s: references unknown ref type %q -- add its shape to knownRefs in lexicon_test.go", path, prop.Ref)
			return
		}
		validateAgainstSchema(t, path, refSchema, m)
	default:
		t.Errorf("%s: lexicon declares an unsupported/unrecognized type %q -- update lexicon_test.go's validateValue to handle it", path, prop.Type)
	}
}

// validateCapturedRecord validates one captured write's record against the
// lexicon named by its own "$type", as its own labeled subtest so failures
// clearly name which writer and which collection produced them.
func validateCapturedRecord(t *testing.T, lexicons map[string]lexObjectSchema, cr capturedRecord) {
	t.Helper()
	t.Run(fmt.Sprintf("%s/%s", cr.Source, cr.Collection), func(t *testing.T) {
		typ, ok := cr.Record["$type"].(string)
		if !ok || typ == "" {
			t.Fatalf("record written to collection %q by %s has no (or an empty) \"$type\" field: %+v", cr.Collection, cr.Source, cr.Record)
		}
		if typ != cr.Collection {
			t.Errorf("record's \"$type\" %q does not match the collection %q it was written to (by %s)", typ, cr.Collection, cr.Source)
		}
		schema, ok := lexicons[typ]
		if !ok {
			t.Fatalf("record's \"$type\" %q (written by %s) has no matching lexicon file in lexicons/", typ, cr.Source)
		}
		for _, req := range schema.Required {
			if _, present := cr.Record[req]; !present {
				t.Errorf("record (collection %s, written by %s) is missing required property %q", cr.Collection, cr.Source, req)
			}
		}
		for key, val := range cr.Record {
			if key == "$type" {
				continue // not one of the lexicon's declared properties; always expected
			}
			prop, declared := schema.Properties[key]
			if !declared {
				t.Errorf("record (collection %s, written by %s) has unknown property %q not declared in lexicon %s", cr.Collection, cr.Source, key, typ)
				continue
			}
			validateValue(t, fmt.Sprintf("%s.%s", cr.Collection, key), prop, val)
		}
	})
}

// ---------------------------------------------------------------------
// Fake PDS: an in-memory stand-in for com.atproto.repo.* / .server.*
// ---------------------------------------------------------------------

const (
	fakeDID    = "did:plc:conformancetest"
	fakeHandle = "conformance.test"
)

type storedRecord struct {
	cid   string
	value map[string]interface{}
}

// capturedRecord is one createRecord/putRecord body the fake PDS received,
// tagged (after the fact, by the driving subtest) with which writer method
// produced it.
type capturedRecord struct {
	Source     string
	Collection string
	Record     map[string]interface{}
}

// fakePDS is a minimal in-memory AT Protocol repo server: just enough of
// com.atproto.server.createSession / repo.createRecord / repo.putRecord /
// repo.getRecord / repo.listRecords for internal/atproto's Client to
// authenticate, write, and (for the derivation-heavy paths like
// currentGameStatus) read back what it and only it has written. There is
// exactly one simulated repo/DID (fakeDID) -- every writer test below uses
// that same DID as both "white" and "black" so every read the client's
// derivation logic issues resolves to this same fake PDS instead of
// attempting real DID/PDS resolution (see resolveReadEndpoint in
// client.go: reads for repoDID == c.did never leave the client's own PDS).
type fakePDS struct {
	mu       sync.Mutex
	seq      int
	records  map[string]map[string]*storedRecord // collection -> rkey -> record
	captured []capturedRecord
}

func newFakePDS() *fakePDS {
	return &fakePDS{records: map[string]map[string]*storedRecord{}}
}

func (p *fakePDS) nextID(prefix string) string {
	p.seq++
	return fmt.Sprintf("%s%d", prefix, p.seq)
}

func (p *fakePDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/xrpc/com.atproto.server.createSession":
		writeJSON(w, map[string]interface{}{
			"accessJwt":  "fake-access-jwt",
			"refreshJwt": "fake-refresh-jwt",
			"did":        fakeDID,
			"handle":     fakeHandle,
		})
	case "/xrpc/com.atproto.repo.createRecord":
		p.handleWrite(w, r, true)
	case "/xrpc/com.atproto.repo.putRecord":
		p.handleWrite(w, r, false)
	case "/xrpc/com.atproto.repo.getRecord":
		p.handleGet(w, r)
	case "/xrpc/com.atproto.repo.listRecords":
		p.handleList(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleWrite serves both com.atproto.repo.createRecord (isCreate == true)
// and com.atproto.repo.putRecord (isCreate == false). isCreate matters only
// for an explicit rkey that already names an existing record: putRecord is
// legitimately an upsert (that IS the operation), but createRecord at an
// already-occupied rkey was verified live (atchess-1c9.113) to be REJECTED
// by a real PDS with a bare, non-distinguishing HTTP 500 -- the first
// write is left intact, not silently overwritten. This fake used to
// overwrite unconditionally regardless of isCreate, diverging from that
// (atchess-1c9.115 item 3, the fourth such divergence found this run).
//
// putRecord additionally enforces "swapRecord" -- a real PDS's compare-
// and-swap parameter -- for real: a putRecord that supplies swapRecord and
// whose value does not match the record's CURRENT cid (including the case
// where no record exists there yet at all) is rejected with the same
// "InvalidSwap"-prefixed error body internal/atproto/client.go's
// isInvalidSwapBody looks for, exactly as a live PDS was verified
// (atchess-1c9.112) to respond. Before atchess-1c9.114, this fake accepted
// every putRecord unconditionally regardless of swapRecord, which is the
// same class of divergence atchess-1c9.112 found in raceMockPDS
// (internal/web/move_concurrency_test.go) before that bead fixed it: a test
// double silently implementing a laxer contract than the real server,
// which could hide a missing/broken CAS in the client exactly the way
// raceMockPDS's now-fixed swapCid defect once did.
func (p *fakePDS) handleWrite(w http.ResponseWriter, r *http.Request, isCreate bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req struct {
		Repo       string                 `json:"repo"`
		Collection string                 `json:"collection"`
		RKey       string                 `json:"rkey"`
		Record     map[string]interface{} `json:"record"`
		SwapRecord *string                `json:"swapRecord"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	if p.records[req.Collection] == nil {
		p.records[req.Collection] = map[string]*storedRecord{}
	}
	rkey := req.RKey
	if rkey == "" {
		rkey = p.nextID("rkey")
	} else if isCreate {
		if _, exists := p.records[req.Collection][rkey]; exists {
			// Explicit-rkey collision on createRecord: reject, matching
			// the live PDS's bare 500, and leave the existing record
			// untouched -- do NOT overwrite it.
			p.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"error": "InternalServerError", "message": "Internal Server Error"})
			return
		}
	} else if req.SwapRecord != nil {
		// putRecord's real compare-and-swap: the caller's swapRecord must
		// match the record's CURRENT cid, whether or not one already
		// exists at rkey. A stale (or altogether absent) cid is rejected,
		// and -- critically -- the existing record is left untouched, not
		// overwritten.
		cur := p.records[req.Collection][rkey]
		if cur == nil || cur.cid != *req.SwapRecord {
			p.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"error": "InvalidSwap", "message": "record was concurrently updated"})
			return
		}
	}
	cid := p.nextID("cid")
	// req.Record is already exactly what a real wire round-trip would
	// produce: it was decoded from the HTTP request body's JSON, so
	// numbers are float64 etc, matching what the client's own GET decode
	// would see back.
	p.records[req.Collection][rkey] = &storedRecord{cid: cid, value: req.Record}
	p.captured = append(p.captured, capturedRecord{Collection: req.Collection, Record: req.Record})
	p.mu.Unlock()

	writeJSON(w, map[string]interface{}{
		"uri": fmt.Sprintf("at://%s/%s/%s", req.Repo, req.Collection, rkey),
		"cid": cid,
	})
}

func (p *fakePDS) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	collection := q.Get("collection")
	rkey := q.Get("rkey")
	repo := q.Get("repo")

	p.mu.Lock()
	var rec *storedRecord
	if byRKey := p.records[collection]; byRKey != nil {
		rec = byRKey[rkey]
	}
	p.mu.Unlock()

	if rec == nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]interface{}{"error": "RecordNotFound", "message": "could not locate record"})
		return
	}
	writeJSON(w, map[string]interface{}{
		"uri":   fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
		"cid":   rec.cid,
		"value": rec.value,
	})
}

func (p *fakePDS) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	collection := q.Get("collection")
	repo := q.Get("repo")

	p.mu.Lock()
	records := []map[string]interface{}{}
	for rkey, rec := range p.records[collection] {
		records = append(records, map[string]interface{}{
			"uri":   fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid":   rec.cid,
			"value": rec.value,
		})
	}
	p.mu.Unlock()

	writeJSON(w, map[string]interface{}{"records": records})
}

// newTestClient stands up a fresh fakePDS and an authenticated Client
// against it.
func newTestClient(t *testing.T) (*Client, *fakePDS) {
	t.Helper()
	pds := newFakePDS()
	server := httptest.NewServer(pds)
	t.Cleanup(server.Close)

	client, err := NewClient(server.URL, fakeHandle, "password")
	if err != nil {
		t.Fatalf("failed to create test client against fake PDS: %v", err)
	}
	return client, pds
}

// ---------------------------------------------------------------------
// The suite
// ---------------------------------------------------------------------

// lexiconsNeverWritten names lexicons in lexicons/ that internal/atproto's
// Client deliberately never writes, each with why -- required by the
// meta-assertion below so an uncovered lexicon must be either exercised or
// explicitly, individually justified here, never silently skipped. This
// must stay in sync with internal/firehose/client.go's WantedCollections
// doc comment, which independently asserts the same two exclusions for the
// same reason (that file is the authoritative source for this claim).
var lexiconsNeverWritten = map[string]string{
	"app.atchess.challengeAcceptance": "retired design, superseded by app.atchess.challengeResponse (atchess-1c9.11): accepting a challenge is expressed by the app.atchess.game record the accepting player creates (its optional \"challenge\" strongRef field), never by a standalone acceptance record. See app.atchess.challengeResponse.json's own description field.",
	"app.atchess.gameIndex":           "unused lexicon: no code path in internal/atproto/client.go (or anywhere else in this repo) writes it. See internal/firehose/client.go's WantedCollections doc comment, which excludes it from the firehose subscription list for the identical reason.",
}

// TestLexiconConformance drives every record-producing Client method
// against a fake PDS, captures the record body(ies) each one POSTs, and
// validates each against the lexicon named by its own "$type". A final
// meta-assertion requires every lexicon file in lexicons/ to have been
// covered by at least one captured record, unless individually exempted
// (with a reason) in lexiconsNeverWritten above.
//
// HISTORY: this suite previously failed, for a real reason, on exactly the
// undeclared-field drift described in this file's top-of-file doc comment
// (app.atchess.game's "updatedAt", app.atchess.move's "draw"). Both are
// now declared in their lexicons, so the suite is green. If it goes red
// again on an "unknown property" error, that is this suite doing its job:
// resolve it the same way (declare the field if it is real and intended,
// or stop the writer from emitting it if it is not) -- never by adding an
// exemption, loosening validateValue, or special-casing a field name here.
func TestLexiconConformance(t *testing.T) {
	lexicons := loadLexicons(t)
	if len(lexicons) == 0 {
		t.Fatal("loaded zero lexicons from lexicons/ -- test setup is broken")
	}

	var allCaptured []capturedRecord
	covered := map[string]bool{}

	// capture tags every fakePDS write recorded since `before` with
	// `source` (the writer method under test) and folds it into
	// allCaptured/covered for later validation and the meta-assertion.
	capture := func(source string, pds *fakePDS, before int) {
		for i := before; i < len(pds.captured); i++ {
			pds.captured[i].Source = source
			if typ, _ := pds.captured[i].Record["$type"].(string); typ != "" {
				covered[typ] = true
			}
			allCaptured = append(allCaptured, pds.captured[i])
		}
	}

	ctx := context.Background()

	t.Run("setup/CreateGame", func(t *testing.T) {
		client, pds := newTestClient(t)
		before := len(pds.captured)
		if _, err := client.CreateGame(ctx, fakeDID, "white"); err != nil {
			t.Fatalf("CreateGame failed: %v", err)
		}
		capture("CreateGame", pds, before)
	})

	// atchess-1c9.29: CreateGameFromChallenge is the only writer that ever
	// sets app.atchess.game's optional "challenge" object (a
	// com.atproto.repo.strongRef of uri+cid) -- CreateGame above never
	// does. Before atchess-1c9.29 gave it a production caller
	// (AcceptChallenge), this suite's coverage of app.atchess.game never
	// actually exercised that field, so a lexicon/writer drift there could
	// have gone unnoticed (see this file's top-of-file doc comment for the
	// general shape of that class of bug). This fixture closes that gap.
	t.Run("setup/CreateGameFromChallenge", func(t *testing.T) {
		client, pds := newTestClient(t)
		before := len(pds.captured)
		if _, err := client.CreateGameFromChallenge(ctx, fakeDID, "white", "fromchallengerkey", "at://did:plc:opponent/app.atchess.challenge/chal001", "challenge-cid-1"); err != nil {
			t.Fatalf("CreateGameFromChallenge failed: %v", err)
		}
		capture("CreateGameFromChallenge", pds, before)
	})

	t.Run("setup/CreateChallenge", func(t *testing.T) {
		client, pds := newTestClient(t)
		before := len(pds.captured)
		if _, err := client.CreateChallenge(ctx, "did:plc:opponent", "white", "good luck, have fun"); err != nil {
			t.Fatalf("CreateChallenge failed: %v", err)
		}
		capture("CreateChallenge", pds, before)
	})

	t.Run("setup/RespondToChallenge", func(t *testing.T) {
		client, pds := newTestClient(t)
		before := len(pds.captured)
		err := client.RespondToChallenge(ctx, "at://did:plc:opponent/app.atchess.challenge/chal001", "challenge-cid-1", "declined")
		if err != nil {
			t.Fatalf("RespondToChallenge failed: %v", err)
		}
		capture("RespondToChallenge", pds, before)
	})

	// RecordMove sets "check"/"checkmate"/"draw" on the move record only
	// conditionally (see client.go's RecordMove), so a single boring
	// e2-e4 fixture never emits any of the three -- that shallow-fixture
	// gap is exactly how the "draw" field's own missing lexicon
	// declaration went unnoticed (see this file's top-of-file doc
	// comment). Three separate move fixtures below force each of the
	// three conditional booleans to actually flow through validation at
	// least once: a plain quiet move (none set), a checkmate (check and
	// checkmate set), and a drawing move (draw set). Each uses its own
	// fresh game since a terminal move (checkmate/draw) leaves that game
	// non-active.
	recordMoveCases := []struct {
		name string
		move *chess.MoveResult
	}{
		{
			name: "Quiet",
			move: &chess.MoveResult{
				From: "e2",
				To:   "e4",
				SAN:  "e4",
				FEN:  "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
			},
		},
		{
			name: "Checkmate",
			move: &chess.MoveResult{
				From:      "h4",
				To:        "e1",
				SAN:       "Qxe1#",
				FEN:       "rnb1kbnr/pppp1ppp/8/4p3/6Pq/5P2/PPPPP2P/RNBQKBNR w KQkq - 0 3",
				Check:     true,
				Checkmate: true,
			},
		},
		{
			name: "Draw",
			move: &chess.MoveResult{
				From: "h5",
				To:   "h6",
				SAN:  "Kh6",
				FEN:  "8/8/7k/8/8/8/6q1/7K w - - 0 1",
				Draw: true,
			},
		},
	}
	for _, tc := range recordMoveCases {
		t.Run("setup/RecordMove_"+tc.name, func(t *testing.T) {
			client, pds := newTestClient(t)
			game, err := client.CreateGame(ctx, fakeDID, "white")
			if err != nil {
				t.Fatalf("setup CreateGame failed: %v", err)
			}
			before := len(pds.captured)
			if err := client.RecordMove(ctx, game.ID, tc.move); err != nil {
				t.Fatalf("RecordMove failed: %v", err)
			}
			capture("RecordMove_"+tc.name, pds, before)
		})
	}

	t.Run("setup/OfferDraw", func(t *testing.T) {
		client, pds := newTestClient(t)
		game, err := client.CreateGame(ctx, fakeDID, "white")
		if err != nil {
			t.Fatalf("setup CreateGame failed: %v", err)
		}
		before := len(pds.captured)
		if _, err := client.OfferDraw(ctx, game.ID, "let's call it a draw"); err != nil {
			t.Fatalf("OfferDraw failed: %v", err)
		}
		capture("OfferDraw", pds, before)
	})

	t.Run("setup/RespondToDrawOffer", func(t *testing.T) {
		client, pds := newTestClient(t)
		game, err := client.CreateGame(ctx, fakeDID, "white")
		if err != nil {
			t.Fatalf("setup CreateGame failed: %v", err)
		}
		offer, err := client.OfferDraw(ctx, game.ID, "")
		if err != nil {
			t.Fatalf("setup OfferDraw failed: %v", err)
		}
		before := len(pds.captured)
		// Decline: exercises app.atchess.drawResponse without also
		// touching app.atchess.game's putRecord update path (that path is
		// already exercised, and its "updatedAt" finding already
		// demonstrated, by RecordMove/ResignGame/ClaimTimeVictory below).
		if err := client.RespondToDrawOffer(ctx, offer.URI, false); err != nil {
			t.Fatalf("RespondToDrawOffer failed: %v", err)
		}
		capture("RespondToDrawOffer", pds, before)
	})

	t.Run("setup/ResignGame", func(t *testing.T) {
		client, pds := newTestClient(t)
		game, err := client.CreateGame(ctx, fakeDID, "white")
		if err != nil {
			t.Fatalf("setup CreateGame failed: %v", err)
		}
		before := len(pds.captured)
		if err := client.ResignGame(ctx, game.ID, "out of time to keep playing"); err != nil {
			t.Fatalf("ResignGame failed: %v", err)
		}
		capture("ResignGame", pds, before)
	})

	t.Run("setup/ClaimTimeVictory", func(t *testing.T) {
		client, pds := newTestClient(t)
		game, err := client.CreateGame(ctx, fakeDID, "white")
		if err != nil {
			t.Fatalf("setup CreateGame failed: %v", err)
		}

		// Backdate the game's createdAt directly in the fake PDS's store
		// (bypassing the HTTP API entirely -- this is fixture setup, not a
		// captured write, and must never appear in allCaptured) so the
		// default correspondence/3-days-per-move window has already
		// elapsed by the time ClaimTimeVictory calls CheckTimeViolation.
		gameParts := strings.Split(game.ID, "/")
		rkey := gameParts[len(gameParts)-1]
		pds.mu.Lock()
		pds.records["app.atchess.game"][rkey].value["createdAt"] = time.Now().Add(-96 * time.Hour).Format(time.RFC3339)
		pds.mu.Unlock()

		before := len(pds.captured)
		if err := client.ClaimTimeVictory(ctx, game.ID); err != nil {
			t.Fatalf("ClaimTimeVictory failed: %v", err)
		}
		capture("ClaimTimeVictory", pds, before)
	})

	// Validate every captured write against the lexicon its own "$type"
	// names.
	for _, cr := range allCaptured {
		validateCapturedRecord(t, lexicons, cr)
	}

	// Meta-assertion (step 4): every lexicon in lexicons/ must be covered
	// by at least one captured record, unless individually exempted above.
	// An uncovered, unexempted lexicon fails the suite and is named.
	for id := range lexicons {
		if _, exempt := lexiconsNeverWritten[id]; exempt {
			continue
		}
		if !covered[id] {
			t.Errorf("meta-assertion: lexicon %q has no captured record covering it in this suite -- either add a writer subtest that exercises it, or add a justified entry to lexiconsNeverWritten in lexicon_test.go", id)
		}
	}
	// Guard the exemption list itself against drift in both directions:
	// a stale reference to a lexicon that no longer exists, and a
	// once-true "never written" claim that a captured record now
	// contradicts.
	for id, reason := range lexiconsNeverWritten {
		if _, exists := lexicons[id]; !exists {
			t.Errorf("lexiconsNeverWritten references %q, which does not match any lexicon file in lexicons/ -- stale exemption? (reason on file: %s)", id, reason)
		}
		if covered[id] {
			t.Errorf("lexiconsNeverWritten claims %q is never written, but a captured record in this suite covered it -- the exemption is stale, remove it", id)
		}
	}
}
