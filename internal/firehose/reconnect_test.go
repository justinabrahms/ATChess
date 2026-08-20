package firehose

import (
	"context"
	"fmt"
	"math/rand"
	"net"
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

// TestClient_StopPromptDuringMidDial verifies Stop() returns quickly even
// while connect() is mid-dial to a host that accepts the TCP connection but
// never responds -- the scenario defect A covers. On the pre-fix code this
// blocks for the full ~30s dial timeout because gorilla/websocket's
// DialContext only consults the context for the initial TCP dial; for the
// subsequent HTTP upgrade write/read (where a wedged host actually hangs) it
// converts ctx.Deadline() into a single absolute net.Conn.SetDeadline call
// made once up front, and never notices ctx.Done() firing early. A raw TCP
// listener (not an httptest/websocket server) is used deliberately so the
// server never completes -- or even attempts -- the HTTP handshake.
func TestClient_StopPromptDuringMidDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Accept the connection but never write anything back and never
		// close it -- the client's dial goroutine is left blocked reading
		// an HTTP response that will never arrive.
		<-time.After(time.Minute)
		conn.Close()
	}()

	url := fmt.Sprintf("ws://%s/", ln.Addr().String())
	handler := func(event Event) error { return nil }

	client := NewClient(handler, WithURL(url))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Synchronize on the listener having actually accepted the connection,
	// so the test genuinely reaches the mid-dial state rather than racing
	// Stop() ahead of the dial even starting.
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener never accepted a connection; test did not reach mid-dial state")
	}
	// Give the client a brief grace period to be inside the blocked
	// handshake read (accept happening is necessary but not by itself
	// sufficient to guarantee the client has moved past writing its
	// request).
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if err := client.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Stop() took %v while mid-dial to a wedged (accept-but-never-respond) listener, expected it to return promptly", elapsed)
	}
}

// TestClient_StopBetweenDialAndStore_ClosesLeakedSocket verifies defect B:
// if Stop() lands between a successful dial and connect() taking c.mu to
// store the resulting connection, the freshly dialed socket must be closed
// and NOT stored/marked connected -- otherwise the client believes it holds
// a live connection that is actually leaked and unowned.
//
// The window between DialContext returning and c.mu.Lock() in the real
// connect() path is too narrow to hit reliably by timing alone, so this
// drives the exact sequence deterministically by calling the client's own
// dial and storeConn steps directly (both already exist as production code,
// not test-only additions) in the order that reproduces the race: dial
// succeeds, THEN Stop (c.cancel()) happens, THEN storeConn runs.
func TestClient_StopBetweenDialAndStore_ClosesLeakedSocket(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
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

	conn, _, err := client.dialer.DialContext(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	// Simulate Stop() having already happened between the successful dial
	// above and connect() taking c.mu to store it.
	client.cancel()

	connCtx, err := client.storeConn(conn)
	if err == nil {
		t.Fatalf("expected storeConn to report an error after Stop, got nil (connCtx=%v)", connCtx)
	}
	if client.IsConnected() {
		t.Error("client reports connected after a post-Stop dial landed; socket leaked as live")
	}
	if client.getConn() != nil {
		t.Error("client stored a connection dialed after Stop")
	}

	// Confirm the dialed socket was actually closed by storeConn, not
	// merely forgotten about: a write to it should now fail.
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err := conn.WriteMessage(websocket.PingMessage, nil); err == nil {
		t.Error("expected write to the socket to fail after storeConn closed it post-Stop, but it succeeded")
	}
}
