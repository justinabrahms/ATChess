package firehose

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/rs/zerolog"
)

// expectedWantedCollections is a hardcoded, independent copy of the NSIDs
// WantedCollections is expected to contain -- deliberately NOT derived from
// WantedCollections itself. Tests that iterate this instead of
// WantedCollections directly still catch a regression that empties or
// shrinks the production list (iterating the mutated variable itself would
// make the test vacuously pass: an empty range loop runs zero iterations,
// so a bug that empties WantedCollections would otherwise go undetected --
// this was caught during atchess-1c9.49 mutation testing).
var expectedWantedCollections = []string{
	"app.atchess.challenge",
	"app.atchess.challengeResponse",
	"app.atchess.drawOffer",
	"app.atchess.drawResponse",
	"app.atchess.game",
	"app.atchess.move",
	"app.atchess.resignation",
	"app.atchess.timeViolation",
}

// newJetstreamMockServer streams messages as TextMessage frames (Jetstream's
// wire framing for its JSON events), then keeps the connection open reading
// (and discarding) whatever the client sends, mirroring
// newMockWebSocketServer in client_test.go but for the Jetstream transport.
func newJetstreamMockServer(t *testing.T, messages [][]byte) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for _, msg := range messages {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

// jetstreamDialURL turns an httptest.Server's http:// URL into the
// ws://.../subscribe URL a Jetstream client would be pointed at -- the
// "/subscribe" suffix is what firehose.DetectTransport keys off of.
func jetstreamDialURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/subscribe"
}

// jetstreamCommitJSON builds one Jetstream "commit" event JSON message, per
// https://github.com/bluesky-social/jetstream's documented shape:
// {"did","time_us","kind":"commit","commit":{"rev","operation","collection","rkey","record","cid"}}.
func jetstreamCommitJSON(t *testing.T, did string, timeUS int64, operation, collection, rkey, cid string, record interface{}) []byte {
	t.Helper()

	var recJSON json.RawMessage
	if record != nil {
		b, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("failed to marshal test record: %v", err)
		}
		recJSON = b
	}

	evt := struct {
		DID    string `json:"did"`
		TimeUS int64  `json:"time_us"`
		Kind   string `json:"kind"`
		Commit struct {
			Rev        string          `json:"rev"`
			Operation  string          `json:"operation"`
			Collection string          `json:"collection"`
			RKey       string          `json:"rkey"`
			Record     json.RawMessage `json:"record,omitempty"`
			CID        string          `json:"cid,omitempty"`
		} `json:"commit"`
	}{DID: did, TimeUS: timeUS, Kind: "commit"}
	evt.Commit.Rev = "rev1"
	evt.Commit.Operation = operation
	evt.Commit.Collection = collection
	evt.Commit.RKey = rkey
	evt.Commit.Record = recJSON
	evt.Commit.CID = cid

	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("failed to marshal test jetstream commit event: %v", err)
	}
	return b
}

// TestJetstream_CommitEventsDecodeForEachChessCollection verifies that a
// commit event for every collection in WantedCollections (the closed list
// of app.atchess.* NSIDs this deployment actually writes) decodes into the
// same Event shape processCBORMessage produces, so downstream
// EventHandler/ChessEventProcessor code is transport-agnostic.
func TestJetstream_CommitEventsDecodeForEachChessCollection(t *testing.T) {
	if len(WantedCollections) != len(expectedWantedCollections) {
		t.Fatalf("WantedCollections has %d entries, want %d (expectedWantedCollections); update expectedWantedCollections if this is an intentional lexicon addition", len(WantedCollections), len(expectedWantedCollections))
	}
	for _, collection := range expectedWantedCollections {
		collection := collection
		t.Run(collection, func(t *testing.T) {
			msg := jetstreamCommitJSON(t, "did:plc:collectiontest", 5000, "create", collection, "rkey1", "cidZ", map[string]interface{}{"field": "value"})
			server := newJetstreamMockServer(t, [][]byte{msg})
			defer server.Close()

			var events []Event
			var mu sync.Mutex
			handler := func(event Event) error {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
				return nil
			}

			client := NewClient(handler, WithURL(jetstreamDialURL(server)))
			if client.Transport() != TransportJetstream {
				t.Fatalf("expected auto-detected TransportJetstream for a /subscribe URL, got %v", client.Transport())
			}
			if err := client.Start(); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			defer client.Stop()

			time.Sleep(150 * time.Millisecond)

			mu.Lock()
			defer mu.Unlock()
			if len(events) != 1 {
				t.Fatalf("expected 1 event for collection %s, got %d", collection, len(events))
			}

			wantPath := collection + "/rkey1"
			wantType := getEventType(wantPath)
			if events[0].Type != wantType {
				t.Errorf("event type = %v, want %v", events[0].Type, wantType)
			}
			if events[0].Path != wantPath {
				t.Errorf("event path = %q, want %q", events[0].Path, wantPath)
			}
			if events[0].Repo != "did:plc:collectiontest" {
				t.Errorf("event repo = %q, want did:plc:collectiontest", events[0].Repo)
			}
			if events[0].CID != "cidZ" {
				t.Errorf("event CID = %q, want cidZ", events[0].CID)
			}
			if events[0].Timestamp.UnixMicro() != 5000 {
				t.Errorf("event timestamp = %v (unix micro %d), want unix micro 5000", events[0].Timestamp, events[0].Timestamp.UnixMicro())
			}
		})
	}
}

