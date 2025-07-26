package atproto

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateChallengeNotification(t *testing.T) {
	// Mock server to simulate PDS
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:test123",
				"handle":    "test.user",
			})
		case "/xrpc/com.atproto.repo.createRecord":
			// Verify the request is creating a challenge notification
			var req map[string]interface{}
			json.NewDecoder(r.Body).Decode(&req)
			
			if req["collection"] != "app.atchess.challengeNotification" {
				t.Errorf("Expected collection app.atchess.challengeNotification, got %v", req["collection"])
			}
			
			record := req["record"].(map[string]interface{})
			if record["challenger"] != "did:plc:test123" {
				t.Errorf("Expected challenger DID to be client's DID, got %v", record["challenger"])
			}
			
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:challenged456/app.atchess.challengeNotification/test123",
				"cid": "test-cid",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	// Create client
	client, err := NewClient(mockPDS.URL, "test.user", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Test creating a challenge notification
	timeControl := map[string]interface{}{
		"daysPerMove": 3,
		"type":        "correspondence",
	}
	err = client.CreateChallengeNotification(
		context.Background(),
		"did:plc:challenged456",
		"at://did:plc:challenger123/app.atchess.challenge/abc123",
		"challenge-cid",
		"challenger.handle",
		"white",
		"Let's play!",
		timeControl,
	)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
}

func TestGetChallengeNotifications(t *testing.T) {
	// Mock server
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:test123",
				"handle":    "test.user",
			})
		case "/xrpc/com.atproto.repo.listRecords":
			// Return mock challenge notifications
			now := time.Now()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"records": []map[string]interface{}{
					{
						"uri": "at://did:plc:test123/app.atchess.challengeNotification/notif1",
						"cid": "cid1",
						"value": map[string]interface{}{
							"createdAt": now.Add(-1 * time.Hour).Format(time.RFC3339),
							"challenge": map[string]interface{}{
								"uri": "at://did:plc:challenger1/app.atchess.challenge/chal1",
								"cid": "chalcid1",
							},
							"challenger":       "did:plc:challenger1",
							"challengerHandle": "player1.chess",
							"timeControl": map[string]interface{}{
								"daysPerMove": 3,
								"type":        "correspondence",
							},
							"color":     "white",
							"message":   "Good luck!",
							"expiresAt": now.Add(23 * time.Hour).Format(time.RFC3339),
						},
					},
					{
						"uri": "at://did:plc:test123/app.atchess.challengeNotification/notif2",
						"cid": "cid2",
						"value": map[string]interface{}{
							"createdAt": now.Add(-30 * time.Minute).Format(time.RFC3339),
							"challenge": map[string]interface{}{
								"uri": "at://did:plc:challenger2/app.atchess.challenge/chal2",
								"cid": "chalcid2",
							},
							"challenger":       "did:plc:challenger2",
							"challengerHandle": "player2.chess",
							"timeControl": map[string]interface{}{
								"daysPerMove": 1,
								"type":        "correspondence",
							},
							"color":     "random",
							"expiresAt": now.Add(23*time.Hour + 30*time.Minute).Format(time.RFC3339),
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	// Create client
	client, err := NewClient(mockPDS.URL, "test.user", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Get notifications
	notifications, err := client.GetChallengeNotifications(context.Background())
	if err != nil {
		t.Fatalf("Failed to get notifications: %v", err)
	}

	// Verify results
	if len(notifications) != 2 {
		t.Errorf("Expected 2 notifications, got %d", len(notifications))
	}

	// Check first notification
	if notifications[0].ChallengerHandle != "player1.chess" {
		t.Errorf("Expected challenger handle player1.chess, got %s", notifications[0].ChallengerHandle)
	}
	if daysPerMove, ok := notifications[0].TimeControl["daysPerMove"].(float64); !ok || int(daysPerMove) != 3 {
		t.Errorf("Expected 3 days per move, got %v", notifications[0].TimeControl["daysPerMove"])
	}
	if notifications[0].Color != "white" {
		t.Errorf("Expected color white, got %s", notifications[0].Color)
	}
}

func TestDeleteChallengeNotification(t *testing.T) {
	deleteCalled := false
	
	// Mock server
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:test123",
				"handle":    "test.user",
			})
		case "/xrpc/com.atproto.repo.deleteRecord":
			deleteCalled = true
			
			// Verify the request
			var req map[string]interface{}
			json.NewDecoder(r.Body).Decode(&req)
			
			if req["collection"] != "app.atchess.challengeNotification" {
				t.Errorf("Expected collection app.atchess.challengeNotification, got %v", req["collection"])
			}
			if req["rkey"] != "notif123" {
				t.Errorf("Expected rkey notif123, got %v", req["rkey"])
			}
			
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	// Create client
	client, err := NewClient(mockPDS.URL, "test.user", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Delete notification - need to provide full URI
	err = client.DeleteChallengeNotification(context.Background(), "at://did:plc:test123/app.atchess.challengeNotification/notif123")
	if err != nil {
		t.Errorf("Failed to delete notification: %v", err)
	}

	if !deleteCalled {
		t.Error("Delete endpoint was not called")
	}
}

func TestChallengeNotificationExpiration(t *testing.T) {
	// Mock server that returns both expired and valid notifications
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:test123",
				"handle":    "test.user",
			})
		case "/xrpc/com.atproto.repo.listRecords":
			now := time.Now()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"records": []map[string]interface{}{
					{
						// Valid notification
						"uri": "at://did:plc:test123/app.atchess.challengeNotification/valid",
						"cid": "cid1",
						"value": map[string]interface{}{
							"createdAt":        now.Add(-1 * time.Hour).Format(time.RFC3339),
							"challenger":       "did:plc:challenger1",
							"challengerHandle": "player1.chess",
							"challenge": map[string]interface{}{
								"uri": "at://challenge1",
								"cid": "chalcid1",
							},
							"expiresAt": now.Add(1 * time.Hour).Format(time.RFC3339), // Future
						},
					},
					{
						// Expired notification
						"uri": "at://did:plc:test123/app.atchess.challengeNotification/expired",
						"cid": "cid2",
						"value": map[string]interface{}{
							"createdAt":        now.Add(-25 * time.Hour).Format(time.RFC3339),
							"challenger":       "did:plc:challenger2",
							"challengerHandle": "player2.chess",
							"challenge": map[string]interface{}{
								"uri": "at://challenge2",
								"cid": "chalcid2",
							},
							"expiresAt": now.Add(-1 * time.Hour).Format(time.RFC3339), // Past
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	// Create client
	client, err := NewClient(mockPDS.URL, "test.user", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Get notifications - should filter out expired ones
	notifications, err := client.GetChallengeNotifications(context.Background())
	if err != nil {
		t.Fatalf("Failed to get notifications: %v", err)
	}

	// Should only return 1 valid notification
	if len(notifications) != 1 {
		t.Errorf("Expected 1 valid notification, got %d", len(notifications))
	}

	if len(notifications) > 0 && notifications[0].ChallengerHandle != "player1.chess" {
		t.Errorf("Expected valid notification from player1.chess, got %s", notifications[0].ChallengerHandle)
	}
}