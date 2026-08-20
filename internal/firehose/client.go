package firehose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipld/go-car"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/rs/zerolog"
)

const (
	// Default firehose endpoint
	DefaultFirehoseURL = "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos"

	// Reconnection parameters
	initialReconnectDelay  = 1 * time.Second
	maxReconnectDelay      = 5 * time.Minute
	reconnectBackoffFactor = 2

	// WebSocket parameters
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
)

// Transport identifies which wire protocol a firehose.Client speaks, and
// therefore -- critically -- what its cursor value MEANS. This distinction
// exists because atchess-1c9.49 added a second transport alongside the
// original CBOR one, and the two transports' cursors are not
// interchangeable:
//
//   - TransportSubscribeRepos speaks the original AT Protocol
//     com.atproto.sync.subscribeRepos CBOR frames directly. Its cursor is a
//     small, host-local, monotonically increasing SEQUENCE NUMBER assigned
//     by that specific PDS/relay's own commit log.
//   - TransportJetstream speaks a public Jetstream instance's JSON
//     `/subscribe` endpoint (https://github.com/bluesky-social/jetstream),
//     which filters server-side by collection NSID (wantedCollections) so
//     only app.atchess.* events cross the wire at all -- the whole point of
//     this transport existing, since decoding and discarding the entire
//     Bluesky network firehose in CBOR is untenable on a small single-vCPU
//     deployment. Its cursor is unix MICROSECONDS (time_us), a wall-clock
//     derived value with a completely different scale and meaning.
//
// Replaying a subscribeRepos sequence number as a Jetstream time_us cursor
// (or vice versa) would silently request something nonsensical -- "the
// beginning of time" or "a point far in the future" -- rather than what the
// caller intended. See CursorStore, which tags every persisted cursor with
// the Transport it was recorded under and refuses to hand it back to a
// connection of a different transport.
type Transport string

const (
	// TransportSubscribeRepos is this client's original, default
	// transport: real AT Protocol com.atproto.sync.subscribeRepos CBOR
	// frames (processCBORMessage). Unchanged by atchess-1c9.49.
	TransportSubscribeRepos Transport = "subscribeRepos"
	// TransportJetstream is the JSON transport added by atchess-1c9.49 for
	// connecting to a public Jetstream instance as a client (NOT
	// self-hosting one -- see that bead's notes on why the two are not the
	// same feasibility question).
	TransportJetstream Transport = "jetstream"
)

// DetectTransport infers which wire protocol rawURL speaks from its path:
// a path ending in "/subscribe" (Jetstream's endpoint shape, e.g.
// "wss://jetstream1.us-east.bsky.network/subscribe") is treated as
// TransportJetstream; everything else -- notably a
// com.atproto.sync.subscribeRepos XRPC path, this client's historical
// default -- is treated as TransportSubscribeRepos. An unparseable URL
// also defaults to TransportSubscribeRepos, preserving this client's
// original behavior for every existing configuration. Callers that need to
// force a specific transport regardless of URL shape should use
// WithTransport instead (see also internal/config.FirehoseConfig.Transport
// for the deployment-level override).
func DetectTransport(rawURL string) Transport {
	u, err := url.Parse(rawURL)
	if err != nil {
		return TransportSubscribeRepos
	}
	p := strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(p, "/subscribe") {
		return TransportJetstream
	}
	return TransportSubscribeRepos
}

// ParseTransportOverride maps a FirehoseConfig.Transport config/env string
// to a Transport. Recognized values (case-insensitive): "subscribeRepos"
// and "cbor" both map to TransportSubscribeRepos; "jetstream" maps to
// TransportJetstream. Any other non-empty value is unrecognized (ok ==
// false); callers should log that clearly and fall back to
// DetectTransport rather than silently guessing.
func ParseTransportOverride(value string) (t Transport, ok bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "subscriberepos", "cbor":
		return TransportSubscribeRepos, true
	case "jetstream":
		return TransportJetstream, true
	default:
		return "", false
	}
}

// WantedCollections is the closed list of app.atchess.* NSIDs this
// deployment actually writes (see internal/atproto/client.go -- every
// "collection": "app.atchess.*" literal there), passed as Jetstream's
// wantedCollections query parameter (one per NSID) so filtering happens
// server-side and only chess events cross the wire at all. This is the
// Jetstream-transport equivalent of isChessRecord's "app.atchess." prefix
// check: subscribeRepos has no server-side filter, so that prefix check is
// sufficient there, but Jetstream's wantedCollections requires each NSID
// enumerated explicitly. Exported and centralized here (one list, not
// hand-typed at each call site) specifically so that adding a new
// app.atchess.* lexicon and forgetting to add it here is a single missed
// edit rather than a silent gap repeated at every Jetstream call site.
//
// Deliberately does NOT include app.atchess.challengeAcceptance or
// app.atchess.gameIndex: neither is currently written anywhere in
// internal/atproto/client.go (an "accept" is instead expressed via the
// app.atchess.game record's optional "challenge" strongRef field; gameIndex
// is an unused lexicon), so subscribing to them would only ever receive
// commits this deployment can never actually produce.
var WantedCollections = []string{
	"app.atchess.challenge",
	"app.atchess.challengeResponse",
	"app.atchess.drawOffer",
	"app.atchess.drawResponse",
	"app.atchess.game",
	"app.atchess.move",
	"app.atchess.resignation",
	"app.atchess.timeViolation",
}

