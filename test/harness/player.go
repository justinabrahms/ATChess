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

// repoListRecordsPageCap bounds how many pages RepoListRecords will follow
// for a single collection before failing closed (atchess-1c9.119
// fix-pass): this harness helper used to request a single page
// (limit=100), decode the response's Cursor field, and then never follow
// it -- exactly the same defect that produced this bead, just in test
// code instead of internal/atproto/client.go. Several e2e assertions
// (challenge_delivery_test.go, ownership_test.go) count RepoListRecords'
// result to prove "exactly one [record exists]"; on a repo that has
// accumulated more than 100 records in the collection being checked,
// those counts would silently be wrong -- the harness would go blind past
// page one exactly like the production bug it exists to catch. 50 pages
// (5,000 records) is far more than any e2e run in this repo accumulates
// per collection per account; exceeding it is treated as a hard test
// failure (returned error) rather than a silently truncated count.
const repoListRecordsPageCap = 50

// RepoListRecords lists ALL records in the given collection directly from
// this player's OWN PDS (com.atproto.repo.listRecords against p.PDSURL,
// following the response cursor across pages until the collection is
// exhausted -- see repoListRecordsPageCap) -- NOT through the protocol
// service. See RepoGetRecord for why this matters.
func (p *Player) RepoListRecords(collection string) ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	var values []map[string]interface{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= repoListRecordsPageCap {
			return nil, fmt.Errorf("listRecords(%s) against %s (repo %s): exceeded %d pages (%d records read so far) without exhausting the collection",
				collection, p.PDSURL, p.DID, repoListRecordsPageCap, len(values))
		}

		u := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=%s&limit=100",
			p.PDSURL, url.QueryEscape(p.DID), url.QueryEscape(collection))
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}

		resp, err := client.Get(u)
		if err != nil {
			return nil, fmt.Errorf("listRecords request to %s failed: %w", p.PDSURL, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
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

		for _, r := range listResp.Records {
			values = append(values, r.Value)
		}

		if listResp.Cursor == "" {
			break
		}
		cursor = listResp.Cursor
	}

	if values == nil {
		values = []map[string]interface{}{}
	}
	return values, nil
}
