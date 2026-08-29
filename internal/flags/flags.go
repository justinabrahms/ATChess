// Package flags reads feature flag state from the fleet's flags service.
//
// WHY THIS EXISTS. This repository is a workload for an agent pipeline whose
// second human gate is "flip the flag after watching it demo". That gate only
// exists if shipped code can be live and off at the same time — deployed dark.
// Without a flag client there is no dark, so every merge is a release and the
// human has nothing left to decide.
//
// FAIL CLOSED. If the flags service is unreachable, absent, or malformed, every
// flag reads FALSE. This is the opposite of what the board does, and the
// difference is deliberate: the board fails open to a file because a board that
// renders every feature off looks exactly like a mass rollback and panics the
// operator. Here, a flag guards a feature that has never been turned on for
// anyone. Reading an unknown flag as ON would turn an unreviewed feature loose
// precisely when the service that governs it is broken — a dark launch that
// goes live because of an outage. An absent answer is not permission.
//
// PREVIEW IS THE POINT. The flags service applies per-request overrides from
// `?<repo>.<feature>=true` and from an `X-Fleet-Flags` header, which is how a
// human sees a dark feature without enabling it for anyone. That only works if
// the incoming request's overrides reach the service, so Enabled takes the
// request and forwards them. A client that evaluates flags without the request
// is a client that silently breaks gate 2 while every unit test passes.
package flags

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// The service is on localhost and answers in well under a millisecond, so
	// anything past a few tens of milliseconds is absence, not slowness. The
	// expensive case is never a refused connection — that fails instantly — it
	// is a port that accepts and never replies, where every request pays the
	// whole budget with nothing logged.
	defaultTimeout = 250 * time.Millisecond

	// How long a failure is remembered, so a dead service costs one timeout
	// rather than one per request. Short, because a flip is supposed to show up
	// without a restart: at worst a flip made during an outage is invisible for
	// this long.
	downFor = 5 * time.Second

	evaluatePath = "/ofrep/v1/evaluate/flags"
	overrideHdr  = "X-Fleet-Flags"
)

// ofrepValue is one flag in the service's OFREP evaluation shape.
type ofrepValue struct {
	Key   string `json:"key"`
	Value bool   `json:"value"`
}

type ofrepResponse struct {
	Flags []ofrepValue `json:"flags"`
}

// Client evaluates flags against the flags service.
type Client struct {
	baseURL string
	http    *http.Client

	mu        sync.Mutex
	down      bool
	downUntil time.Time
}

// New returns a client for the flags service at baseURL (e.g.
// "http://localhost:7780").
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// isDown reports whether the service is inside its remembered outage window.
func (c *Client) isDown(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.down && now.Before(c.downUntil)
}

func (c *Client) markDown(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = true
	c.downUntil = now.Add(downFor)
}

func (c *Client) markUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = false
}

// overridesFrom builds an X-Fleet-Flags value carrying this request's flag
// overrides, so a preview survives the hop to the flags service.
//
// Both sources the service honours are forwarded: an existing X-Fleet-Flags
// header, and any dotted query parameter. Dotted is the whole test — flags are
// keyed `<repo>.<feature>`, so a parameter without a dot is some other query
// parameter and forwarding it would let an unrelated `?sort=true` collide with
// a flag name.
func overridesFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	var parts []string
	if h := strings.TrimSpace(r.Header.Get(overrideHdr)); h != "" {
		parts = append(parts, h)
	}
	if r.URL != nil {
		for k, vs := range r.URL.Query() {
			if !strings.Contains(k, ".") || len(vs) == 0 {
				continue
			}
			parts = append(parts, k+"="+vs[len(vs)-1])
		}
	}
	return strings.Join(parts, ",")
}

// Enabled reports whether key is on for this request.
//
// r may be nil for a background caller with no request context; the flag is
// then evaluated without overrides. Any failure — unreachable service, non-200,
// unparsable body, unknown key — reads false. See the package comment.
func (c *Client) Enabled(ctx context.Context, key string, r *http.Request) bool {
	now := time.Now()
	if c.isDown(now) {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+evaluatePath, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if ov := overridesFrom(r); ov != "" {
		req.Header.Set(overrideHdr, ov)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.markDown(now)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// A non-200 is a service that is answering but not agreeing. Not an
		// outage — do not suppress subsequent lookups — but not permission
		// either.
		return false
	}
	c.markUp()

	var out ofrepResponse
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false
	}
	for _, f := range out.Flags {
		if f.Key == key {
			return f.Value
		}
	}
	// The service answered and does not know this key. An unheard-of flag is
	// off; that is how a slice ships dark before anyone has declared it.
	return false
}