// EventType represents the type of chess event
type EventType string

const (
	EventTypeMove                EventType = "move"
	EventTypeDrawOffer           EventType = "drawOffer"
	EventTypeResignation         EventType = "resignation"
	EventTypeGame                EventType = "game"
	EventTypeChallenge           EventType = "challenge"
	EventTypeChallengeAcceptance EventType = "challengeAcceptance"
	// EventTypeChallengeResponse corresponds to app.atchess.challengeResponse
	// (atchess-1c9.11): a decline, written into the RESPONDING player's own
	// repo. Not currently consumed by EventProcessor (only the challenger's
	// own instance would care, and it is out of this bead's scope), but
	// classified here so isChessRecord/getEventType don't misroute it into
	// the generic EventTypeChallenge bucket.
	EventTypeChallengeResponse EventType = "challengeResponse"
)

// Event represents a chess-related event from the firehose
type Event struct {
	Type      EventType
	Repo      string // DID of the repository
	Path      string // Record path
	CID       string // Content ID
	Timestamp time.Time
	Record    interface{} // Decoded record data
}

// EventHandler is called for each chess-related event
type EventHandler func(event Event) error

// Client connects to the AT Protocol firehose and filters chess events
type Client struct {
	url            string
	conn           *websocket.Conn
	handler        EventHandler
	logger         zerolog.Logger
	ctx            context.Context
	cancel         context.CancelFunc
	reconnectDelay time.Duration
	mu             sync.RWMutex
	wg             sync.WaitGroup
	connected      bool
	// lastSequence is the last processed firehose sequence number, used to
	// resume a dropped connection exactly where it left off (see connect).
	// -1 means "no cursor established yet" (a brand new client that has not
	// been given an explicit starting cursor via WithCursor and has not yet
	// processed any message) -- connect omits the cursor query param
	// entirely in that case, which every AT Protocol PDS interprets as
	// "start at the live tip". 0 is a legitimate, DISTINCT value from -1:
	// it explicitly requests replay from the very beginning of the PDS's
	// commit log (see WithCursor), which is how atchess-1c9.11's
	// backfill-on-login is implemented -- a full historical resubscribe,
	// not merely resuming live tail.
	lastSequence int64
	// transport is which wire protocol this client speaks -- and therefore
	// what lastSequence MEANS (a subscribeRepos sequence number or a
	// Jetstream time_us microsecond cursor; see the Transport type's doc
	// comment). Set once, either explicitly via WithTransport or inferred
	// from url via DetectTransport, before Start is ever called; never
	// mutated afterward, so it is safe to read without c.mu.
	transport Transport
	// transportExplicit records whether WithTransport was passed, so
	// NewClient knows whether to run DetectTransport(url) as a fallback
	// (only when the caller did not explicitly choose a transport).
	transportExplicit bool
	// connCancel cancels connCtx, the context scoped to the lifetime of the
	// current connection. It is canceled (and cleared) whenever the
	// connection is replaced or torn down, so that goroutines bound to the
	// old connection (e.g. pingLoop) know to exit rather than pick up
	// whatever connection happens to be current later.
	connCancel context.CancelFunc

	// For testing
	dialer        *websocket.Dialer
	mockWebSocket bool
}

// Option configures the client
type Option func(*Client)

// WithURL sets a custom firehose URL
func WithURL(url string) Option {
	return func(c *Client) {
		c.url = url
	}
}

// WithLogger sets a custom logger
func WithLogger(logger zerolog.Logger) Option {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithMockWebSocket enables mock mode for testing
func WithMockWebSocket(dialer *websocket.Dialer) Option {
	return func(c *Client) {
		c.mockWebSocket = true
		c.dialer = dialer
	}
}

// WithInitialReconnectDelay sets the initial reconnect delay
func WithInitialReconnectDelay(delay time.Duration) Option {
	return func(c *Client) {
		c.reconnectDelay = delay
	}
}

// WithTransport forces this client to speak t regardless of what its URL
// looks like, overriding the automatic per-URL guess (DetectTransport).
// Without this option, NewClient infers the transport from whatever URL is
// in effect once every Option has run (so it also respects a WithURL
// passed alongside it). See internal/config.FirehoseConfig.Transport for
// the deployment-level config/env knob that maps onto this.
func WithTransport(t Transport) Option {
	return func(c *Client) {
		c.transport = t
		c.transportExplicit = true
	}
}

// WithCursor requests that the FIRST connection this client makes start
// replaying from cursor (inclusive of whatever the PDS has from that point
// forward) rather than defaulting to the live tip. Passing 0 requests a full
// historical replay of the watched PDS's commit log -- this is how
// atchess-1c9.11's backfill-on-login is implemented: cmd/protocol/main.go
// gives every firehose.Client it starts WithCursor(0), so a challenge issued
// while this process was not running is still discovered once it starts and
// resubscribes from the beginning, rather than only ever seeing challenges
// created after that moment. This only affects the FIRST connection
// attempt; every reconnect after that already resumes from LastSequence
// (whatever was last actually processed), which is always >= cursor once
// the first message has been processed.
func WithCursor(cursor int64) Option {
	return func(c *Client) {
		c.lastSequence = cursor
	}
}

// NewClient creates a new firehose client
func NewClient(handler EventHandler, opts ...Option) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		url:            DefaultFirehoseURL,
		handler:        handler,
		logger:         zerolog.Nop(),
		ctx:            ctx,
		cancel:         cancel,
		reconnectDelay: initialReconnectDelay,
		dialer:         websocket.DefaultDialer,
		lastSequence:   -1, // no cursor established yet; see the field's doc comment
	}

	for _, opt := range opts {
		opt(client)
	}

	// Transport selection happens after every Option has run, not before,
	// so that DetectTransport sees whatever URL is actually in effect
	// (e.g. WithURL) rather than always the default. WithTransport, if
	// passed, always wins over the guess.
	if !client.transportExplicit {
		client.transport = DetectTransport(client.url)
	}

	return client
}

