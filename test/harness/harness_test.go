//go:build e2e

package harness

import (
	"testing"
)

// TestSmokeDistinctIdentities constructs both harness players against the
// live dual-PDS stack and asserts that they are genuinely distinct
// identities: different DIDs and different PDS URLs. This is the exact
// failure mode this harness exists to catch -- if it ever passed with
// alice and bob resolving to the same identity, every downstream
// conformance test built on this harness would be worthless.
func TestSmokeDistinctIdentities(t *testing.T) {
	accounts := LoadAccounts(t)
	services := StartServices(t, accounts)

	alice := NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := NewPlayer(t, accounts.Bob, services.BobURL)

	if alice.DID == "" {
		t.Fatal("alice has an empty DID")
	}
	if bob.DID == "" {
		t.Fatal("bob has an empty DID")
	}
	if alice.DID == bob.DID {
		t.Fatalf("alice and bob resolved to the SAME DID (%s) -- the harness is not providing distinct identities", alice.DID)
	}

	if alice.PDSURL == "" {
		t.Fatal("alice has an empty PDS URL")
	}
	if bob.PDSURL == "" {
		t.Fatal("bob has an empty PDS URL")
	}
	if alice.PDSURL == bob.PDSURL {
		t.Fatalf("alice and bob resolved to the SAME PDS URL (%s) -- the harness is not providing distinct PDS instances", alice.PDSURL)
	}

	if alice.SessionID == "" {
		t.Fatal("alice has an empty session id")
	}
	if bob.SessionID == "" {
		t.Fatal("bob has an empty session id")
	}

	t.Logf("alice: did=%s pds=%s protocol=%s", alice.DID, alice.PDSURL, alice.ProtocolURL)
	t.Logf("bob:   did=%s pds=%s protocol=%s", bob.DID, bob.PDSURL, bob.ProtocolURL)
}
