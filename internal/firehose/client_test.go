package firehose

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

type mockWebSocketServer struct {
	*httptest.Server
	upgrader websocket.Upgrader
	messages [][]byte
	mu       sync.Mutex
}

func newMockWebSocketServer(messages [][]byte) *mockWebSocketServer {
	m := &mockWebSocketServer{
		upgrader: websocket.Upgrader{},
		messages: messages,
	}
	
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := m.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		
		// Send test messages
		m.mu.Lock()
		msgs := m.messages
		m.mu.Unlock()
		
		for _, msg := range msgs {
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		
		// Keep connection alive for pings
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	
	return m
}

func TestClient_Connect(t *testing.T) {
	server := newMockWebSocketServer(nil)
	defer server.Close()
	
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	
	var eventCount int
	handler := func(event Event) error {
		eventCount++
		return nil
	}
	
	client := NewClient(handler, WithURL(url))
	
	err := client.Start()
	if err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	
	// Give it time to connect
	time.Sleep(100 * time.Millisecond)
	
	if !client.IsConnected() {
		t.Error("Client should be connected")
	}
	
	err = client.Stop()
	if err != nil {
		t.Errorf("Failed to stop client: %v", err)
	}
	
	if client.IsConnected() {
		t.Error("Client should be disconnected after stop")
	}
}

func TestClient_ProcessChessEvents(t *testing.T) {
	// Create test messages
	messages := [][]byte{
		createTestMessage(1, "app.atchess.move", map[string]interface{}{
			"gameID": "game123",
			"move":   "e2e4",
			"player": "did:plc:player1",
		}),
		createTestMessage(2, "app.atchess.drawOffer", map[string]interface{}{
			"gameID": "game123",
			"player": "did:plc:player2",
		}),
	}
	
	server := newMockWebSocketServer(messages)
	defer server.Close()
	
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	
	events := make([]Event, 0)
	var mu sync.Mutex
	
	handler := func(event Event) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	}
	
	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler, WithURL(url), WithLogger(logger))
	
	err := client.Start()
	if err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()
	
	// Wait for events to be processed
	time.Sleep(200 * time.Millisecond)
	
	mu.Lock()
	defer mu.Unlock()
	
	if len(events) != 2 {
		t.Errorf("Expected 2 events, got %d", len(events))
	}
	
	if len(events) > 0 {
		if events[0].Type != EventTypeMove {
			t.Errorf("Expected first event to be move, got %s", events[0].Type)
		}
		if events[0].Path != "app.atchess.move" {
			t.Errorf("Expected path app.atchess.move, got %s", events[0].Path)
		}
	}
	
	if len(events) > 1 {
		if events[1].Type != EventTypeDrawOffer {
			t.Errorf("Expected second event to be draw offer, got %s", events[1].Type)
		}
	}
}

func TestClient_Reconnection(t *testing.T) {
	// Test server that closes connection after first message
	var connectionCount int
	var mu sync.Mutex
	connectedCh := make(chan int, 10)
	
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectionCount++
		count := connectionCount
		mu.Unlock()
		
		// Signal connection
		connectedCh <- count
		
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		
		// First connection: send a message then close
		if count == 1 {
			msg := createTestMessage(1, "app.atchess.move", map[string]interface{}{
				"gameID": "game123",
				"move":   "e2e4",
			})
			conn.WriteMessage(websocket.BinaryMessage, msg)
			time.Sleep(10 * time.Millisecond)
			return
		}
		
		// Second connection: stay alive
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()
	
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	
	var eventCount int
	handler := func(event Event) error {
		eventCount++
		return nil
	}
	
	logger := zerolog.New(zerolog.NewTestWriter(t))
	client := NewClient(handler, 
		WithURL(url), 
		WithLogger(logger),
		WithInitialReconnectDelay(100 * time.Millisecond))
	
	err := client.Start()
	if err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()
	
	// Wait for first connection
	select {
	case n := <-connectedCh:
		t.Logf("First connection established: %d", n)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for first connection")
	}
	
	// Wait for reconnection
	select {
	case n := <-connectedCh:
		t.Logf("Second connection established: %d", n)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for reconnection")
	}
	
	// Give some time to stabilize
	time.Sleep(50 * time.Millisecond)
	
	mu.Lock()
	finalCount := connectionCount
	mu.Unlock()
	
	if finalCount < 2 {
		t.Errorf("Expected at least 2 connections (reconnection), got %d", finalCount)
	}
	
	if !client.IsConnected() {
		t.Error("Client should be connected after reconnection")
	}
}

func TestClient_SequenceTracking(t *testing.T) {
	messages := [][]byte{
		createTestMessage(100, "app.atchess.move", map[string]interface{}{
			"gameID": "game123",
			"move":   "e2e4",
		}),
		createTestMessage(101, "app.atchess.move", map[string]interface{}{
			"gameID": "game123",
			"move":   "e7e5",
		}),
	}
	
	server := newMockWebSocketServer(messages)
	defer server.Close()
	
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	
	handler := func(event Event) error {
		return nil
	}
	
	client := NewClient(handler, WithURL(url))
	
	err := client.Start()
	if err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()
	
	// Wait for messages to be processed
	time.Sleep(200 * time.Millisecond)
	
	if client.lastSequence != 101 {
		t.Errorf("Expected last sequence to be 101, got %d", client.lastSequence)
	}
}

// Helper function to create test messages
func createTestMessage(seq int64, recordPath string, recordData map[string]interface{}) []byte {
	// Create a simplified test message format
	// In reality, this would include CAR data, but for testing we'll use a simpler format
	
	header := map[string]interface{}{
		"op":  1,
		"t":   "#commit",
		"seq": seq,
		"repo": "did:plc:testuser",
		"rev":  "testrev",
		"ops": []map[string]interface{}{
			{
				"action": "create",
				"path":   recordPath,
				"cid":    "testcid",
			},
		},
	}
	
	headerBytes, _ := json.Marshal(header)
	headerLen := len(headerBytes)
	
	// Create message with header length prefix
	message := make([]byte, 4+headerLen)
	message[0] = byte(headerLen >> 24)
	message[1] = byte(headerLen >> 16)
	message[2] = byte(headerLen >> 8)
	message[3] = byte(headerLen)
	copy(message[4:], headerBytes)
	
	// In a real scenario, we'd append CAR data here
	// For testing, we'll skip that part since the extraction would fail anyway
	
	return message
}

func TestIsChessRecord(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"app.atchess.move", true},
		{"app.atchess.game", true},
		{"app.atchess.drawOffer", true},
		{"app.atchess.resignation", true},
		{"app.atchess.challenge", true},
		{"app.atchess.challengeAcceptance", true},
		{"app.bsky.feed.post", false},
		{"com.atproto.repo.createRecord", false},
		{"", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := isChessRecord(tt.path)
			if result != tt.expected {
				t.Errorf("isChessRecord(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestGetEventType(t *testing.T) {
	tests := []struct {
		path     string
		expected EventType
	}{
		{"app.atchess.move", EventTypeMove},
		{"app.atchess.drawOffer", EventTypeDrawOffer},
		{"app.atchess.resignation", EventTypeResignation},
		{"app.atchess.game", EventTypeGame},
		{"app.atchess.challenge", EventTypeChallenge},
		{"app.atchess.challengeAcceptance", EventTypeChallengeAcceptance},
		{"app.atchess.unknown", EventTypeGame}, // default
	}
	
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := getEventType(tt.path)
			if result != tt.expected {
				t.Errorf("getEventType(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}