// Transport returns which wire protocol this client speaks. Fixed at
// construction (see NewClient); safe for concurrent use without locking
// since it is never mutated afterward.
func (c *Client) Transport() Transport {
	return c.transport
}

// Start begins listening to the firehose
func (c *Client) Start() error {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run()
	}()
	return nil
}

// Stop gracefully shuts down the client. It blocks until all background
// goroutines (the connection loop and ping loop) have fully exited, so
// callers can safely release resources (e.g. loggers) immediately after
// Stop returns.
func (c *Client) Stop() error {
	c.cancel()

	c.mu.Lock()
	var err error
	if c.conn != nil {
		err = c.conn.Close()
		c.conn = nil
		c.connected = false
	}
	if c.connCancel != nil {
		c.connCancel()
		c.connCancel = nil
	}
	c.mu.Unlock()

	c.wg.Wait()

	return err
}

// IsConnected returns whether the client is connected
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// LastSequence returns the last processed firehose sequence number, or -1 if
// no cursor has been established yet (a brand new client with no WithCursor
// override that has not processed a message). Safe for concurrent use.
func (c *Client) LastSequence() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSequence
}

// setLastSequence updates the last processed firehose sequence number.
// Safe for concurrent use.
func (c *Client) setLastSequence(seq int64) {
	c.mu.Lock()
	c.lastSequence = seq
	c.mu.Unlock()
}

// getConn returns the current websocket connection, if any. Safe for
// concurrent use. The result may be nil (e.g. briefly while reconnecting
// or after Stop); callers must check.
func (c *Client) getConn() *websocket.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}

func (c *Client) run() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			connCtx, err := c.connect()
			if err != nil {
				c.logger.Error().Err(err).Msg("Failed to connect to firehose")
				c.handleReconnect()
				continue
			}

			if err := c.listen(connCtx); err != nil {
				c.logger.Error().Err(err).Msg("Error listening to firehose")
				c.handleReconnect()
				continue
			}
		}
	}
}

// connect dials the firehose and returns a context scoped to the lifetime
// of the resulting connection. That context is canceled by handleReconnect
// or Stop when the connection is torn down, allowing connection-scoped
// goroutines (pingLoop) to exit promptly instead of outliving it.
func (c *Client) connect() (context.Context, error) {
	c.logger.Info().Str("url", c.url).Str("transport", string(c.transport)).Msg("Connecting to firehose")

	dialURL := c.buildDialURL()

	// Set up headers
	headers := http.Header{}
	headers.Set("User-Agent", "ATChess/1.0")

	// Connect with timeout
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()

	conn, _, err := c.dialer.DialContext(ctx, dialURL, headers)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	connCtx, connCancel := context.WithCancel(c.ctx)

	c.mu.Lock()
	c.conn = conn
	c.connCancel = connCancel
	c.connected = true
	c.reconnectDelay = initialReconnectDelay
	c.mu.Unlock()

	c.logger.Info().Msg("Connected to firehose")

	// Set up ping/pong handlers
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongTimeout))
		return nil
	})

	return connCtx, nil
}

