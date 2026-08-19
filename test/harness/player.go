//go:build e2e

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// sessionHeaderTransport injects X-Session-ID into every outgoing request,
// so a Player's HTTP client automatically authenticates against the
// protocol service without callers having to remember the header.
type sessionHeaderTransport struct {
	sessionID string
	base      http.RoundTripper
}

func (t *sessionHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if t.sessionID != "" {
		cloned.Header.Set("X-Session-ID", t.sessionID)
	}
	return t.base.RoundTrip(cloned)
}

// Player represents one authenticated participant in the harness: their AT
// Protocol identity, the protocol-service instance that authenticated them,
// and an HTTP client pre-configured to hit that service's public API.
type Player struct {
	Handle      string
	DID         string
	PDSURL      string
	SessionID   string
	ProtocolURL string

	// HTTPClient talks to this player's protocol-service instance
	// (ProtocolURL) and automatically sets X-Session-ID on every request.
	HTTPClient *http.Client
}

// loginRequest/loginResponse mirror internal/web/service.go's AuthRequest /
// AuthResponse. Duplicated here deliberately: the harness must not import
// internal/... packages, since the point of the harness is to exercise the
// real public HTTP boundary rather than shortcut it.
type loginRequest struct {
	Handle   string `json:"handle"`
	Password string `json:"password"`
}

type loginResponse struct {
	Success     bool   `json:"success"`
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	AccessToken string `json:"accessToken"`
	Error       string `json:"error,omitempty"`
}

// NewPlayer authenticates account against the protocol-service instance
// running at protocolURL via the public POST /api/auth/login endpoint (the
// same surface a browser uses), and returns a Player wired up with the
// resulting session id.
//
// It fails the test loudly (not silently) if login returns 200 with an
// empty session id -- a silent empty token would otherwise surface much
// later as a confusing 401 on some unrelated call.
func NewPlayer(t *testing.T, account Account, protocolURL string) *Player {
	t.Helper()

	reqBody, err := json.Marshal(loginRequest{
		Handle:   account.Handle,
		Password: account.Password,
	})
	if err != nil {
		t.Fatalf("failed to marshal login request for %s: %v", account.Handle, err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(protocolURL+"/api/auth/login", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("login request to %s for %s failed: %v", protocolURL, account.Handle, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read login response body from %s for %s: %v", protocolURL, account.Handle, err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login for %s against %s returned HTTP %d: %s", account.Handle, protocolURL, resp.StatusCode, string(body))
	}

	var loginResp loginResponse
	if err := json.Unmarshal(body, &loginResp); err != nil {
		t.Fatalf("failed to decode login response from %s for %s: %v (body: %s)", protocolURL, account.Handle, err, string(body))
	}

	if !loginResp.Success {
		t.Fatalf("login for %s against %s reported success=false: %s (body: %s)", account.Handle, protocolURL, loginResp.Error, string(body))
	}

	// Edge case called out explicitly in the brief: a 200 with an empty
	// session id must fail loudly here, not three calls later as a
	// mysterious 401.
	if loginResp.AccessToken == "" {
		t.Fatalf("login for %s against %s returned HTTP 200 success=true but an EMPTY session id (accessToken); refusing to silently proceed (body: %s)", account.Handle, protocolURL, string(body))
	}

	if loginResp.DID == "" {
		t.Fatalf("login for %s against %s returned HTTP 200 success=true but an EMPTY did (body: %s)", account.Handle, protocolURL, string(body))
	}

	if account.DID != "" && loginResp.DID != account.DID {
		t.Fatalf("login for %s against %s returned did %q but the harness account file says %q", account.Handle, protocolURL, loginResp.DID, account.DID)
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &sessionHeaderTransport{
			sessionID: loginResp.AccessToken,
			base:      http.DefaultTransport,
		},
	}

	return &Player{
		Handle:      loginResp.Handle,
		DID:         loginResp.DID,
		PDSURL:      account.PDSURL,
		SessionID:   loginResp.AccessToken,
		ProtocolURL: protocolURL,
		HTTPClient:  httpClient,
	}
}

// repoRecord mirrors the relevant fields of com.atproto.repo.getRecord's
// response.
type repoRecord struct {
	URI   string                 `json:"uri"`
	CID   string                 `json:"cid"`
	Value map[string]interface{} `json:"value"`
}

// RepoGetRecord fetches a single record directly from this player's OWN PDS
// (com.atproto.repo.getRecord against p.PDSURL) -- NOT through the protocol
// service. This direct-to-PDS read is the entire point of the harness: it
// is how downstream conformance tests prove where a record actually landed,
// independent of what the protocol service claims.
func (p *Player) RepoGetRecord(collection, rkey string) (map[string]interface{}, error) {
	u := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=%s&rkey=%s",
		p.PDSURL, url.QueryEscape(p.DID), url.QueryEscape(collection), url.QueryEscape(rkey))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("getRecord request to %s failed: %w", p.PDSURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read getRecord response body from %s: %w", p.PDSURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getRecord(%s, %s) against %s (repo %s) returned HTTP %d: %s",
			collection, rkey, p.PDSURL, p.DID, resp.StatusCode, string(body))
	}

	var record repoRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("failed to decode getRecord response from %s: %w (body: %s)", p.PDSURL, err, string(body))
	}

	return record.Value, nil
}

// repoListRecordsResponse mirrors the relevant fields of
// com.atproto.repo.listRecords's response.
type repoListRecordsResponse struct {
	Records []struct {
		URI   string                 `json:"uri"`
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	} `json:"records"`
	Cursor string `json:"cursor,omitempty"`
}

// RepoListRecords lists records in the given collection directly from this
// player's OWN PDS (com.atproto.repo.listRecords against p.PDSURL) -- NOT
// through the protocol service. See RepoGetRecord for why this matters.
func (p *Player) RepoListRecords(collection string) ([]map[string]interface{}, error) {
	u := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=%s&limit=100",
		p.PDSURL, url.QueryEscape(p.DID), url.QueryEscape(collection))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("listRecords request to %s failed: %w", p.PDSURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read listRecords response body from %s: %w", p.PDSURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listRecords(%s) against %s (repo %s) returned HTTP %d: %s",
			collection, p.PDSURL, p.DID, resp.StatusCode, string(body))
	}

	var listResp repoListRecordsResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("failed to decode listRecords response from %s: %w (body: %s)", p.PDSURL, err, string(body))
	}

	values := make([]map[string]interface{}, 0, len(listResp.Records))
	for _, r := range listResp.Records {
		values = append(values, r.Value)
	}
	return values, nil
}
