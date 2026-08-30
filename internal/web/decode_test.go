package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The case that was actually reported, kept verbatim so it cannot be
// paraphrased away: a handle pasted with two trailing spaces made both sign-in
// and challenge creation fail, with an error message that named neither the
// whitespace nor the handle.
const pastedHandle = "atchess-player1.bsky.social  "

func TestDecodeTrimsTheHandleThatBrokeSignIn(t *testing.T) {
	body := `{"opponent_did":"` + pastedHandle + `","color":"random"}`
	r := httptest.NewRequest(http.MethodPost, "/api/challenges", strings.NewReader(body))

	var req CreateChallengeRequest
	if err := decodeJSONBody(r, &req); err != nil {
		t.Fatalf("decodeJSONBody: %v", err)
	}
	if req.OpponentDID != "atchess-player1.bsky.social" {
		t.Errorf("handle came through as %q, want it trimmed.\n"+
			"This exact value made handle resolution fail with "+
			"'label \"social  \" must contain only ASCII letters, digits and hyphens'.",
			req.OpponentDID)
	}
}

func TestDecodeTrimsEveryStringShape(t *testing.T) {
	type inner struct {
		Deep string `json:"deep"`
	}
	type payload struct {
		Top    string            `json:"top"`
		Nested inner             `json:"nested"`
		Ptr    *inner            `json:"ptr"`
		List   []string          `json:"list"`
		Map    map[string]string `json:"map"`
		Secret string            `json:"secret" trim:"-"`
		hidden string            //nolint:unused // unexported: must not panic
	}

	body := `{"top":"  a  ","nested":{"deep":"  b  "},"ptr":{"deep":"  c  "},` +
		`"list":["  d  "],"map":{"k":"  e  "},"secret":"  keep me  "}`
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))

	var p payload
	if err := decodeJSONBody(r, &p); err != nil {
		t.Fatalf("decodeJSONBody: %v", err)
	}
	_ = p.hidden

	for name, got := range map[string]string{
		"top":    p.Top,
		"nested": p.Nested.Deep,
		"ptr":    p.Ptr.Deep,
		"list":   p.List[0],
		"map":    p.Map["k"],
	} {
		if strings.TrimSpace(got) != got {
			t.Errorf("%s was not trimmed: %q", name, got)
		}
	}

	// The opt-out is the half that protects credentials. A password may end in
	// a space, and quietly changing it is an auth failure with no visible cause.
	if p.Secret != "  keep me  " {
		t.Errorf(`a trim:"-" field was trimmed: %q — passwords must survive verbatim`, p.Secret)
	}
}

// TestNoHandlerDecodesDirectly is what makes the fix hold. There were nine
// decode sites; fixing only the two that had been reported would have left
// seven, and the next handler would have made it eight.
func TestNoHandlerDecodesDirectly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		werr := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			base := filepath.Base(path)
			if strings.HasSuffix(base, "_test.go") || base == "decode.go" {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if strings.Contains(string(b), "json.NewDecoder(r.Body).Decode") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
			}
			return nil
		})
		if werr != nil {
			t.Fatalf("walking %s: %v", dir, werr)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%d file(s) decode a request body directly instead of using decodeJSONBody:\n  %s\n\n"+
			"Direct decoding skips input trimming. A handle pasted with trailing "+
			"whitespace then fails resolution with an error that names neither the "+
			"whitespace nor the field.", len(offenders), strings.Join(offenders, "\n  "))
	}
}

// A bad handle is the caller's mistake. Returning 500 tells the user the
// server is broken about a typo, and tells error-rate monitoring the same
// thing — nobody is served by the wrong number.
func TestBadHandleIsAClientError(t *testing.T) {
	clientFaults := []string{
		`invalid handle "atchess-player1.bsky.social  ": label "social  " must contain only ASCII letters, digits and hyphens`,
		"failed to resolve handle: Unable to resolve handle",
		"invalid DID format",
	}
	for _, msg := range clientFaults {
		if got := statusForError(errString(msg)); got != http.StatusBadRequest {
			t.Errorf("statusForError(%q) = %d, want 400", msg, got)
		}
	}

	ourFaults := []string{
		"connection refused",
		"context deadline exceeded",
		"unable to open database file (14)",
	}
	for _, msg := range ourFaults {
		if got := statusForError(errString(msg)); got != http.StatusInternalServerError {
			t.Errorf("statusForError(%q) = %d, want 500 — an unrecognised failure is ours until shown otherwise", msg, got)
		}
	}

	if got := statusForError(nil); got != http.StatusInternalServerError {
		t.Errorf("statusForError(nil) = %d", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
