package firehose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// EventType represents the type of chess event
type EventType string

const (
	EventTypeMove                  EventType = "move"
	EventTypeDrawOffer             EventType = "drawOffer"
	EventTypeResignation           EventType = "resignation"
	EventTypeGame                  EventType = "game"
	EventTypeChallenge             EventType = "challenge"
	EventTypeChallengeAcceptance   EventType = "challengeAcceptance"
	EventTypeChallengeNotification EventType = "challengeNotification"
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
	lastSequence   int64
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
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
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

// LastSequence returns the last processed firehose sequence number.
// Safe for concurrent use.
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
	c.logger.Info().Str("url", c.url).Msg("Connecting to firehose")

	// Build URL with cursor if we have a sequence
	url := c.url
	if lastSeq := c.LastSequence(); lastSeq > 0 {
		url = fmt.Sprintf("%s?cursor=%d", url, lastSeq)
	}

	// Set up headers
	headers := http.Header{}
	headers.Set("User-Agent", "ATChess/1.0")

	// Connect with timeout
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()

	conn, _, err := c.dialer.DialContext(ctx, url, headers)
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

			if messageType != websocket.BinaryMessage {
				continue
			}

			if err := c.processMessage(data); err != nil {
				c.logger.Error().Err(err).Msg("Error processing message")
				// Continue processing other messages
			}
		}
	}
}

func (c *Client) processMessage(data []byte) error {
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

	// Extract t field
	tNode, err := headerNode.LookupByString("t")
	if err != nil {
		return nil
	}
	msgType, err := tNode.AsString()
	if err != nil {
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
	case strings.Contains(path, "app.atchess.challengeNotification"):
		return EventTypeChallengeNotification
	case strings.Contains(path, "app.atchess.challengeAcceptance"):
		return EventTypeChallengeAcceptance
	case strings.Contains(path, "app.atchess.challenge"):
		return EventTypeChallenge
	default:
		return EventTypeGame
	}
}