// TestJetstream_NonCommitKindsIgnored verifies that "identity" and
// "account" Jetstream events (which carry no "commit" field at all) are
// silently ignored -- no handler call, no crash -- while the cursor (from
// time_us) still advances, since Jetstream's cursor semantics are "resume
// the stream from this point in time", not "resume from the last commit
// specifically".
func TestJetstream_NonCommitKindsIgnored(t *testing.T) {
	identity := []byte(`{"did":"did:plc:user","time_us":2000,"kind":"identity"}`)
	account := []byte(`{"did":"did:plc:user","time_us":3000,"kind":"account"}`)

	server := newJetstreamMockServer(t, [][]byte{identity, account})
	defer server.Close()

	var eventCount int
	var mu sync.Mutex
	handler := func(event Event) error {
		mu.Lock()
		eventCount++
		mu.Unlock()
		return nil
	}

	client := NewClient(handler, WithURL(jetstreamDialURL(server)))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer client.Stop()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	n := eventCount
	mu.Unlock()
	if n != 0 {
		t.Errorf("expected identity/account events to be ignored (no handler calls), got %d", n)
	}

	if seq := client.LastSequence(); seq != 3000 {
		t.Errorf("expected LastSequence to advance to the latest time_us seen (3000) even for ignored event kinds, got %d", seq)
	}
}

// TestJetstream_MalformedJSONDoesNotKillListener verifies that a frame
// which isn't valid JSON at all is logged and skipped rather than
// crashing/killing the listener -- a public, uncontrolled peer sending one
// bad frame must not take down the connection.
func TestJetstream_MalformedJSONDoesNotKillListener(t *testing.T) {
	validMsg := jetstreamCommitJSON(t, "did:plc:user", 1000, "create", "app.atchess.move", "rkey1", "cidY", map[string]interface{}{"ok": true})
	messages := [][]byte{
		[]byte(`{"did": this is not valid json`),
		validMsg,
	}

	server := newJetstreamMockServer(t, messages)
	defer server.Close()

	var events []Event
	var mu sync.Mutex
	handler := func(event Event) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	}

	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler, WithURL(jetstreamDialURL(server)), WithLogger(logger))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer client.Stop()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected the malformed frame to be skipped and the valid one after it still processed, got %d events", len(events))
	}
	if !client.IsConnected() {
		t.Errorf("expected the client to remain connected after a malformed JSON frame")
	}
}

