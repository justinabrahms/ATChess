package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
)

// TestNonceStore_ConcurrentGetSet is atchess-1c9.12's explicit concurrency
// requirement: the nonce store is shared mutable state read and written by
// concurrent requests (every OAuth-bound HTTP request in internal/web
// potentially reads and writes it), so it must be race-safe. Run with
// `go test -race` (as the project's default suite does) to actually catch
// a data race; without -race this test only proves the store doesn't
// deadlock or panic under contention.
func TestNonceStore_ConcurrentGetSet(t *testing.T) {
	store := NewNonceStore()
	const goroutines = 50
	const opsPerGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			origin := fmt.Sprintf("https://origin-%d.example.com", g%5)
			for i := 0; i < opsPerGoroutine; i++ {
				store.Set(origin, fmt.Sprintf("nonce-%d-%d", g, i))
				_ = store.Get(origin)
			}
		}(g)
	}
	wg.Wait()
}

func TestNonceStore_GetSetRoundTrip(t *testing.T) {
	store := NewNonceStore()
	if got := store.Get("https://example.com"); got != "" {
		t.Fatalf("Get on empty store = %q, want empty", got)
	}
	store.Set("https://example.com", "nonce-1")
	if got := store.Get("https://example.com"); got != "nonce-1" {
		t.Fatalf("Get after Set = %q, want %q", got, "nonce-1")
	}
	// A different origin is unaffected.
	if got := store.Get("https://other.example.com"); got != "" {
		t.Fatalf("Get for a different origin = %q, want empty", got)
	}
	// Overwrite reflects the newest value (nonce rotation).
	store.Set("https://example.com", "nonce-2")
	if got := store.Get("https://example.com"); got != "nonce-2" {
		t.Fatalf("Get after second Set = %q, want %q", got, "nonce-2")
	}
	// Empty nonce/origin are no-ops, not stored.
	store.Set("https://example.com", "")
	if got := store.Get("https://example.com"); got != "nonce-2" {
		t.Fatalf("Set with empty nonce must be a no-op, got %q", got)
	}
	store.Set("", "nonce-3")
	if got := store.Get(""); got != "" {
		t.Fatalf("Set with empty origin must be a no-op, got %q", got)
	}
}

func TestOriginOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://bsky.social/oauth/par", "https://bsky.social"},
		{"https://bsky.social:8443/oauth/token?x=1", "https://bsky.social:8443"},
		{"not a url", ""},
		{"/just/a/path", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := OriginOf(c.in); got != c.want {
			t.Errorf("OriginOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCreateProof_ClaimsAndNonce(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	proof, err := CreateProof(key, "POST", "https://example.com/token", "sometoken", "somenonce")
	if err != nil {
		t.Fatalf("CreateProof: %v", err)
	}
	if proof == "" {
		t.Fatal("CreateProof returned an empty proof")
	}
}
