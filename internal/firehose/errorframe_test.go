package firehose

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/rs/zerolog"
)

// createCBORErrorFrame builds a real CBOR-encoded AT Protocol firehose
// error frame: header {op: -1} (no "t") followed by body {error, message}.
func createCBORErrorFrame(t *testing.T, code, message string) []byte {
	t.Helper()

	var buf bytes.Buffer

	headerBuilder := basicnode.Prototype.Map.NewBuilder()
	hma, _ := headerBuilder.BeginMap(1)
	hma.AssembleKey().AssignString("op")
	hma.AssembleValue().AssignInt(-1)
	hma.Finish()
	if err := dagcbor.Encode(headerBuilder.Build(), &buf); err != nil {
		t.Fatalf("failed to encode CBOR error header: %v", err)
	}

	bodyBuilder := basicnode.Prototype.Map.NewBuilder()
	bma, _ := bodyBuilder.BeginMap(2)
	bma.AssembleKey().AssignString("error")
	bma.AssembleValue().AssignString(code)
	bma.AssembleKey().AssignString("message")
	bma.AssembleValue().AssignString(message)
	bma.Finish()
	if err := dagcbor.Encode(bodyBuilder.Build(), &buf); err != nil {
		t.Fatalf("failed to encode CBOR error body: %v", err)
	}

	return buf.Bytes()
}

// createCBORInfoFrame builds a real CBOR-encoded AT Protocol firehose
// #info frame: header {op: 1, t: "#info"} followed by body {name, message}.
func createCBORInfoFrame(t *testing.T, name, message string) []byte {
	t.Helper()

	var buf bytes.Buffer

	headerBuilder := basicnode.Prototype.Map.NewBuilder()
	hma, _ := headerBuilder.BeginMap(2)
	hma.AssembleKey().AssignString("op")
	hma.AssembleValue().AssignInt(1)
	hma.AssembleKey().AssignString("t")
	hma.AssembleValue().AssignString("#info")
	hma.Finish()
	if err := dagcbor.Encode(headerBuilder.Build(), &buf); err != nil {
		t.Fatalf("failed to encode CBOR info header: %v", err)
	}

	bodyBuilder := basicnode.Prototype.Map.NewBuilder()
	bma, _ := bodyBuilder.BeginMap(2)
	bma.AssembleKey().AssignString("name")
	bma.AssembleValue().AssignString(name)
	bma.AssembleKey().AssignString("message")
	bma.AssembleValue().AssignString(message)
	bma.Finish()
	if err := dagcbor.Encode(bodyBuilder.Build(), &buf); err != nil {
		t.Fatalf("failed to encode CBOR info body: %v", err)
	}

	return buf.Bytes()
}

// TestClient_FutureCursorErrorFrame_ResetsCursorAndReconnects verifies that
// a FutureCursor error frame (the host rejecting our requested cursor as
// invalid -- "too old"/nonexistent) is handled explicitly rather than
// crashing the client or looping forever retrying the same rejected
// cursor: the client's in-memory LastSequence is reset to -1 (no cursor /
// live tip) so the next reconnect requests the live tip instead.
func TestClient_FutureCursorErrorFrame_ResetsCursorAndReconnects(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var connCount int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		mu.Lock()
		connCount++
		n := connCount
		mu.Unlock()

		if n == 1 {
			// First connection: reject with FutureCursor and close.
			_ = conn.WriteMessage(websocket.BinaryMessage, createCBORErrorFrame(t, "FutureCursor", "cursor is in the future"))
			return
		}

		// Second connection (the reconnect): stay open briefly so the test
		// can observe it happened, then let the test's server.Close() tear
		// it down.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	handler := func(event Event) error { return nil }
	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler, WithURL(url), WithLogger(logger), WithCursor(999999999))

	if err := client.Start(); err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	defer client.Stop()

	// Wait for the FutureCursor rejection to be processed and a reconnect
	// to happen.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := connCount
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	n := connCount
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected the client to reconnect after a FutureCursor error frame, but only saw %d connection(s)", n)
	}

	if seq := client.LastSequence(); seq != -1 {
		t.Errorf("expected LastSequence to be reset to -1 (no cursor / live tip) after a FutureCursor error frame, got %d", seq)
	}
}

// TestClient_OutdatedCursorInfoFrame_DoesNotDisconnect verifies that an
// #info OutdatedCursor frame -- advisory, not an error -- does not tear
// down the connection or crash the client; commits that follow it on the
// same connection are still processed normally.
func TestClient_OutdatedCursorInfoFrame_DoesNotDisconnect(t *testing.T) {
	messages := [][]byte{
		createCBORInfoFrame(t, "OutdatedCursor", "requested cursor is older than this host's retention window"),
		createCBORTestMessage(t, 42, "did:plc:cboruser", "app.atchess.move"),
	}

	server := newMockWebSocketServer(messages)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	var events []Event
	var mu sync.Mutex
	handler := func(event Event) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	}

	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler, WithURL(url), WithLogger(logger))

	if err := client.Start(); err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	defer client.Stop()

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected the commit event following the #info frame to still be processed, got %d event(s)", len(events))
	}
	if !client.IsConnected() {
		t.Errorf("expected the client to remain connected after an #info frame (advisory, not an error)")
	}
}
