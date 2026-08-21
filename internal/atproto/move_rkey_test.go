package atproto

import (
	"encoding/json"
	"regexp"
	"testing"
)

// --- atchess-1c9.113: deterministic move-record rkey ---
//
// RecordMove used to let the PDS mint a server-generated rkey for every
// move record. That left a same-player double-submit completely
// unprotected whenever the mover did not own the game record (the CAS in
// RecordMove's "if repo == c.did" block never runs for the non-owner --
// see client.go), because two independent createRecord calls always
// landed as two independent records. moveRkeyForPly fixes this by
// deriving the move's rkey from (gameURI, ply) instead, so a duplicate
// submission collides at the PDS rather than minting a second record.

// atRkeySyntax matches AT Protocol's record-key syntax:
// [a-zA-Z0-9._:~-]{1,512}, and never exactly "." or "..".
var atRkeySyntax = regexp.MustCompile(`^[a-zA-Z0-9._:~-]{1,512}$`)

func mustBeValidRkey(t *testing.T, rkey string) {
	t.Helper()
	if rkey == "." || rkey == ".." {
		t.Fatalf("rkey %q is the reserved '.'/'..' form, invalid AT Protocol record-key syntax", rkey)
	}
	if !atRkeySyntax.MatchString(rkey) {
		t.Fatalf("rkey %q does not match AT Protocol record-key syntax", rkey)
	}
}

func TestMoveRkeyForPly_DeterministicForSameMove(t *testing.T) {
	gameURI := "at://did:plc:opponent/app.atchess.game/game1"
	a := moveRkeyForPly(gameURI, 3)
	b := moveRkeyForPly(gameURI, 3)
	if a != b {
		t.Fatalf("expected the same (gameURI, ply) to always produce the same rkey, got %q and %q", a, b)
	}
	mustBeValidRkey(t, a)
}

func TestMoveRkeyForPly_DistinctAcrossPliesInSameGame(t *testing.T) {
	gameURI := "at://did:plc:opponent/app.atchess.game/game1"
	seen := map[string]int{}
	for ply := 0; ply < 50; ply++ {
		rkey := moveRkeyForPly(gameURI, ply)
		mustBeValidRkey(t, rkey)
		if prior, ok := seen[rkey]; ok {
			t.Fatalf("ply %d produced the same rkey %q as ply %d -- expected distinct rkeys across plies in one game", ply, rkey, prior)
		}
		seen[rkey] = ply
	}
}

func TestMoveRkeyForPly_DistinctAcrossGamesAtSamePly(t *testing.T) {
	rkeyA := moveRkeyForPly("at://did:plc:alice/app.atchess.game/game1", 5)
	rkeyB := moveRkeyForPly("at://did:plc:bob/app.atchess.game/game1", 5)
	if rkeyA == rkeyB {
		t.Fatalf("two different games (different gameURI) at the same ply must not collide, both got %q", rkeyA)
	}
}

func TestMoveRkeyForPly_AlwaysSyntacticallyValid_EvenForAdversarialGameURI(t *testing.T) {
	// atchess-1c9.92 notes proposedGameId (and hence a derived gameURI) is
	// not itself syntax-checked. moveRkeyForPly must never propagate
	// whatever garbage characters gameURI contains into its own output --
	// it hashes them away instead.
	adversarial := []string{
		"at://did:plc:x/app.atchess.game/../../etc/passwd",
		"at://did:plc:x/app.atchess.game/?foo=1&bar=2",
		"",
		"not-even-a-uri",
		"at://did:plc:x/app.atchess.game/" + string(rune(0)) + "null-byte",
	}
	for _, g := range adversarial {
		mustBeValidRkey(t, moveRkeyForPly(g, 1))
	}
}

// --- moveRecordContentEqual unit coverage ---

func baseMoveRecordValue() map[string]interface{} {
	return map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": "2026-08-21T00:00:00Z",
		"game": map[string]interface{}{
			"uri": "at://did:plc:opponent/app.atchess.game/game1",
			"cid": "bafyabc123",
		},
		"player": "did:plc:player",
		"from":   "e2",
		"to":     "e4",
		"san":    "e4",
		"fen":    "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
	}
}

func TestMoveRecordContentEqual_IgnoresCreatedAt(t *testing.T) {
	a := baseMoveRecordValue()
	b := baseMoveRecordValue()
	b["createdAt"] = "2026-08-21T00:00:05Z" // a later retry's own timestamp
	if !moveRecordContentEqual(a, b) {
		t.Fatalf("expected records identical except createdAt to compare equal")
	}
}

func TestMoveRecordContentEqual_DifferentMoveContentNotEqual(t *testing.T) {
	a := baseMoveRecordValue()
	b := baseMoveRecordValue()
	b["to"] = "d4"
	b["san"] = "d4"
	b["fen"] = "rnbqkbnr/pppppppp/8/8/3P4/8/PPP1PPPP/RNBQKBNR b KQkq d3 0 1"
	if moveRecordContentEqual(a, b) {
		t.Fatalf("expected genuinely different moves (different to/san/fen) to compare unequal")
	}
}

func TestMoveRecordContentEqual_DifferentGameCidNotEqual(t *testing.T) {
	a := baseMoveRecordValue()
	b := baseMoveRecordValue()
	bGame := b["game"].(map[string]interface{})
	bGame["cid"] = "bafydifferent"
	if moveRecordContentEqual(a, b) {
		t.Fatalf("expected a differing embedded game cid to compare unequal, not be silently folded together")
	}
}

