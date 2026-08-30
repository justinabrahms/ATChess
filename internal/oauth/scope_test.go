package oauth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// SCOPE CREEP IS THE THING TO CATCH HERE.
//
// The consent screen a user sees is generated from what we ask for. Adding one
// word to a scope string silently widens what every future user grants, and
// nothing about the app looks different afterwards — it is the one change that
// affects everybody and shows up nowhere.
//
// Reported by the user on 2026-08-30, having just signed in: the permissions
// requested read as "manage your profile, posts, likes and follows", "create,
// update, and delete any public data linked to your account", and "perform
// authenticated actions towards any service on your behalf". For a chess app.
// That is `transition:generic`, and the honest answer is that no narrower
// option exists yet — both eurosky.social and bsky.social advertise only
// ["atproto", "transition:email", "transition:generic", "transition:chat.bsky"]
// and `atproto` alone grants no repo write.
//
// So these tests do not assert that the scope is narrow. They assert it is no
// WIDER than the minimum that works, and that it stays that way.

// TestScopeRequestsNoMoreThanNeeded pins the two scopes that are on offer,
// unnecessary, and would each widen the consent screen.
func TestScopeRequestsNoMoreThanNeeded(t *testing.T) {
	for _, unwanted := range []string{"transition:email", "transition:chat.bsky"} {
		if strings.Contains(Scope, unwanted) {
			t.Errorf("Scope requests %q, which ATChess does not need.\n"+
				"It widens what every user grants at sign-in and nothing else about "+
				"the app changes, so it will not be noticed. Remove it.", unwanted)
		}
	}

	// atproto is required; without it the request is not an AT Protocol one.
	if !strings.Contains(Scope, "atproto") {
		t.Error("Scope is missing the base `atproto` scope")
	}

	// If this ever fails because the scope was narrowed, that is good news:
	// update it here and delete this check. It exists so that a WIDENING is
	// caught, not to freeze the value forever.
	if Scope != "atproto transition:generic" {
		t.Logf("Scope changed to %q. If this narrowed the request, update this "+
			"test and docs/. If it widened it, revert.", Scope)
	}
}

// TestScopeIsNotDuplicatedAsLiterals is the drift gate.
//
// The scope used to be three separate string literals: the client metadata we
// advertise at /client-metadata.json, the authorization request, and the token
// exchange. Those three disagreeing is a silent failure — an authorization
// server can issue a token for scopes that differ from what the consent screen
// showed, or refuse the exchange outright, and neither is visible from inside
// the app. One constant is the fix; this keeps it one.
func TestScopeIsNotDuplicatedAsLiterals(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	// Any transitional scope written as a literal, anywhere but the constant.
	literal := regexp.MustCompile(`"[^"]*transition:[a-z.]+[^"]*"`)

	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil // tests may name scopes to assert on them
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "const Scope") || strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if literal.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, rel+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("an OAuth scope is written as a literal in %d place(s) instead of using oauth.Scope:\n  %s\n\n"+
			"Three copies of this string is what the constant replaced. When they drift, the "+
			"consent screen and the issued token stop agreeing and nothing in the app notices.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// The client metadata served to authorization servers must advertise exactly
// the scope we then request. A server is entitled to refuse a request for
// anything the metadata did not declare.
func TestClientMetadataAdvertisesTheSameScope(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "internal", "web", "service.go"))
	if err != nil {
		t.Fatalf("reading service.go: %v", err)
	}
	src := string(b)

	// The metadata's scope field must be the constant, not a literal.
	if !regexp.MustCompile(`"scope":\s*oauth\.Scope`).MatchString(src) {
		t.Error("client-metadata.json's scope field does not use oauth.Scope.\n" +
			"What we advertise and what we request must be the same value, or an " +
			"authorization server may refuse the request for a scope we never declared.")
	}
}