// buildDialURL constructs the URL passed to the websocket dialer.
//
// For TransportSubscribeRepos this is byte-for-byte the client's original
// (pre-atchess-1c9.49) behavior: c.url with "?cursor=<lastSeq>" appended
// when a cursor is established (-1 means "none yet" -- see lastSequence's
// field doc comment; 0 is a legitimate, distinct value meaning "replay
// from the very beginning", used for atchess-1c9.11's backfill-on-login
// via WithCursor(0)). Deliberately unchanged so existing CBOR deployments
// see no behavior difference from this bead.
//
// For TransportJetstream, the cursor (Jetstream's cursor query param is
// also literally named "cursor", but its value is unix MICROSECONDS --
// time_us -- not a subscribeRepos sequence number; see the Transport type)
// is added the same way, and one wantedCollections query param per
// WantedCollections entry is added so filtering happens server-side and
// only chess events cross the wire at all -- the entire point of this
// transport (atchess-1c9.49).
func (c *Client) buildDialURL() string {
	lastSeq := c.LastSequence()

	if c.transport != TransportJetstream {
		if lastSeq >= 0 {
			return fmt.Sprintf("%s?cursor=%d", c.url, lastSeq)
		}
		return c.url
	}

	u, err := url.Parse(c.url)
	if err != nil {
		// Should not happen for a valid configured URL; fall back to the
		// unmodified URL rather than crashing the connection attempt. This
		// does mean wantedCollections would be missing (i.e. the full,
		// unfiltered Jetstream stream) -- logged loudly so it's not a
		// silent degradation.
		c.logger.Error().Err(err).Str("url", c.url).Msg("failed to parse Jetstream URL to add cursor/wantedCollections; dialing it unmodified (this means NO server-side collection filtering will happen)")
		return c.url
	}

	q := u.Query()
	if lastSeq >= 0 {
		q.Set("cursor", strconv.FormatInt(lastSeq, 10))
	}
	for _, coll := range WantedCollections {
		q.Add("wantedCollections", coll)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// listen reads messages from the current connection until it errors, is
// closed, or the client's context is canceled. connCtx is the
// connection-scoped context returned by connect for this specific
// connection; it is passed to pingLoop so the ping loop is tied to this
// connection's lifetime rather than whatever c.conn happens to be later.
func (c *Client) listen(connCtx context.Context) error {
	// Start ping routine, scoped to this connection.
	conn := c.getConn()
	c.wg.Add(1)
	go c.pingLoop(connCtx, conn)

	for {
		select {
		case <-c.ctx.Done():
			return nil
		default:
			conn := c.getConn()
			if conn == nil {
				return fmt.Errorf("connection is nil")
			}
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					return fmt.Errorf("websocket read error: %w", err)
				}
				return err
			}

			if c.transport == TransportJetstream {
				// Jetstream sends JSON, which the gorilla/websocket
				// library (and most peers) frame as TextMessage, but
				// accept BinaryMessage too rather than assume a specific
				// peer's framing choice -- either way it's still valid
				// UTF-8 JSON to decode.
				if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
					continue
				}
			} else if messageType != websocket.BinaryMessage {
				continue
			}

			if err := c.processMessage(data); err != nil {
				var errFrame *firehoseErrorFrame
				if errors.As(err, &errFrame) {
					// A firehose error frame (e.g. FutureCursor) means the
					// host has rejected this connection/cursor outright.
					// handleErrorFrame already logged and reacted (e.g.
					// resetting the cursor); returning here propagates up
					// to run(), which reconnects immediately rather than
					// waiting on the underlying socket to also close,
					// which the protocol does not guarantee happens
					// synchronously with the error frame.
					return err
				}
				c.logger.Error().Err(err).Msg("Error processing message")
				// Continue processing other messages
			}
		}
	}
}

func (c *Client) processMessage(data []byte) error {
	if c.transport == TransportJetstream {
		return c.processJetstreamMessage(data)
	}

	// The AT Protocol firehose uses a specific message format
	// We handle both test format and real AT Protocol CBOR format

	// First try to parse as our test format (with 4-byte header length prefix)
	if len(data) >= 4 {
		headerLen := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
		if len(data) >= 4+headerLen && headerLen > 0 && headerLen < len(data) {
			// Looks like our test format
			return c.processTestMessage(data)
		}
	}

	// Parse as real AT Protocol CBOR format
	// Each message contains two concatenated CBOR items: header + body
	return c.processCBORMessage(data)
}

// jetstreamEvent mirrors the JSON shape a Jetstream instance's /subscribe
// endpoint sends for every event on the stream:
//
//	{"did":"did:plc:...","time_us":1725911162329308,"kind":"commit","commit":{...}}
//
// kind is "commit", "identity", or "account". Only "commit" events carry a
// non-nil Commit and correspond to an actual repo write; "identity" and
// "account" events carry no Commit at all and are ignored by
// processJetstreamMessage (they don't represent a chess record).
type jetstreamEvent struct {
	DID    string           `json:"did"`
	TimeUS int64            `json:"time_us"`
	Kind   string           `json:"kind"`
	Commit *jetstreamCommit `json:"commit"`
}

// jetstreamCommit mirrors Jetstream's "commit" object: operation is
// "create", "update", or "delete". Record is the raw record JSON (absent
// for "delete") -- left as json.RawMessage and decoded lazily only for
// chess-collection commits, mirroring processCBORMessage's CAR-block
// extraction happening only for chess ops.
type jetstreamCommit struct {
	Rev        string          `json:"rev"`
	Operation  string          `json:"operation"`
	Collection string          `json:"collection"`
	RKey       string          `json:"rkey"`
	Record     json.RawMessage `json:"record"`
	CID        string          `json:"cid"`
}