// TestMoveRecordContentEqual_NewFieldIsNotSilentlyExcluded is the drift
// guard for atchess-1c9.115 item 1. moveRecordContentEqual used to compare
// a hardcoded field list; a future field added to moveRecord (client.go)
// would be silently EXCLUDED from the comparison, so two submissions
// differing only in that new field would wrongly compare equal and be
// folded together as an idempotent resubmission instead of surfacing as
// ErrMoveRecordConflict. moveRecordContentEqual is now derived from the
// union of both records' own keys instead of a hand-written list, which
// makes that exclusion structurally impossible: this test pins that by
// adding an entirely new, never-hardcoded key and confirming a difference
// in it alone is still detected.
func TestMoveRecordContentEqual_NewFieldIsNotSilentlyExcluded(t *testing.T) {
	a := baseMoveRecordValue()
	b := baseMoveRecordValue()
	a["totallyNewHypotheticalField"] = "value-one"
	b["totallyNewHypotheticalField"] = "value-two"
	if moveRecordContentEqual(a, b) {
		t.Fatalf("expected a difference in a field neither hardcoded field list nor any prior test knew about to still be detected, not silently ignored")
	}
}

// TestMoveRecordContentEqual_NewFieldIsNotSilentlyExcluded_Numeric extends
// the .115 drift guard above to a NUMERIC field, alongside the existing
// string one. A string field's type never changes across a JSON round
// trip, so it can't exercise atchess-1c9.116's hazard (int vs float64) --
// this test adds a same-named, same-valued numeric field to both sides
// and confirms they still compare equal, i.e. that adding a numeric field
// to moveRecord does not, by itself, start producing spurious inequality.
func TestMoveRecordContentEqual_NewFieldIsNotSilentlyExcluded_Numeric(t *testing.T) {
	a := baseMoveRecordValue()
	b := baseMoveRecordValue()
	a["totallyNewHypotheticalNumericField"] = 7
	b["totallyNewHypotheticalNumericField"] = 7
	if !moveRecordContentEqual(a, b) {
		t.Fatalf("expected an identical numeric field present on both sides to compare equal")
	}

	a["totallyNewHypotheticalNumericField"] = 7
	b["totallyNewHypotheticalNumericField"] = 8
	if moveRecordContentEqual(a, b) {
		t.Fatalf("expected a difference in a new numeric field to still be detected, not silently ignored")
	}
}

// TestMoveRecordContentEqual_NumericFieldSurvivesJSONRoundTrip is the
// atchess-1c9.116 headline: RecordMove compares a Go-built moveRecord
// (where an int field stays an int) against one just read back via
// getRecordByURI, which decodes the PDS's JSON response into
// map[string]interface{} -- and encoding/json always decodes a JSON
// number as float64, never int. Simulate that real round trip explicitly
// (rather than hand-writing a float64 literal, which would not pin the
// actual hazard): "goSide" holds an int, exactly as moveRecord would after
// gaining a numeric field such as the lexicon's declared-but-unwritten
// "moveNumber" (atchess-1c9.8); "pdsSide" is the SAME logical record after
// a JSON marshal/unmarshal, exactly as getRecordByURI's json.Decode would
// produce it. These must compare equal -- an identical resubmission must
// not be misclassified as ErrMoveRecordConflict (HTTP 409) merely because
// int != float64.
func TestMoveRecordContentEqual_NumericFieldSurvivesJSONRoundTrip(t *testing.T) {
	goSide := baseMoveRecordValue()
	goSide["moveNumber"] = 5 // int, exactly as a Go literal in moveRecord would be

	buf, err := json.Marshal(goSide)
	if err != nil {
		t.Fatalf("failed to marshal goSide: %v", err)
	}
	var pdsSide map[string]interface{}
	if err := json.Unmarshal(buf, &pdsSide); err != nil {
		t.Fatalf("failed to unmarshal pdsSide: %v", err)
	}
	// Sanity check the hazard is actually present in this test's setup:
	// pdsSide's "moveNumber" must have decoded as float64, not int, or
	// this test would not be exercising atchess-1c9.116 at all.
	if _, ok := pdsSide["moveNumber"].(float64); !ok {
		t.Fatalf("test setup invalid: expected pdsSide[\"moveNumber\"] to decode as float64 (got %T), this test is meaningless without that", pdsSide["moveNumber"])
	}

	if !moveRecordContentEqual(goSide, pdsSide) {
		t.Fatalf("expected an identical resubmission differing only in numeric-field Go type (int) vs JSON-decoded type (float64) to compare equal, not be misclassified as a conflict")
	}
}

// TestMoveRecordContentEqual_NilValuePresentVsKeyAbsent_SymmetricInBothDirections
// pins the atchess-1c9.116 asymmetry fix: a key present with an explicit
// nil value on one side and absent entirely from the other must compare
// UNEQUAL, regardless of which side is passed as "a" and which as "b".
// Unreachable via moveRecord today (it never sets a nil value), but
// latent and worth pinning directly at the unit level.
func TestMoveRecordContentEqual_NilValuePresentVsKeyAbsent_SymmetricInBothDirections(t *testing.T) {
	withNil := baseMoveRecordValue()
	withNil["extra"] = nil
	withoutKey := baseMoveRecordValue()

	if moveRecordContentEqual(withNil, withoutKey) {
		t.Fatalf("expected a key present-with-nil on the first argument and absent from the second to compare unequal")
	}
	if moveRecordContentEqual(withoutKey, withNil) {
		t.Fatalf("expected a key present-with-nil on the second argument and absent from the first to compare unequal")
	}
}
