package firehose

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newStreamingMockServer streams a burst of messages as fast as possible on
// every connection, so listen()'s loop (and its per-iteration c.conn read)
// executes many times per connection, maximizing the chance of overlapping
// with a concurrent Stop().
func newStreamingMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	var seq int64
	var seqMu sync.Mutex

	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for i := 0; i < 2000; i++ {
			seqMu.Lock()
			seq++
			s := seq
			seqMu.Unlock()

			msg := createTestMessage(s, "app.atchess.move", map[string]interface{}{
				"gameID": "g",
				"move":   "e2e4",
			})
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		}
	}))
}

// TestClient_ConcurrentReconnectAndRead_Race guards against a regression of
// the unsynchronized c.conn read in listen(): it keeps the read side hot
// (a continuous message stream drives repeated loop iterations, each
// re-reading the connection) while concurrently calling Stop() from another
// goroutine after a short, randomized delay, across many trials. On the
// pre-fix code this reliably triggered "WARNING: DATA RACE" between
// listen()'s unsynchronized field read and Stop()'s locked write under
// `go test -race`.
func TestClient_ConcurrentReconnectAndRead_Race(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		server := newStreamingMockServer(t)

		url := "ws" + strings.TrimPrefix(server.URL, "http")
		handler := func(event Event) error { return nil }

		client := NewClient(handler, WithURL(url), WithInitialReconnectDelay(time.Millisecond))

		if err := client.Start(); err != nil {
			server.Close()
			t.Fatalf("trial %d: Start failed: %v", trial, err)
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.Intn(500)) * time.Microsecond)
			_ = client.Stop()
		}()

		wg.Wait()
		server.Close()
	}
}

// dropOnceMockServer accepts connections and, for the first N connections,
// writes one message and then closes — forcing the client through
// handleReconnect/connect repeatedly. It reports each accepted connection
// on connectedCh so the test can pace itself against real reconnect events
// instead of guessing with sleeps.
func newDropOnceMockServer(t *testing.T, connectedCh chan<- int) *httptest.Server {
	t.Helper()
	var count int
	var mu sync.Mutex

	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		mu.Lock()
		count++
		n := count
		mu.Unlock()

		msg := createTestMessage(int64(n), "app.atchess.move", map[string]interface{}{
			"gameID": "g",
			"move":   "e2e4",
		})
		_ = conn.WriteMessage(websocket.BinaryMessage, msg)

		select {
		case connectedCh <- n:
		default:
		}
		// Close immediately to force a reconnect.
	}))
}

// TestClient_PingLoopGoroutineCountStableAcrossReconnects guards against
// pingLoop goroutine accumulation: on the pre-fix code, each reconnect
// spawned a new pingLoop that never exited (it silently adopted whatever
// connection happened to be current), so the live goroutine count grew by
// roughly one per reconnect. With the fix, old ping loops terminate as soon
// as their connection is superseded, so the count stays flat regardless of
// how many reconnects occur.
func TestClient_PingLoopGoroutineCountStableAcrossReconnects(t *testing.T) {
	connectedCh := make(chan int, 64)
	server := newDropOnceMockServer(t, connectedCh)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	handler := func(event Event) error { return nil }

	client := NewClient(handler, WithURL(url), WithInitialReconnectDelay(time.Millisecond))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer client.Stop()

	waitForConnections := func(n int) {
		t.Helper()
		deadline := time.After(20 * time.Second)
		seen := 0
		for seen < n {
			select {
			case <-connectedCh:
				seen++
			case <-deadline:
				t.Fatalf("timed out waiting for %d connections, saw %d", n, seen)
			}
		}
	}

	// Warm up: let a couple of reconnects happen and settle so the
	// baseline goroutine count reflects steady-state (one run() + one live
	// pingLoop), not startup noise.
	waitForConnections(2)
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	// Drive N (5) more forced reconnects.
	const n = 5
	waitForConnections(n)
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	t.Logf("goroutines before additional reconnects: %d, after: %d", before, after)

	const tolerance = 2
	if after > before+tolerance {
		t.Errorf("goroutine count grew by %d (before=%d after=%d), want <= %d: ping loops are likely accumulating across reconnects",
			after-before, before, after, tolerance)
	}
}

// TestClient_StopPromptDuringBlockedRead verifies Stop() returns quickly
// even while listen() is blocked inside a ReadMessage call with no
// messages arriving.
func TestClient_StopPromptDuringBlockedRead(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Never send anything; block until the client closes.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	handler := func(event Event) error { return nil }

	client := NewClient(handler, WithURL(url))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // ensure we are blocked in ReadMessage

	start := time.Now()
	if err := client.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Stop() took %v while blocked on read, expected it to return promptly", elapsed)
	}
}

// TestClient_StopPromptDuringReconnectBackoff verifies Stop() returns
// quickly even while the client is sleeping in handleReconnect's backoff.
func TestClient_StopPromptDuringReconnectBackoff(t *testing.T) {
	handler := func(event Event) error { return nil }

	// Point at a URL with nothing listening so connect() fails immediately
	// and the client enters the backoff sleep in handleReconnect.
	client := NewClient(handler,
		WithURL("ws://127.0.0.1:1/does-not-exist"),
		WithInitialReconnectDelay(time.Hour), // long backoff, must be interrupted by Stop
	)
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // ensure we're in the backoff sleep

	start := time.Now()
	_ = client.Stop()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Stop() took %v during reconnect backoff, expected it to return promptly", elapsed)
	}
}
