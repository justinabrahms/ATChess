package atproto

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateChallenge_WritesOnlyToOwnRepo is a regression test for
// atchess-1c9.11: CreateChallenge used to follow up its own-repo challenge
// write with a second, cross-repo createRecord call naming the challenged
// player's DID as "repo" -- a write AT Protocol never permits (you may only
// write to your own repo with your own session credentials). That call has
// been removed entirely (see atchess-1c9.11's notes: "leaving it invites a
// retry"). This asserts CreateChallenge issues exactly one createRecord
// call, into the caller's own repo, and never attempts to name any other
// repo.
func TestCreateChallenge_WritesOnlyToOwnRepo(t *testing.T) {
	var createRecordCalls int
	var lastRepo, lastCollection string
	var lastRecord map[string]interface{}

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:challenger123",
				"handle":    "challenger.test",
			})
		case "/xrpc/com.atproto.repo.createRecord":
			createRecordCalls++
			var req map[string]interface{}
			json.NewDecoder(r.Body).Decode(&req)
			lastRepo, _ = req["repo"].(string)
			lastCollection, _ = req["collection"].(string)
			lastRecord, _ = req["record"].(map[string]interface{})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:challenger123/app.atchess.challenge/chal789",
				"cid": "challenge-cid",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	client, err := NewClient(mockPDS.URL, "challenger.test", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	challenge, err := client.CreateChallenge(context.Background(), "did:plc:challenged456", "white", "gg")
	if err != nil {
		t.Fatalf("expected CreateChallenge to succeed with no cross-repo write attempted, got: %v", err)
	}
	if challenge == nil || challenge.ID == "" {
		t.Fatalf("expected a non-nil challenge with an ID, got %+v", challenge)
	}

	if createRecordCalls != 1 {
		t.Errorf("expected exactly 1 createRecord call (no follow-up cross-repo write), got %d", createRecordCalls)
	}
	if lastRepo != "did:plc:challenger123" {
		t.Errorf("expected the challenge record's repo to be the caller's own did (did:plc:challenger123), got %q", lastRepo)
	}
	if lastCollection != "app.atchess.challenge" {
		t.Errorf("expected collection app.atchess.challenge, got %q", lastCollection)
	}
	if got, _ := lastRecord["challengerHandle"].(string); got != "challenger.test" {
		t.Errorf("expected the challenge record to embed challengerHandle=%q, got %q", "challenger.test", got)
	}
	if got, _ := lastRecord["challenged"].(string); got != "did:plc:challenged456" {
		t.Errorf("expected the challenge record's challenged field to be did:plc:challenged456, got %q", got)
	}
}

// TestRespondToChallenge_WritesOnlyToOwnRepo asserts decline is expressed by
// a record in the RESPONDING player's own repository, referencing the
// original challenge by strongRef, never by writing into the challenger's
// repo (atchess-1c9.11).
func TestRespondToChallenge_WritesOnlyToOwnRepo(t *testing.T) {
	var lastRepo, lastCollection string
	var lastRecord map[string]interface{}

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:responder123",
				"handle":    "responder.test",
			})
		case "/xrpc/com.atproto.repo.createRecord":
			var req map[string]interface{}
			json.NewDecoder(r.Body).Decode(&req)
			lastRepo, _ = req["repo"].(string)
			lastCollection, _ = req["collection"].(string)
			lastRecord, _ = req["record"].(map[string]interface{})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:responder123/app.atchess.challengeResponse/resp1",
				"cid": "response-cid",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	client, err := NewClient(mockPDS.URL, "responder.test", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	challengeURI := "at://did:plc:challenger789/app.atchess.challenge/chal1"
	err = client.RespondToChallenge(context.Background(), challengeURI, "challenge-cid", "declined")
	if err != nil {
		t.Fatalf("expected RespondToChallenge to succeed, got: %v", err)
	}

	if lastRepo != "did:plc:responder123" {
		t.Errorf("expected the response record's repo to be the responder's own did, got %q", lastRepo)
	}
	if lastCollection != "app.atchess.challengeResponse" {
		t.Errorf("expected collection app.atchess.challengeResponse, got %q", lastCollection)
	}
	challengeRef, _ := lastRecord["challenge"].(map[string]interface{})
	if uri, _ := challengeRef["uri"].(string); uri != challengeURI {
		t.Errorf("expected the response record to reference challenge uri %q, got %q", challengeURI, uri)
	}
	if got, _ := lastRecord["response"].(string); got != "declined" {
		t.Errorf("expected response field to be \"declined\", got %q", got)
	}
}

// TestRespondToChallenge_RejectsUnsupportedResponse guards against silently
// writing a record with an out-of-lexicon response value.
func TestRespondToChallenge_RejectsUnsupportedResponse(t *testing.T) {
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:responder123",
				"handle":    "responder.test",
			})
		default:
			t.Errorf("unexpected request to %s; RespondToChallenge should have rejected the response value before making any write", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	client, err := NewClient(mockPDS.URL, "responder.test", "password")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	err = client.RespondToChallenge(context.Background(), "at://did:plc:challenger789/app.atchess.challenge/chal1", "cid", "accepted")
	if err == nil {
		t.Fatal("expected an error for an unsupported response value, got nil")
	}
}
