package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// WebSocket upgrader with reasonable settings
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins for now, tighten in production
		return true
	},
}

// Hub maintains active WebSocket connections
type Hub struct {
	// Registered clients by game ID
	gameClients map[string]map[*Client]bool
	
	// Broadcast channel for game updates
	broadcast chan GameUpdate
	
	// Register requests from clients
	register chan *Client
	
	// Unregister requests from clients
	unregister chan *Client
	
	mu sync.RWMutex
}

// Client represents a WebSocket connection
type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	gameID string
	userID string
}

// GameUpdate represents an update to broadcast
type GameUpdate struct {
	GameID string      `json:"gameId"`
	Type   string      `json:"type"` // "move", "draw_offer", "resignation", "game_end"
	Data   interface{} `json:"data"`
}

// NewHub creates a new WebSocket hub
func NewHub() *Hub {
	return &Hub{
		gameClients: make(map[string]map[*Client]bool),
		broadcast:   make(chan GameUpdate),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
	}
}

// Run starts the hub's main event loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if h.gameClients[client.gameID] == nil {
				h.gameClients[client.gameID] = make(map[*Client]bool)
			}
			h.gameClients[client.gameID][client] = true
			h.mu.Unlock()
			
			log.Info().
				Str("gameID", client.gameID).
				Str("userID", client.userID).
				Msg("Client connected to game")
			
		case client := <-h.unregister:
			h.mu.Lock()
			if clients, ok := h.gameClients[client.gameID]; ok {
				if _, ok := clients[client]; ok {
					delete(clients, client)
					close(client.send)
					
					// Clean up empty game rooms
					if len(clients) == 0 {
						delete(h.gameClients, client.gameID)
					}
				}
			}
			h.mu.Unlock()
			
			log.Info().
				Str("gameID", client.gameID).
				Str("userID", client.userID).
				Msg("Client disconnected from game")
			
		case update := <-h.broadcast:
			h.mu.RLock()
			clients := h.gameClients[update.GameID]
			h.mu.RUnlock()
			
			if clients != nil {
				message, err := json.Marshal(update)
				if err != nil {
					log.Error().Err(err).Msg("Failed to marshal game update")
					continue
				}
				
				for client := range clients {
					select {
					case client.send <- message:
					default:
						// Client's send channel is full, close it
						close(client.send)
						h.mu.Lock()
						delete(clients, client)
						h.mu.Unlock()
					}
				}
			}
		}
	}
}

// BroadcastGameUpdate sends an update to all clients watching a game
func (h *Hub) BroadcastGameUpdate(update GameUpdate) {
	select {
	case h.broadcast <- update:
	default:
		log.Warn().Str("gameID", update.GameID).Msg("Broadcast channel full, dropping update")
	}
}

// WebSocketHandler handles WebSocket upgrade requests
func (s *Service) WebSocketHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get game ID from query params
		gameID := r.URL.Query().Get("gameId")
		if gameID == "" {
			http.Error(w, "Missing gameId parameter", http.StatusBadRequest)
			return
		}
		
		// TODO: Get user ID from session/auth
		userID := "anonymous"
		
		// Upgrade connection
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Error().Err(err).Msg("Failed to upgrade WebSocket connection")
			return
		}
		
		// Create client
		client := &Client{
			hub:    hub,
			conn:   conn,
			send:   make(chan []byte, 256),
			gameID: gameID,
			userID: userID,
		}
		
		// Register client
		client.hub.register <- client
		
		// Start client goroutines
		go client.writePump()
		go client.readPump()
	}
}

// readPump handles incoming messages from the WebSocket
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Error().Err(err).Msg("WebSocket error")
			}
			break
		}
		
		// Handle incoming messages (ping/pong, etc.)
		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err == nil {
			if msg["type"] == "ping" {
				// Send pong response
				pong := map[string]string{"type": "pong"}
				if data, err := json.Marshal(pong); err == nil {
					select {
					case c.send <- data:
					default:
					}
				}
			}
		}
	}
}

// writePump handles sending messages to the WebSocket
func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			
			// Add queued messages to the current WebSocket message
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.send)
			}
			
			if err := w.Close(); err != nil {
				return
			}
			
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// BroadcastToGame sends an update to all clients watching a specific game
func (h *Hub) BroadcastToGame(gameID string, update GameUpdate) {
	update.GameID = gameID
	h.broadcast <- update
}

// BroadcastToPlayer sends an update to all clients for a specific player
func (h *Hub) BroadcastToPlayer(playerDID string, update GameUpdate) {
	// For now, we broadcast to all clients and let them filter
	// In a production system, you'd want to track clients by player DID
	update.Data = map[string]interface{}{
		"playerDID": playerDID,
		"data": update.Data,
	}
	h.broadcast <- update
}

// Integration with firehose events
func (h *Hub) HandleFirehoseEvent(ctx context.Context, eventType string, gameID string, data interface{}) {
	update := GameUpdate{
		GameID: gameID,
		Type:   eventType,
		Data:   data,
	}
	h.BroadcastGameUpdate(update)
}