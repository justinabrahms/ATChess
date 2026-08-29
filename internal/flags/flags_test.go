package flags

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The behaviour under test is mostly about what happens when things go WRONG,
// because that is where a flag client decides whether an unreviewed feature
// goes live. A client that reads a broken flags service as "on" turns a dark
// launch into a release triggered by an outage.

func serviceReturning(t *testing.T, flags map[string]bool, capture *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.Header.Get(overrideHdr)
		}
		out := ofrepResponse{}
		for k, v := range flags {
			out.Flags = append(out.Flags, ofrepValue{Key: k, Value: v})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestEnabledReadsTheService(t *testing.T) {
	srv := serviceReturning(t, map[string]bool{
		"atchess.pgn_export": true,
		"atchess.ratings":    false,
	}, nil)
	defer srv.Close()

	c := New(srv.URL)
	if !c.Enabled(context.Background(), "atchess.pgn_export", nil) {
		t.Error("a flag the service reports as on read as off")
	}
	if c.Enabled(context.Background(), "atchess.ratings", nil) {
		t.Error("a flag the service reports as off read as on")
	}
}

// Each of these is a way the flag service can betray a caller. Every one must
// read false. If any reads true, an unreviewed feature ships because something
// unrelated broke.
func TestEveryFailureReadsFalse(t *testing.T) {
	t.Run("unreachable service", func(t *testing.T) {
		// A port nothing is listening on: connection refused, immediately.
		c := New("http://127.0.0.1:1")
		if c.Enabled(context.Background(), "atchess.pgn_export", nil) {
			t.Error("an unreachable flags service read as ON — an outage would ship the feature")
		}
	})

	t.Run("non-200 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		if New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", nil) {
			t.Error("a 500 from the flags service read as ON")
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json at all"))
		}))
		defer srv.Close()
		if New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", nil) {
			t.Error("an unparsable flags response read as ON")
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		srv := serviceReturning(t, map[string]bool{"atchess.something_else": true}, nil)
		defer srv.Close()
		if New(srv.URL).Enabled(context.Background(), "atchess.never_declared", nil) {
			t.Error("a key the service does not know read as ON — a slice must ship dark")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		srv := serviceReturning(t, map[string]bool{"atchess.pgn_export": true}, nil)
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if New(srv.URL).Enabled(ctx, "atchess.pgn_export", nil) {
			t.Error("a cancelled request read as ON")
		}
	})
}

// PREVIEW. Gate 2 depends on a human seeing a dark feature without enabling it,
// which the flags service implements via per-request overrides. Those overrides
// arrive on the INBOUND request and mean nothing unless this client forwards
// them. Nothing else in the suite would notice if it stopped.
func TestOverridesAreForwarded(t *testing.T) {
	t.Run("dotted query parameter", func(t *testing.T) {
		var got string
		srv := serviceReturning(t, map[string]bool{"atchess.pgn_export": false}, &got)
		defer srv.Close()

		r := httptest.NewRequest(http.MethodGet, "/games/1?atchess.pgn_export=true", nil)
		New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", r)

		if !strings.Contains(got, "atchess.pgn_export=true") {
			t.Errorf("the preview override did not reach the flags service; it sent %q\n"+
				"Without this, ?atchess.pgn_export=true renders nothing and gate 2 "+
				"has no way to demo a dark slice.", got)
		}
	})

	t.Run("inbound X-Fleet-Flags header", func(t *testing.T) {
		var got string
		srv := serviceReturning(t, map[string]bool{"atchess.pgn_export": false}, &got)
		defer srv.Close()

		r := httptest.NewRequest(http.MethodGet, "/games/1", nil)
		r.Header.Set(overrideHdr, "atchess.pgn_export=true")
		New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", r)

		if !strings.Contains(got, "atchess.pgn_export=true") {
			t.Errorf("an inbound %s header was not forwarded; sent %q", overrideHdr, got)
		}
	})

	t.Run("undotted parameters are not forwarded", func(t *testing.T) {
		var got string
		srv := serviceReturning(t, map[string]bool{}, &got)
		defer srv.Close()

		// `sort` is an ordinary query parameter. Forwarding it would let
		// unrelated request state collide with the flag namespace.
		r := httptest.NewRequest(http.MethodGet, "/games?sort=true&page=2", nil)
		New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", r)

		if strings.Contains(got, "sort") || strings.Contains(got, "page") {
			t.Errorf("an undotted query parameter was forwarded as a flag override: %q", got)
		}
	})
}

// A nil request is legitimate for a background caller. It must evaluate without
// overrides rather than panicking, which would take the service down from a
// cron path that no HTTP test covers.
func TestNilRequestIsSafe(t *testing.T) {
	var got string
	srv := serviceReturning(t, map[string]bool{"atchess.pgn_export": true}, &got)
	defer srv.Close()

	if !New(srv.URL).Enabled(context.Background(), "atchess.pgn_export", nil) {
		t.Error("a nil request should still evaluate the flag")
	}
	if got != "" {
		t.Errorf("a nil request sent overrides: %q", got)
	}
}

// After an outage the client stops asking for a few seconds. That must not
// leave it stuck off once the service returns — a flip that never takes effect
// looks like a broken feature rather than a stale client.
func TestOutageIsRememberedThenForgotten(t *testing.T) {
	c := New("http://127.0.0.1:1")
	if c.Enabled(context.Background(), "atchess.pgn_export", nil) {
		t.Fatal("unreachable service read as ON")
	}
	if !c.isDown(time.Now()) {
		t.Error("an outage was not remembered; every request will pay the timeout again")
	}
	// The window is what bounds it. Past the window the client must be willing
	// to ask again, or a recovered service is invisible until restart.
	if c.isDown(time.Now().Add(downFor + time.Second)) {
		t.Error("the outage is remembered past its window; a recovered service would stay invisible")
	}
}