// TestJetstream_CursorAdvancesAndResumesOnReconnect verifies that
// LastSequence tracks the latest time_us seen, and that a reconnect dials
// with that same value as the "cursor" query param -- i.e. Jetstream
// resumption actually works, using its own (microsecond) cursor semantics
// rather than a subscribeRepos sequence number.
func TestJetstream_CursorAdvancesAndResumesOnReconnect(t *testing.T) {
	const timeUS = int64(1725911162329308) // a realistic time_us value

	var mu sync.Mutex
	var queries []string
	var connCount int
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connCount++
		n := connCount
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if n == 1 {
			msg := jetstreamCommitJSON(t, "did:plc:cursoruser", timeUS, "create", "app.atchess.move", "rkey1", "cidX", map[string]interface{}{"x": 1})
			_ = conn.WriteMessage(websocket.TextMessage, msg)
			return // close immediately, forcing a reconnect
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	handler := func(event Event) error { return nil }
	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler,
		WithURL(jetstreamDialURL(server)),
		WithLogger(logger),
		WithInitialReconnectDelay(50*time.Millisecond))

	if client.Transport() != TransportJetstream {
		t.Fatalf("expected auto-detected TransportJetstream for a /subscribe URL, got %v", client.Transport())
	}

	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer client.Stop()

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
	gotQueries := append([]string(nil), queries...)
	mu.Unlock()

	if len(gotQueries) < 2 {
		t.Fatalf("expected at least 2 connections (a reconnect), saw %d", len(gotQueries))
	}

	if seq := client.LastSequence(); seq != timeUS {
		t.Errorf("expected LastSequence to be %d (the time_us of the only message seen), got %d", timeUS, seq)
	}

	secondQuery, err := url.ParseQuery(gotQueries[1])
	if err != nil {
		t.Fatalf("failed to parse second connection's query %q: %v", gotQueries[1], err)
	}
	if got := secondQuery.Get("cursor"); got != fmt.Sprintf("%d", timeUS) {
		t.Errorf("expected the reconnect to resume with cursor=%d, got cursor=%q (full query: %q)", timeUS, got, gotQueries[1])
	}
}

// TestJetstream_WantedCollectionsInDialedURL verifies that connect() adds
// one wantedCollections query param per WantedCollections entry to the
// dialed URL -- this is what makes server-side filtering (the entire point
// of the Jetstream transport, per atchess-1c9.49) actually happen.
func TestJetstream_WantedCollectionsInDialedURL(t *testing.T) {
	var mu sync.Mutex
	var capturedQuery url.Values
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedQuery = r.URL.Query()
		mu.Unlock()

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

	handler := func(event Event) error { return nil }
	client := NewClient(handler, WithURL(jetstreamDialURL(server)))
	if err := client.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer client.Stop()

	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	q := capturedQuery
	mu.Unlock()

	if q == nil {
		t.Fatalf("server never received a connection")
	}

	got := q["wantedCollections"]
	if len(got) != len(expectedWantedCollections) {
		t.Fatalf("expected %d wantedCollections query params, got %d: %v", len(expectedWantedCollections), len(got), got)
	}
	for _, want := range expectedWantedCollections {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected wantedCollections to include %q, got %v", want, got)
		}
	}
}

// TestDetectTransport_MixedURLList_DetectedIndependently verifies the
// design decision for "mixed transports across a comma-separated URL
// list" (atchess-1c9.49 step 4): since each firehose.Client independently
// infers its own transport from its own URL (no shared/global transport
// state), a config listing both a subscribeRepos host and a Jetstream
// instance just works -- each one gets the right transport.
func TestDetectTransport_MixedURLList_DetectedIndependently(t *testing.T) {
	urls := config.SplitFirehoseURLs("wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos,wss://jetstream1.us-east.bsky.network/subscribe")
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(urls), urls)
	}
	if got := DetectTransport(urls[0]); got != TransportSubscribeRepos {
		t.Errorf("DetectTransport(%q) = %v, want TransportSubscribeRepos", urls[0], got)
	}
	if got := DetectTransport(urls[1]); got != TransportJetstream {
		t.Errorf("DetectTransport(%q) = %v, want TransportJetstream", urls[1], got)
	}
}

// TestDetectTransport_ExplicitOverrideWinsOverGuess verifies WithTransport
// overrides the URL-based guess, which is how
// internal/config.FirehoseConfig.Transport forces a transport regardless
// of URL shape.
func TestDetectTransport_ExplicitOverrideWinsOverGuess(t *testing.T) {
	handler := func(event Event) error { return nil }
	client := NewClient(handler,
		WithURL("wss://jetstream1.us-east.bsky.network/subscribe"),
		WithTransport(TransportSubscribeRepos))
	if client.Transport() != TransportSubscribeRepos {
		t.Errorf("expected WithTransport to override the URL-based guess, got %v", client.Transport())
	}
}

func TestParseTransportOverride(t *testing.T) {
	cases := []struct {
		in     string
		want   Transport
		wantOK bool
	}{
		{"jetstream", TransportJetstream, true},
		{"Jetstream", TransportJetstream, true},
		{" jetstream ", TransportJetstream, true},
		{"subscribeRepos", TransportSubscribeRepos, true},
		{"cbor", TransportSubscribeRepos, true},
		{"", "", false},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseTransportOverride(tc.in)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("ParseTransportOverride(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