// processJetstreamMessage decodes one Jetstream JSON message and, for
// commit events whose collection is one of WantedCollections'
// app.atchess.* NSIDs, builds the same Event type processCBORMessage does
// so downstream EventHandler/ChessEventProcessor code is transport-
// agnostic.
//
// Malformed JSON is logged at Debug and swallowed (returns nil), mirroring
// processCBORMessage's handling of malformed CBOR: a single bad frame from
// a public, uncontrolled peer must never kill the listener. Non-"commit"
// kinds (identity/account) and an unrecognized/absent collection are
// silently ignored -- not errors, just "nothing to do here" -- for the
// same reason: wantedCollections already does most of the filtering
// server-side, but a defense-in-depth client-side check (isChessRecord)
// costs nothing and guards against, e.g., a misconfigured/omitted
// wantedCollections param handing back the whole firehose.
func (c *Client) processJetstreamMessage(data []byte) error {
	var evt jetstreamEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		c.logger.Debug().Err(err).Int("len", len(data)).Msg("Failed to decode Jetstream JSON message")
		return nil
	}

	// Jetstream cursors are unix MICROSECONDS (time_us), not subscribeRepos
	// sequence numbers -- see the Transport type's doc comment and
	// CursorStore's transport tagging. Advance on every event actually
	// seen on the stream (not just "commit" ones): identity/account events
	// carry a time_us too, and resuming from the latest one seen is still
	// correct -- Jetstream's cursor semantics are "resume the stream from
	// this point in time", not "resume from the last commit specifically".
	if evt.TimeUS > 0 {
		c.setLastSequence(evt.TimeUS)
	}

	if evt.Kind != "commit" || evt.Commit == nil {
		return nil
	}

	// Same path shape processCBORMessage/getEventType/isChessRecord
	// already expect: "<collection>/<rkey>", e.g.
	// "app.atchess.challenge/3l...". An empty/unrecognized collection (or
	// rkey) falls through isChessRecord's prefix check harmlessly rather
	// than crashing.
	path := evt.Commit.Collection + "/" + evt.Commit.RKey
	if !isChessRecord(path) {
		return nil
	}

	var record interface{}
	if len(evt.Commit.Record) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(evt.Commit.Record, &m); err == nil {
			record = m
		} else {
			c.logger.Debug().Err(err).Str("path", path).Msg("Failed to decode Jetstream commit record JSON")
		}
	}
	if record == nil {
		// Matches processCBORMessage's fallback for a missing/unextractable
		// record (e.g. a "delete" operation carries no record at all).
		record = map[string]interface{}{}
	}

	ts := time.Now()
	if evt.TimeUS > 0 {
		ts = time.UnixMicro(evt.TimeUS)
	}

	event := Event{
		Type:      getEventType(path),
		Repo:      evt.DID,
		Path:      path,
		CID:       evt.Commit.CID,
		Timestamp: ts,
		Record:    record,
	}

	if err := c.handler(event); err != nil {
		c.logger.Error().Err(err).Msg("Event handler error")
	}

	return nil
}

