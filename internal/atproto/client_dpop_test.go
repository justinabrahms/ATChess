package atproto

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientWithDPoP(t *testing.T) {
	// Mock PDS server
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check authorization header
		authHeader := r.Header.Get("Authorization")
		
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			// Session creation doesn't use DPoP
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"accessJwt": "test-jwt-token",
				"did": "did:plc:testuser",
				"handle": "test.user"
			}`))
			
		case "/xrpc/com.atproto.repo.createRecord":
			// Check for DPoP header
			dpopHeader := r.Header.Get("DPoP")
			if dpopHeader == "" {
				t.Error("Expected DPoP header but not found")
			}
			
			// Check authorization uses DPoP scheme
			if !strings.HasPrefix(authHeader, "DPoP ") {
				t.Errorf("Expected Authorization header to start with 'DPoP ', got: %s", authHeader)
			}
			
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"uri": "at://did:plc:testuser/app.atchess.game/abc123",
				"cid": "test-cid"
			}`))
			
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()
	
	// Test with DPoP enabled
	client, err := NewClientWithDPoP(mockPDS.URL, "test.user", "password", true)
	if err != nil {
		t.Fatalf("Failed to create client with DPoP: %v", err)
	}
	
	// Verify DPoP manager was created
	if client.dpopManager == nil {
		t.Error("Expected DPoP manager to be created")
	}
	
	// Verify useDPoP flag is set
	if !client.useDPoP {
		t.Error("Expected useDPoP to be true")
	}
	
	// Test making a request
	_, err = client.CreateGame(nil, "did:plc:opponent", "white")
	if err != nil {
		t.Fatalf("Failed to create game: %v", err)
	}
}

func TestNewClientWithoutDPoP(t *testing.T) {
	// Mock PDS server
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check authorization header
		authHeader := r.Header.Get("Authorization")
		
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"accessJwt": "test-jwt-token",
				"did": "did:plc:testuser",
				"handle": "test.user"
			}`))
			
		case "/xrpc/com.atproto.repo.createRecord":
			// Check for absence of DPoP header
			dpopHeader := r.Header.Get("DPoP")
			if dpopHeader != "" {
				t.Error("Unexpected DPoP header found")
			}
			
			// Check authorization uses Bearer scheme
			if !strings.HasPrefix(authHeader, "Bearer ") {
				t.Errorf("Expected Authorization header to start with 'Bearer ', got: %s", authHeader)
			}
			
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"uri": "at://did:plc:testuser/app.atchess.game/abc123",
				"cid": "test-cid"
			}`))
			
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()
	
	// Test with DPoP disabled (default)
	client, err := NewClient(mockPDS.URL, "test.user", "password")
	if err != nil {
		t.Fatalf("Failed to create client without DPoP: %v", err)
	}
	
	// Verify DPoP manager was not created
	if client.dpopManager != nil {
		t.Error("Expected DPoP manager to be nil")
	}
	
	// Verify useDPoP flag is false
	if client.useDPoP {
		t.Error("Expected useDPoP to be false")
	}
	
	// Test making a request
	_, err = client.CreateGame(nil, "did:plc:opponent", "white")
	if err != nil {
		t.Fatalf("Failed to create game: %v", err)
	}
}