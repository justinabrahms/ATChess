package atproto

import (
	"encoding/json"
	"testing"
)

// participantDIDs decides whether a game belongs in your list. Getting it wrong
// in either direction is bad in a way nobody notices: a missed shape drops a
// real game silently, and there is no error, just a shorter list.
func TestParticipantDIDsHandlesBothRecordShapes(t *testing.T) {
	cases := []struct {
		name                 string
		raw                  string
		wantWhite, wantBlack string
	}{
		{
			name:      "nested objects",
			raw:       `{"white":{"did":"did:plc:w","handle":"w.test"},"black":{"did":"did:plc:b"}}`,
			wantWhite: "did:plc:w", wantBlack: "did:plc:b",
		},
		{
			// The shape actually found in production on 2026-08-30: the live
			// game record between two real accounts stores the DIDs flat.
			name:      "flat strings",
			raw:       `{"white":"did:plc:w","black":"did:plc:b","status":"active"}`,
			wantWhite: "did:plc:w", wantBlack: "did:plc:b",
		},
		{
			name:      "mixed, white nested and black flat",
			raw:       `{"white":{"did":"did:plc:w"},"black":"did:plc:b"}`,
			wantWhite: "did:plc:w", wantBlack: "did:plc:b",
		},
		{
			name:      "neither present",
			raw:       `{"status":"active"}`,
			wantWhite: "", wantBlack: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, b := participantDIDs(json.RawMessage(tc.raw))
			if w != tc.wantWhite || b != tc.wantBlack {
				t.Errorf("got white=%q black=%q, want white=%q black=%q\n"+
					"A record shape this does not understand is a game that "+
					"silently never appears in its players' lists.",
					w, b, tc.wantWhite, tc.wantBlack)
			}
		})
	}
}

// The index rkey must be stable, or every listing writes another copy of the
// same entry and the index grows without bound.
func TestDeriveIndexRkeyIsStableAndDistinct(t *testing.T) {
	a := "at://did:plc:x/app.atchess.game/aaa"
	b := "at://did:plc:x/app.atchess.game/bbb"

	if deriveIndexRkey(a) != deriveIndexRkey(a) {
		t.Error("the same game produced two different rkeys; every listing would add a duplicate index entry")
	}
	if deriveIndexRkey(a) == deriveIndexRkey(b) {
		t.Error("two different games collided on one rkey; one would overwrite the other")
	}
	// Record keys have a restricted charset; anything outside it is rejected by
	// the PDS at write time, which would only show up in production.
	for _, r := range deriveIndexRkey(a) {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			t.Errorf("rkey %q contains %q, which is outside the safe record-key charset", deriveIndexRkey(a), r)
		}
	}
}

func TestRepoOfATURI(t *testing.T) {
	for uri, want := range map[string]string{
		"at://did:plc:abc/app.atchess.game/xyz": "did:plc:abc",
		"at://did:plc:abc":                      "did:plc:abc",
		"https://example.com/x":                 "",
		"":                                      "",
	} {
		if got := repoOfATURI(uri); got != want {
			t.Errorf("repoOfATURI(%q) = %q, want %q", uri, got, want)
		}
	}
}