func (c *Client) processCBORMessage(data []byte) error {
	// AT Protocol firehose messages contain two concatenated CBOR items.
	// dagcbor.Decode rejects trailing data, so we split them first.
	boundary, err := scanCBORItem(data, 0)
	if err != nil || boundary >= len(data) {
		c.logger.Debug().Int("len", len(data)).Msg("Failed to find CBOR boundary")
		return nil
	}

	// Decode CBOR header: {op: int, t: string}
	headerBuilder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(headerBuilder, bytes.NewReader(data[:boundary])); err != nil {
		c.logger.Debug().Err(err).Int("len", len(data)).Msg("Failed to decode CBOR header")
		return nil // Skip malformed messages
	}
	headerNode := headerBuilder.Build()

	// Extract op field
	opNode, err := headerNode.LookupByString("op")
	if err != nil {
		return nil
	}
	op, err := opNode.AsInt()
	if err != nil {
		return nil
	}

	// op == -1 is an AT Protocol firehose "error frame" (no "t" field at
	// all -- {op: -1, ...} followed by a body {error, message}), sent when
	// the host rejects this connection outright, notably FutureCursor: our
	// requested cursor is beyond what the host has ever produced (e.g. a
	// stale/corrupt persisted cursor, or a host whose sequence counter was
	// reset). Handled explicitly (atchess-1c9.46) rather than silently
	// falling through to the "unknown message" case below and looping
	// forever retrying the same rejected cursor.
	if op == -1 {
		return c.handleErrorFrame(data[boundary:])
	}

	// Extract t field
	tNode, err := headerNode.LookupByString("t")
	if err != nil {
		return nil
	}
	msgType, err := tNode.AsString()
	if err != nil {
		return nil
	}

	// #info frames (op: 1, t: "#info") are advisory, not fatal -- notably
	// OutdatedCursor: our requested cursor is older than the host's
	// retention window, so the host is about to start streaming from the
	// earliest point it still has rather than from our exact cursor
	// (atchess-1c9.46's "a cursor the host rejects as too old" case). The
	// connection stays open; logged loudly because it means some events
	// between our old cursor and the host's retention floor were
	// unrecoverably missed by this subscription (the login-time repo-read
	// backfill, internal/backfill, is the intended mitigation for exactly
	// this gap).
	if op == 1 && msgType == "#info" {
		c.handleInfoFrame(data[boundary:])
		return nil
	}

	// We only care about commit events
	if op != 1 || msgType != "#commit" {
		return nil
	}

	// Decode CBOR body
	bodyBuilder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(bodyBuilder, bytes.NewReader(data[boundary:])); err != nil {
		c.logger.Debug().Err(err).Msg("Failed to decode CBOR body")
		return nil
	}
	bodyNode := bodyBuilder.Build()

	// Extract sequence number
	if seqNode, err := bodyNode.LookupByString("seq"); err == nil {
		if seq, err := seqNode.AsInt(); err == nil && seq > 0 {
			c.setLastSequence(seq)
		}
	}

	// Extract repo (DID)
	repoNode, err := bodyNode.LookupByString("repo")
	if err != nil {
		return nil
	}
	repo, err := repoNode.AsString()
	if err != nil {
		return nil
	}

	// Extract ops array
	opsNode, err := bodyNode.LookupByString("ops")
	if err != nil {
		return nil
	}

	// Check if any ops are chess-related before extracting blocks
	hasChessOps := false
	iter := opsNode.ListIterator()
	for !iter.Done() {
		_, opEntry, err := iter.Next()
		if err != nil {
			break
		}
		pathNode, err := opEntry.LookupByString("path")
		if err != nil {
			continue
		}
		path, err := pathNode.AsString()
		if err != nil {
			continue
		}
		if isChessRecord(path) {
			hasChessOps = true
			break
		}
	}

	if !hasChessOps {
		return nil
	}

	// Extract blocks (CAR data) for record extraction
	var blocksData []byte
	if blocksNode, err := bodyNode.LookupByString("blocks"); err == nil {
		blocksData, _ = blocksNode.AsBytes()
	}

	// Process each chess-related operation
	iter = opsNode.ListIterator()
	for !iter.Done() {
		_, opEntry, err := iter.Next()
		if err != nil {
			break
		}

		pathNode, err := opEntry.LookupByString("path")
		if err != nil {
			continue
		}
		path, err := pathNode.AsString()
		if err != nil {
			continue
		}

		if !isChessRecord(path) {
			continue
		}

		// Extract CID string from the op
		var cidStr string
		if cidNode, err := opEntry.LookupByString("cid"); err == nil {
			// CID may be encoded as a CBOR link (tag 42) or a string
			if link, err := cidNode.AsLink(); err == nil {
				cidStr = link.String()
			} else if s, err := cidNode.AsString(); err == nil {
				cidStr = s
			}
		}

		// Try to extract the record from CAR blocks
		var record interface{}
		if len(blocksData) > 0 && cidStr != "" {
			record, _ = c.extractRecord(blocksData, cidStr)
		}
		if record == nil {
			record = map[string]interface{}{}
		}

		event := Event{
			Type:      getEventType(path),
			Repo:      repo,
			Path:      path,
			CID:       cidStr,
			Timestamp: time.Now(),
			Record:    record,
		}

		if err := c.handler(event); err != nil {
			c.logger.Error().Err(err).Msg("Event handler error")
		}
	}

	return nil
}

// firehoseErrorFrame represents an AT Protocol firehose error frame
// (header {op: -1}, body {error, message}) -- see processCBORMessage's
// op == -1 branch. Returned (not merely logged) from processMessage so
// listen() can react explicitly -- notably by reconnecting immediately --
// rather than depending on the host also closing the underlying TCP/WS
// connection, which the spec does not guarantee happens synchronously with
// the error frame.
type firehoseErrorFrame struct {
	Code    string
	Message string
}

func (e *firehoseErrorFrame) Error() string {
	return fmt.Sprintf("firehose error frame: %s: %s", e.Code, e.Message)
}

// handleErrorFrame decodes an error frame body ({error: string, message:
// string}, per com.atproto.sync.subscribeRepos) and reacts to known error
// codes. FutureCursor -- the host rejected our requested cursor as beyond
// anything it has ever produced, most plausibly a stale/corrupt persisted
// cursor (see internal/firehose.CursorStore) or a host whose sequence
// counter was reset -- clears this client's in-memory cursor back to "none
// established" (-1) so the next reconnect (triggered by returning a
// non-nil error here) requests the live tip instead of retrying the same
// rejected cursor forever. cmd/protocol/main.go's periodic cursor
// persistence then propagates that reset to disk too (CursorStore.Store
// treats a negative sequence as "clear"), so a subsequent process restart
// does not immediately hit the same FutureCursor rejection again.
func (c *Client) handleErrorFrame(body []byte) error {
	bodyBuilder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(bodyBuilder, bytes.NewReader(body)); err != nil {
		c.logger.Error().Err(err).Msg("Received a firehose error frame but failed to decode its body")
		return &firehoseErrorFrame{Code: "unknown", Message: "(undecodable error frame body)"}
	}
	bodyNode := bodyBuilder.Build()

	code := "unknown"
	if n, err := bodyNode.LookupByString("error"); err == nil {
		if s, err := n.AsString(); err == nil {
			code = s
		}
	}
	message := ""
	if n, err := bodyNode.LookupByString("message"); err == nil {
		if s, err := n.AsString(); err == nil {
			message = s
		}
	}

	c.logger.Error().Str("code", code).Str("message", message).Str("url", c.url).Msg("Firehose host sent an error frame")

	if code == "FutureCursor" {
		c.logger.Warn().Str("url", c.url).Msg("Cursor rejected as FutureCursor; resetting to live tip (no cursor) for the next reconnect instead of retrying the same cursor")
		c.setLastSequence(-1)
	}

	return &firehoseErrorFrame{Code: code, Message: message}
}

