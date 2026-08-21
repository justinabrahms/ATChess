package atproto

import (
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
