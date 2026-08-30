package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// UI ↔ API PARITY.
//
// THE BUG THIS EXISTS FOR, measured against the live deployment 2026-08-30:
//
//	https://atchess.abrah.ms/api/auth/session             -> 401  (exists)
//	https://atchess.abrah.ms/api/challenge-notifications  -> 401  (exists)
//	https://atchess.abrah.ms/api/challenges               -> 404  MISSING
//	https://atchess.abrah.ms/api/games                    -> 404  MISSING
//
// The shipped page has a "Create Game" button and an "Accept" button. Both
// call endpoints the shipped backend does not serve. You could log in and then
// do nothing at all — no game could be created, no challenge accepted — and
// every test in this repository passed the whole time.
//
// Nothing could have caught it. The Go suite tests handlers, never whether a
// handler is REGISTERED at the path the page fetches. The page itself is under
// web/static/, which docs/ORACLES.md marks supervised-only precisely because it
// has no oracle. So the seam between them — the one place a front end and a
// back end can disagree while both are internally consistent — was the one
// thing neither side tested.
//
// This test reads the two sources of truth and compares them: every `${API_BASE}/x`
// the page fetches must appear as a registered route in the protocol service.
//
// LIMITATION, stated rather than hidden: it compares SOURCE TEXT, not a live
// router. It cannot see a route registered by a variable, and it does not check
// HTTP methods — so it proves a path is served, not that it is served for the
// verb the page uses. The stronger version walks a real mux built by a shared
// RegisterRoutes(), which is a refactor of cmd/protocol/main.go and is filed
// separately. This weaker check catches the class of bug actually observed, and
// was written the day it was observed rather than after the refactor it wants.

// uiFetchPath matches the API paths the page fetches, e.g.
//
//	fetch(`${API_BASE}/challenge-notifications/${key}/accept`, ...)
var uiFetchPath = regexp.MustCompile(`\$\{API_BASE\}(/[A-Za-z0-9\-_/]*)`)

// routeRegistration matches a gorilla/mux registration, e.g.
//
//	authed.HandleFunc("/challenges", ...).Methods("POST")
var routeRegistration = regexp.MustCompile(`HandleFunc\(\s*"([^"]+)"`)

// normalize reduces a path to its shape so a template interpolation and a mux
// variable compare equal: /games/${id} and /games/{id} both become /games/*.
func normalize(p string) string {
	segs := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") || strings.Contains(s, "$") || s == "" && i > 0 {
			segs[i] = "*"
		}
	}
	return strings.Join(segs, "/")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/web -> repo root
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// uiPaths collects every API path any shipped page fetches.
func uiPaths(t *testing.T, root string) map[string][]string {
	t.Helper()
	pages, err := filepath.Glob(filepath.Join(root, "web", "static", "*.html"))
	if err != nil {
		t.Fatalf("globbing pages: %v", err)
	}
	if len(pages) == 0 {
		t.Fatal("no pages under web/static — this test would pass vacuously")
	}
	out := map[string][]string{}
	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("reading %s: %v", page, err)
		}
		for _, m := range uiFetchPath.FindAllStringSubmatch(string(b), -1) {
			p := normalize(m[1])
			if p == "" {
				continue
			}
			out[p] = append(out[p], filepath.Base(page))
		}
	}
	return out
}

// registeredPaths collects every path the protocol service registers.
func registeredPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "cmd", "protocol", "main.go"))
	if err != nil {
		t.Fatalf("reading the protocol service: %v", err)
	}
	out := map[string]bool{}
	for _, m := range routeRegistration.FindAllStringSubmatch(string(src), -1) {
		out[normalize(m[1])] = true
	}
	if len(out) == 0 {
		t.Fatal("found no registered routes — the scrape is broken, so this gate is vacuous")
	}
	return out
}

// TestEveryUIEndpointIsRegistered is the gate. A button that calls a route
// nobody serves is a dead button, and it is invisible to both sides' tests.
func TestEveryUIEndpointIsRegistered(t *testing.T) {
	root := repoRoot(t)
	ui := uiPaths(t, root)
	registered := registeredPaths(t, root)

	var missing []string
	for p, pages := range ui {
		if !registered[p] {
			missing = append(missing, p+"  (called by "+strings.Join(pages, ", ")+")")
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the page calls %d endpoint(s) the protocol service does not register:\n  %s\n\n"+
			"This is the exact shape of the defect measured on the live site on\n"+
			"2026-08-30: the UI's Create Game and Accept buttons called /api/games\n"+
			"and /api/challenges, both of which answered 404 in production, while\n"+
			"every test here passed. Register the route, or stop calling it.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// A gate that scrapes source is one refactor away from silently matching
// nothing and passing forever. These two assertions are what keep the test
// above from becoming decoration.
func TestParityScrapeIsNotVacuous(t *testing.T) {
	root := repoRoot(t)

	ui := uiPaths(t, root)
	if len(ui) < 3 {
		t.Errorf("only %d UI endpoint(s) found; the page fetches more than that, "+
			"so the scrape has stopped seeing them and the parity gate is now decoration", len(ui))
	}
	registered := registeredPaths(t, root)
	if len(registered) < 5 {
		t.Errorf("only %d registered route(s) found; the scrape is broken", len(registered))
	}

	// The endpoints that were actually broken in production. If the scrape ever
	// stops finding these specific ones, the gate has lost the bug it was
	// written for.
	for _, want := range []string{"/games", "/challenges", "/challenge-notifications"} {
		if _, ok := ui[want]; !ok {
			t.Errorf("the UI scrape no longer finds %q — that is one of the paths that 404'd in production", want)
		}
	}
}