// handleInfoFrame decodes an #info frame body ({name: string, message:
// string}, per com.atproto.sync.subscribeRepos) and logs it. Unlike error
// frames, #info frames are advisory: the connection stays open and this
// method never returns an error. OutdatedCursor specifically is logged at
// Warn (not Info) because it means this subscription has a gap: the host
// is skipping forward to the earliest point it still retains, past our
// requested cursor.
func (c *Client) handleInfoFrame(body []byte) {
	bodyBuilder := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(bodyBuilder, bytes.NewReader(body)); err != nil {
		c.logger.Debug().Err(err).Msg("Received a firehose #info frame but failed to decode its body")
		return
	}
	bodyNode := bodyBuilder.Build()

	name := "unknown"
	if n, err := bodyNode.LookupByString("name"); err == nil {
		if s, err := n.AsString(); err == nil {
			name = s
		}
	}
	message := ""
	if n, err := bodyNode.LookupByString("message"); err == nil {
		if s, err := n.AsString(); err == nil {
			message = s
		}
	}

	logEvt := c.logger.Info()
	if name == "OutdatedCursor" {
		logEvt = c.logger.Warn()
	}
	logEvt.Str("name", name).Str("message", message).Str("url", c.url).Msg("Firehose host sent an #info frame")
}

func (c *Client) processTestMessage(data []byte) error {
	// Parse test message format
	headerLen := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) < 4+headerLen {
		return fmt.Errorf("invalid header length")
	}

	headerData := data[4 : 4+headerLen]

	var message struct {
		Op   int    `json:"op"`
		T    string `json:"t"`
		Seq  int64  `json:"seq"`
		Repo string `json:"repo"`
		Rev  string `json:"rev"`
		Ops  []struct {
			Action string `json:"action"`
			Path   string `json:"path"`
			CID    string `json:"cid"`
		} `json:"ops"`
	}

	if err := json.Unmarshal(headerData, &message); err != nil {
		return fmt.Errorf("failed to parse header: %w", err)
	}

	// Update sequence for resumption
	if message.Seq > 0 {
		c.setLastSequence(message.Seq)
	}

	// We're only interested in commit events
	if message.Op != 1 || message.T != "#commit" {
		return nil
	}

	// Check if any operations are chess-related
	for _, op := range message.Ops {
		if !isChessRecord(op.Path) {
			continue
		}

		// For test messages, we don't have real CAR data
		// Just create a simple event
		event := Event{
			Type:      getEventType(op.Path),
			Repo:      message.Repo,
			Path:      op.Path,
			CID:       op.CID,
			Timestamp: time.Now(),
			Record:    map[string]interface{}{}, // Empty record for tests
		}

		if err := c.handler(event); err != nil {
			c.logger.Error().Err(err).Msg("Event handler error")
		}
	}

	return nil
}

func (c *Client) extractRecord(carData []byte, targetCID string) (interface{}, error) {
	// Create CAR reader
	reader, err := car.NewCarReader(bytes.NewReader(carData))
	if err != nil {
		return nil, fmt.Errorf("failed to create CAR reader: %w", err)
	}

	// Iterate through blocks to find our target
	for {
		block, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read block: %w", err)
		}

		// Check if this is our target block
		if block.Cid().String() == targetCID {
			// Decode CBOR data
			nb := basicnode.Prototype.Any.NewBuilder()
			err := dagcbor.Decode(nb, bytes.NewReader(block.RawData()))
			if err != nil {
				return nil, fmt.Errorf("failed to decode CBOR: %w", err)
			}
			node := nb.Build()

			// Convert to Go map
			return nodeToGo(node)
		}
	}

	return nil, fmt.Errorf("target CID not found in CAR file")
}

func nodeToGo(node ipld.Node) (interface{}, error) {
	switch node.Kind() {
	case ipld.Kind_Map:
		m := make(map[string]interface{})
		iter := node.MapIterator()
		for !iter.Done() {
			k, v, err := iter.Next()
			if err != nil {
				return nil, err
			}
			keyStr, err := k.AsString()
			if err != nil {
				return nil, err
			}
			val, err := nodeToGo(v)
			if err != nil {
				return nil, err
			}
			m[keyStr] = val
		}
		return m, nil

	case ipld.Kind_List:
		var list []interface{}
		iter := node.ListIterator()
		for !iter.Done() {
			_, v, err := iter.Next()
			if err != nil {
				return nil, err
			}
			val, err := nodeToGo(v)
			if err != nil {
				return nil, err
			}
			list = append(list, val)
		}
		return list, nil

	case ipld.Kind_String:
		return node.AsString()

	case ipld.Kind_Int:
		return node.AsInt()

	case ipld.Kind_Float:
		return node.AsFloat()

	case ipld.Kind_Bool:
		return node.AsBool()

	case ipld.Kind_Null:
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported node kind: %v", node.Kind())
	}
}

