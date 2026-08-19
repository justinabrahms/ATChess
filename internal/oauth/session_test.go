package oauth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSession_EnsureFresh_NoRefreshWhenNotExpired verifies EnsureFresh
// returns the current access token without calling refreshFn when the
// session is not yet within the expiry skew window.
func TestSession_EnsureFresh_NoRefreshWhenNotExpired(t *testing.T) {
	session := &Session{
		AccessToken:          "still-good",
		RefreshToken:         "refresh-token",
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	var calls int32
	refreshFn := func(refreshToken string) (string, string, time.Time, error) {
		atomic.AddInt32(&calls, 1)
		return "new-access", "new-refresh", time.Now().Add(time.Hour), nil
	}

	token, err := session.EnsureFresh(refreshFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "still-good" {
		t.Errorf("expected unchanged access token, got %q", token)
	}
	if calls != 0 {
		t.Errorf("expected refreshFn NOT to be called, got %d calls", calls)
	}
}

// TestSession_EnsureFresh_RefreshesWhenExpired verifies EnsureFresh calls
// refreshFn and updates AccessToken/RefreshToken/AccessTokenExpiresAt (but
// not the session-level ExpiresAt) when the access token is expired.
func TestSession_EnsureFresh_RefreshesWhenExpired(t *testing.T) {
	sessionLifetime := time.Now().Add(24 * time.Hour)
	session := &Session{
		AccessToken:          "stale",
		RefreshToken:         "refresh-token",
		ExpiresAt:            sessionLifetime,
		AccessTokenExpiresAt: time.Now().Add(-time.Minute), // already expired
	}

	refreshFn := func(refreshToken string) (string, string, time.Time, error) {
		if refreshToken != "refresh-token" {
			t.Errorf("expected refreshFn to receive the session's refresh token, got %q", refreshToken)
		}
		return "fresh-access", "fresh-refresh", time.Now().Add(time.Hour), nil
	}

	token, err := session.EnsureFresh(refreshFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "fresh-access" {
		t.Errorf("expected refreshed access token, got %q", token)
	}
	if session.RefreshToken != "fresh-refresh" {
		t.Errorf("expected refresh token to be updated, got %q", session.RefreshToken)
	}
	if !session.ExpiresAt.Equal(sessionLifetime) {
		t.Errorf("session-level ExpiresAt must NOT be changed by a token refresh; got %v, want %v", session.ExpiresAt, sessionLifetime)
	}
}

// TestSession_EnsureFresh_PropagatesRefreshError verifies a failing
// refreshFn surfaces its error rather than silently returning a stale
// token.
func TestSession_EnsureFresh_PropagatesRefreshError(t *testing.T) {
	session := &Session{
		AccessToken:          "stale",
		RefreshToken:         "refresh-token",
		AccessTokenExpiresAt: time.Now().Add(-time.Minute),
	}

	refreshFn := func(refreshToken string) (string, string, time.Time, error) {
		return "", "", time.Time{}, fmt.Errorf("refresh failed: revoked")
	}

	if _, err := session.EnsureFresh(refreshFn); err == nil {
		t.Fatal("expected an error when refreshFn fails, got nil")
	}
}

// TestSession_ForceRefresh_IgnoresExpiry verifies ForceRefresh always
// refreshes, even when the session isn't near expiry -- used after an
// unexpected 401 from the PDS.
func TestSession_ForceRefresh_IgnoresExpiry(t *testing.T) {
	session := &Session{
		AccessToken:          "still-technically-valid-locally",
		RefreshToken:         "refresh-token",
		AccessTokenExpiresAt: time.Now().Add(time.Hour), // NOT locally expired
	}

	var calls int32
	refreshFn := func(refreshToken string) (string, string, time.Time, error) {
		atomic.AddInt32(&calls, 1)
		return "forced-fresh", "forced-fresh-refresh", time.Now().Add(time.Hour), nil
	}

	token, err := session.ForceRefresh(refreshFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "forced-fresh" {
		t.Errorf("expected forced refresh to replace the token, got %q", token)
	}
	if calls != 1 {
		t.Errorf("expected refreshFn to be called exactly once, got %d", calls)
	}
}

// TestSession_EnsureFresh_ConcurrentCallsCoalesceToOneRefresh is the
// explicit "concurrent requests on one session refreshing simultaneously"
// edge case called out in atchess-1c9.9's brief: many goroutines calling
// EnsureFresh at once on an expired session must serialize on s.mu so at
// most one refreshFn call happens, and every caller observes the resulting
// fresh token.
func TestSession_EnsureFresh_ConcurrentCallsCoalesceToOneRefresh(t *testing.T) {
	session := &Session{
		AccessToken:          "stale",
		RefreshToken:         "refresh-token",
		AccessTokenExpiresAt: time.Now().Add(-time.Minute),
	}

	var calls int32
	refreshFn := func(refreshToken string) (string, string, time.Time, error) {
		atomic.AddInt32(&calls, 1)
		// Simulate network latency so concurrent callers actually overlap.
		time.Sleep(20 * time.Millisecond)
		return "coalesced-fresh", "coalesced-fresh-refresh", time.Now().Add(time.Hour), nil
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = session.EnsureFresh(refreshFn)
		}(i)
	}
	wg.Wait()

	if calls != 1 {
		t.Errorf("expected exactly 1 refreshFn call across %d concurrent EnsureFresh callers, got %d", n, calls)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: unexpected error: %v", i, err)
		}
		if results[i] != "coalesced-fresh" {
			t.Errorf("caller %d: expected coalesced-fresh, got %q", i, results[i])
		}
	}
}
