package atproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file covers the session-to-client machinery flagged as untested by
// atchess-1c9.33: the 401 refresh-and-retry loop in Client.makeRequest,
// NewClientFromSession's use of an Authenticator, and doRequest's token
// selection. These were previously verified only by throwaway probes during
// review of atchess-1c9.9 -- this file turns those probes into permanent
// regression tests. Style follows the existing inline
// httptest.NewServer(http.HandlerFunc(...)) mock-PDS pattern used in
// client_test.go/client_dpop_test.go; none of the package's other mock-PDS
// helpers fit here (deriveTestPDS in derive_status_test.go is a read-only
// multi-repo derivation fixture with no createRecord/putRecord support, and
// newResolveHandleMockPDS in url_test.go is specific to handle resolution),
// so each test below builds its own small inline fake server, consistent
// with that existing style rather than introducing a fourth reusable helper.

// fakeAuthenticator is a minimal, non-locking atproto.Authenticator double
// for driving Client.makeRequest/doRequest directly. It is intentionally
// simple (no mutex) for tests 1-3, which are single-goroutine; the
// concurrency test (4) below uses a separate, locking double that mirrors
// oauth.Session's real coalescing behaviour.
type fakeAuthenticator struct {
	token    string
	tokenErr error

	refreshToken string
	refreshErr   error
	refreshCalls int32
}

func (a *fakeAuthenticator) Token() (string, error) {
	if a.tokenErr != nil {
		return "", a.tokenErr
	}
	return a.token, nil
}

func (a *fakeAuthenticator) ForceRefresh() (string, error) {
	atomic.AddInt32(&a.refreshCalls, 1)
	if a.refreshErr != nil {
		return "", a.refreshErr
	}
	a.token = a.refreshToken
	return a.token, nil
}

// TestMakeRequest_Persistent401_ExactlyTwoAttemptsOneForceRefresh is
// probe (1) from atchess-1c9.33: a PDS that always returns 401 must cause
// exactly two doRequest attempts and exactly one ForceRefresh call -- never
// an unbounded retry loop.
func TestMakeRequest_Persistent401_ExactlyTwoAttemptsOneForceRefresh(t *testing.T) {
	var attempts int32
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mockPDS.Close()

	auth := &fakeAuthenticator{token: "stale-token", refreshToken: "still-stale-token"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", false, nil, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession failed: %v", err)
	}

	resp, err := client.makeRequest("GET", mockPDS.URL+"/xrpc/some.method", nil)
	if err != nil {
		t.Fatalf("expected makeRequest to return the (401) response after one retry, not an error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected the final response to still be 401, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected exactly 2 request attempts against the PDS, got %d", got)
	}
	if got := atomic.LoadInt32(&auth.refreshCalls); got != 1 {
		t.Errorf("expected exactly 1 ForceRefresh call, got %d", got)
	}
}

// TestMakeRequest_401ThenSuccess_RetriesWithFullBodyAndRefreshedToken is
// probe (2), the most valuable of the four: a write that gets a 401 on its
// first attempt and a 200 on the retry must have its FULL body re-sent on
// the retry (guarding against an already-consumed io.Reader silently
// sending an empty payload) and must carry the refreshed token, not the
// stale one.
func TestMakeRequest_401ThenSuccess_RetriesWithFullBodyAndRefreshedToken(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var authHeaders []string
	attempt := 0

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt++
		thisAttempt := attempt
		mu.Unlock()

		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		bodies = append(bodies, body)
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()

		if thisAttempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uri":"at://did:plc:test/app.atchess.game/abc","cid":"cid1"}`))
	}))
	defer mockPDS.Close()

	auth := &fakeAuthenticator{token: "stale-token", refreshToken: "fresh-token"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", false, nil, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession failed: %v", err)
	}

	payload := []byte(`{"repo":"did:plc:test","collection":"app.atchess.game","record":{"payload":"must-survive-the-retry-byte-for-byte"}}`)

	resp, err := client.makeRequest("POST", mockPDS.URL+"/xrpc/com.atproto.repo.createRecord", payload)
	if err != nil {
		t.Fatalf("expected makeRequest to succeed after refresh+retry, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on the retry, got %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 2 {
		t.Fatalf("expected exactly 2 request attempts, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], payload) {
		t.Errorf("expected the first attempt's body to equal the intended payload exactly, got %q want %q", bodies[0], payload)
	}
	if !bytes.Equal(bodies[1], payload) {
		t.Errorf("expected the retry's body to equal the intended payload exactly (full re-send, not an empty already-consumed body), got %q want %q", bodies[1], payload)
	}

	if authHeaders[0] != "Bearer stale-token" {
		t.Errorf("expected the first attempt to carry the stale token, got %q", authHeaders[0])
	}
	if authHeaders[1] != "Bearer fresh-token" {
		t.Errorf("expected the retry to carry the REFRESHED token, not the stale one, got %q", authHeaders[1])
	}
}

// TestMakeRequest_401_RefreshFailure_SurfacesErrorWithoutFallback is probe
// (3): when ForceRefresh itself fails, makeRequest must surface a wrapped
// error containing "request unauthorized (401) and token refresh failed:"
// and must NOT silently fall back to any other identity by retrying the
// request -- asserted here as "no second attempt was made".
func TestMakeRequest_401_RefreshFailure_SurfacesErrorWithoutFallback(t *testing.T) {
	var attempts int32
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mockPDS.Close()

	refreshErr := errors.New("refresh token revoked")
	auth := &fakeAuthenticator{token: "stale-token", refreshErr: refreshErr}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", false, nil, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession failed: %v", err)
	}

	resp, err := client.makeRequest("GET", mockPDS.URL+"/xrpc/some.method", nil)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error when token refresh fails, got nil")
	}

	const wantPrefix = "request unauthorized (401) and token refresh failed:"
	if !strings.Contains(err.Error(), wantPrefix) {
		t.Errorf("expected error to contain %q, got %q", wantPrefix, err.Error())
	}
	if !errors.Is(err, refreshErr) {
		t.Errorf("expected error to wrap the underlying refresh error (%v), got %v", refreshErr, err)
	}

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 request attempt (no fallback retry using some other identity) after a failed refresh, got %d", got)
	}
	if got := atomic.LoadInt32(&auth.refreshCalls); got != 1 {
		t.Errorf("expected exactly 1 ForceRefresh call, got %d", got)
	}
}