// pingLoop sends periodic pings on conn until it errors, connCtx is
// canceled (this connection was replaced or torn down by handleReconnect
// or Stop), or the client's ctx is canceled. It intentionally does not
// re-read c.conn: doing so would let a stale ping loop silently adopt a
// later connection instead of exiting, accumulating one extra goroutine
// (and duplicate pings) per reconnect.
func (c *Client) pingLoop(connCtx context.Context, conn *websocket.Conn) {
	defer c.wg.Done()

	if conn == nil {
		return
	}

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-connCtx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeTimeout)); err != nil {
				c.logger.Error().Err(err).Msg("Ping failed")
				return
			}
		}
	}
}

func (c *Client) handleReconnect() {
	c.mu.Lock()
	c.connected = false
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	if c.connCancel != nil {
		c.connCancel()
		c.connCancel = nil
	}

	// Get current delay before updating
	delay := c.reconnectDelay

	// Exponential backoff
	c.reconnectDelay = time.Duration(float64(c.reconnectDelay) * reconnectBackoffFactor)
	if c.reconnectDelay > maxReconnectDelay {
		c.reconnectDelay = maxReconnectDelay
	}
	c.mu.Unlock()

	c.logger.Info().Str("delay", delay.String()).Msg("Waiting before reconnect")

	select {
	case <-time.After(delay):
	case <-c.ctx.Done():
	}
}

// scanCBORItem scans one CBOR item starting at offset and returns the
// offset immediately after it. CBOR is self-delimiting so we can find
// item boundaries without fully decoding.
func scanCBORItem(data []byte, offset int) (int, error) {
	if offset >= len(data) {
		return 0, fmt.Errorf("unexpected end of CBOR data")
	}

	initial := data[offset]
	major := initial >> 5
	addInfo := initial & 0x1f
	offset++

	// Get the argument value
	var argVal uint64
	switch {
	case addInfo < 24:
		argVal = uint64(addInfo)
	case addInfo == 24:
		if offset >= len(data) {
			return 0, fmt.Errorf("unexpected end")
		}
		argVal = uint64(data[offset])
		offset++
	case addInfo == 25:
		if offset+2 > len(data) {
			return 0, fmt.Errorf("unexpected end")
		}
		argVal = uint64(data[offset])<<8 | uint64(data[offset+1])
		offset += 2
	case addInfo == 26:
		if offset+4 > len(data) {
			return 0, fmt.Errorf("unexpected end")
		}
		argVal = uint64(data[offset])<<24 | uint64(data[offset+1])<<16 | uint64(data[offset+2])<<8 | uint64(data[offset+3])
		offset += 4
	case addInfo == 27:
		if offset+8 > len(data) {
			return 0, fmt.Errorf("unexpected end")
		}
		argVal = uint64(data[offset])<<56 | uint64(data[offset+1])<<48 | uint64(data[offset+2])<<40 | uint64(data[offset+3])<<32 |
			uint64(data[offset+4])<<24 | uint64(data[offset+5])<<16 | uint64(data[offset+6])<<8 | uint64(data[offset+7])
		offset += 8
	case addInfo == 31:
		// Indefinite length — scan until break code (0xff)
		for {
			if offset >= len(data) {
				return 0, fmt.Errorf("unexpected end in indefinite item")
			}
			if data[offset] == 0xff {
				return offset + 1, nil
			}
			var err error
			offset, err = scanCBORItem(data, offset)
			if err != nil {
				return 0, err
			}
		}
	default:
		return 0, fmt.Errorf("unsupported additional info: %d", addInfo)
	}

	switch major {
	case 0, 1: // unsigned int, negative int
		return offset, nil
	case 2, 3: // byte string, text string
		return offset + int(argVal), nil
	case 4: // array — scan argVal child items
		for i := uint64(0); i < argVal; i++ {
			var err error
			offset, err = scanCBORItem(data, offset)
			if err != nil {
				return 0, err
			}
		}
		return offset, nil
	case 5: // map — scan argVal*2 items (key + value pairs)
		for i := uint64(0); i < argVal*2; i++ {
			var err error
			offset, err = scanCBORItem(data, offset)
			if err != nil {
				return 0, err
			}
		}
		return offset, nil
	case 6: // tag — scan the tagged item
		return scanCBORItem(data, offset)
	case 7: // float, simple, break
		return offset, nil
	default:
		return 0, fmt.Errorf("unknown CBOR major type: %d", major)
	}
}

func isChessRecord(path string) bool {
	return strings.HasPrefix(path, "app.atchess.")
}

func getEventType(path string) EventType {
	switch {
	case strings.Contains(path, "app.atchess.move"):
		return EventTypeMove
	case strings.Contains(path, "app.atchess.drawOffer"):
		return EventTypeDrawOffer
	case strings.Contains(path, "app.atchess.resignation"):
		return EventTypeResignation
	case strings.Contains(path, "app.atchess.game"):
		return EventTypeGame
	case strings.Contains(path, "app.atchess.challengeResponse"):
		return EventTypeChallengeResponse
	case strings.Contains(path, "app.atchess.challengeAcceptance"):
		return EventTypeChallengeAcceptance
	case strings.Contains(path, "app.atchess.challenge"):
		return EventTypeChallenge
	default:
		return EventTypeGame
	}
}
