package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Sessions surviving a restart is the whole feature. Before this they lived in
// a map and nowhere else, so every deploy logged out every user — reported
// 2026-08-30 as "when I refresh, I'm logged out" during five deploys in an
// afternoon.
func TestSessionsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	first := NewSessionStore()
	if err := first.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}
	id := first.CreateSession(&Session{
		DID: "did:plc:someone", Handle: "someone.test",
		PDSURL: "https://pds.test", AccessToken: "at", RefreshToken: "rt",
		DPoPKey: key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if id == "" {
		t.Fatal("CreateSession returned no id")
	}

	// A brand new store, as after a restart.
	second := NewSessionStore()
	if err := second.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence (restart): %v", err)
	}
	got, err := second.GetSession(id)
	if err != nil {
		t.Fatalf("the session did not survive the restart: %v\n"+
			"This is what logged every user out on every deploy.", err)
	}
	if got.DID != "did:plc:someone" || got.RefreshToken != "rt" {
		t.Errorf("session came back wrong: did=%q refresh=%q", got.DID, got.RefreshToken)
	}

	// THE DPoP KEY IS THE HALF THAT FAILS SILENTLY. MarshalJSON used to drop
	// private keys on purpose. A restored OAuth session without its key looks
	// perfectly valid and cannot sign a single request — worse than being
	// logged out, because it fails later and for no visible reason.
	if got.DPoPKey == nil {
		t.Fatal("the DPoP key was not restored; the session would look valid and fail every signed request")
	}
	if got.DPoPKey.D.Cmp(key.D) != 0 {
		t.Error("a different DPoP key came back than went in")
	}
}

// The file holds refresh tokens and DPoP private keys.
func TestSessionFileIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	s := NewSessionStore()
	if err := s.EnablePersistence(path); err != nil {
		t.Fatal(err)
	}
	s.CreateSession(&Session{DID: "did:plc:x", ExpiresAt: time.Now().Add(time.Hour)})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no session file was written: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("session file mode is %o; it holds refresh tokens and private keys and must not be group- or world-readable", perm)
	}
}

// A corrupt file must not keep the service down. Losing sessions is bad;
// refusing to start is worse.
func TestCorruptSessionFileDoesNotPreventStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSessionStore()
	err := s.EnablePersistence(path)
	if err == nil {
		t.Error("a corrupt session file was accepted silently; the caller cannot log that anything was lost")
	}
	// The store must still be usable afterwards.
	if id := s.CreateSession(&Session{DID: "did:plc:x", ExpiresAt: time.Now().Add(time.Hour)}); id == "" {
		t.Error("the store was left unusable by a corrupt file")
	}
}