// lockingFakeAuthenticator mirrors oauth.Session's real EnsureFresh/
// ForceRefresh locking semantics (check freshness under a mutex, refresh at
// most once, everyone observes the result) without pulling in
// internal/oauth or internal/web (which imports this package, so cannot be
// imported back from here). Token() performs its "refresh" by making a real
// HTTP call to refreshURL, so the concurrency test below can gate that call
// on a channel and prove genuine contention rather than accidental
// sequential execution.
type lockingFakeAuthenticator struct {
	refreshURL string
	client     *http.Client

	mu    sync.Mutex
	fresh bool
	token string

	refreshCalls int32
}

func newLockingFakeAuthenticator(refreshURL string) *lockingFakeAuthenticator {
	return &lockingFakeAuthenticator{
		refreshURL: refreshURL,
		client:     &http.Client{Timeout: 10 * time.Second},
		token:      "initial-token",
	}
}

func (a *lockingFakeAuthenticator) Token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fresh {
		return a.token, nil
	}
	return a.refreshLocked()
}

func (a *lockingFakeAuthenticator) ForceRefresh() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked()
}

// refreshLocked must be called with a.mu held.
func (a *lockingFakeAuthenticator) refreshLocked() (string, error) {
	atomic.AddInt32(&a.refreshCalls, 1)
	resp, err := a.client.Get(a.refreshURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessJwt string `json:"accessJwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	a.token = body.AccessJwt
	a.fresh = true
	return a.token, nil
}

// TestClient_ConcurrentTokenRefresh_CoalescesAndDoesNotDeadlock is probe
// (4)'s coalescing half: N goroutines calling Token() concurrently on a
// freshly-expired Authenticator must cause exactly ONE underlying refresh
// call to reach the server, and all N goroutines must complete within a
// bounded deadline (no deadlock). The refresh HTTP call is gated on a
// channel and only released once the test has observed the one real refresh
// reach the server, so the completion of all N goroutines afterwards is
// genuinely evidence of the other N-1 having been blocked waiting on the
// Authenticator's lock (real contention), not of accidental sequential
// scheduling.
//
// Invariant asserted: arrivedAtServer (the number of real refresh calls
// that reached the fake server) is exactly 1, strictly fewer than the
// n=10 concurrent callers.
func TestClient_ConcurrentTokenRefresh_CoalescesAndDoesNotDeadlock(t *testing.T) {
	const n = 10

	gate := make(chan struct{})
	var arrivedAtServer int32

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&arrivedAtServer, 1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"accessJwt": "refreshed-token"})
	}))
	defer refreshSrv.Close()

	auth := newLockingFakeAuthenticator(refreshSrv.URL)

	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _ = auth.Token()
			done <- struct{}{}
		}()
	}

	// Wait (bounded, condition-polled -- not a fixed guessed sleep) for the
	// single real refresh call to reach the gated server.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&arrivedAtServer) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the coalesced refresh call to reach the gated server")
		}
		time.Sleep(time.Millisecond)
	}

	// At this point exactly one goroutine holds auth.mu and is blocked in
	// the gated HTTP call; the other n-1 goroutines are blocked trying to
	// acquire auth.mu (real contention). Release them all at once.
	close(gate)

	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("deadlock: not all concurrent Token() callers completed")
		}
	}

	if got := atomic.LoadInt32(&arrivedAtServer); got != 1 {
		t.Errorf("expected exactly 1 real refresh call to reach the server despite %d concurrent Token() callers (refreshes must coalesce), got %d", n, got)
	}
}

// TestClient_ConcurrentTokenAndForceRefresh_NoDeadlock is probe (4)'s
// no-deadlock half for the mixed EnsureFresh/ForceRefresh case: concurrent
// Token() (the analogue of oauth.Session.EnsureFresh) and ForceRefresh()
// calls on the same Authenticator must all complete within a bounded
// deadline. Unlike Token(), ForceRefresh() always performs a real refresh
// by design (it must, to recover from server-side revocation), so this test
// does not assert a coalescing count -- only that mixing the two never
// deadlocks.
func TestClient_ConcurrentTokenAndForceRefresh_NoDeadlock(t *testing.T) {
	const n = 10

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"accessJwt": "refreshed-token"})
	}))
	defer refreshSrv.Close()

	auth := newLockingFakeAuthenticator(refreshSrv.URL)

	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := auth.Token(); err != nil {
				errs <- err
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := auth.ForceRefresh(); err != nil {
				errs <- err
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: concurrent Token()/ForceRefresh() calls did not all complete")
	}

	close(errs)
	for err := range errs {
		t.Errorf("unexpected error from concurrent Token()/ForceRefresh(): %v", err)
	}
}
