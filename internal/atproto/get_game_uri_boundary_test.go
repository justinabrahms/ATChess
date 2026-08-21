package atproto

import (
	"context"
	"errors"
	"testing"
	"time"
)

// This file is a reviewer-directed follow-up on atchess-1c9.67: GetGame's
// malformed-URI guard (len(parts) < 4) was stale relative to its sibling
// getGameRecord's (len(parts) < 5) -- both split gameURI on "/" and read
// parts[4] for the rkey, but GetGame's guard let a 4-part URI (one with no
// rkey segment at all, e.g. "at://did:plc:x/app.atchess.game") through,
// which then panicked on the parts[4] index-out-of-range instead of
// returning the ErrInvalidGameURI/400 the bead's done-criteria require.
// These tests pin the boundary on BOTH sides: a 4-part URI must be
// rejected (not panic), and a well-formed 5-part URI must NOT be rejected
// as invalid.

// TestGetGame_FourPartURI_RejectedNotPanic is the exact panic case the
// reviewer proved: a game URI with a repo and collection but no rkey
// segment splits into exactly 4 "/"-separated parts. Before the guard fix
// this indexed parts[4] out of range and panicked; it must instead return
// an error wrapping ErrInvalidGameURI (400 at the handler layer).
func TestGetGame_FourPartURI_RejectedNotPanic(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()
	client := newDeriveTestClient(t, mock)

	const gameURI = "at://did:plc:x/app.atchess.game" // 4 parts: "at:", "", "did:plc:x", "app.atchess.game"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetGame panicked on malformed 4-part URI %q instead of returning an error: %v", gameURI, r)
		}
	}()

	_, err := client.GetGame(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected an error for malformed URI %q, got nil", gameURI)
	}
	if !errors.Is(err, ErrInvalidGameURI) {
		t.Fatalf("expected error to wrap ErrInvalidGameURI, got: %v", err)
	}
}

// TestGetGame_FivePartURI_NotRejectedAsInvalid pins the other side of the
// boundary: a well-formed 5-part game URI (repo/collection/rkey all
// present) must never be rejected as ErrInvalidGameURI, regardless of
// whether the record itself exists.
func TestGetGame_FivePartURI_NotRejectedAsInvalid(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()
	client := newDeriveTestClient(t, mock)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		if errors.Is(err, ErrInvalidGameURI) {
			t.Fatalf("well-formed 5-part URI %q was rejected as invalid: %v", gameURI, err)
		}
		t.Fatalf("unexpected error for well-formed, seeded game URI %q: %v", gameURI, err)
	}
	if game == nil {
		t.Fatalf("expected a non-nil game for well-formed, seeded game URI %q", gameURI)
	}
}
