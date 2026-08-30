package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// uiFetchPath matches the API paths the page requests, in either form:
//
//	fetch(`${API_BASE}/challenge-notifications/${key}/accept`, ...)   (legacy)
//	apiFetch(`/challenge-notifications/${key}/accept`, ...)           (current)
//
// BOTH ARE MATCHED DELIBERATELY. Introducing apiFetch removed `${API_BASE}`
// from every call site, so a scrape that only knew the first form found one
// endpoint instead of nine and this file quietly stopped checking anything.
// TestParityScrapeIsNotVacuous caught that on the pre-push hook, which is the
// entire reason it exists — a gate that scrapes source is one refactor away
// from passing forever.
var uiFetchPath = regexp.MustCompile(
	`(?:\$\{API_BASE\}|apiFetch\(\s*` + "`" + `)(/[A-Za-z0-9\-_/]*)`)

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

// EVERY API CALL MUST GO THROUGH apiFetch.
//
// Measured 2026-08-30, and the reason this exists. The authenticated endpoints
// require an X-Session-ID header (auth_middleware.go). Nine fetch calls were
// written by hand and exactly two remembered it. So /auth/session and
// /auth/logout worked, and creating a game, making a move, listing challenges,
// accepting one, declining one and loading a game were all answered 401.
//
// The failure mode is the nasty one: the app looked signed in and fully
// functional -- correct handle in the corner, board rendered, "Ready to play"
// -- while nothing a player actually does could succeed. And each call site
// threw its own generic string on a non-2xx, discarding the server's message,
// so the only symptom was "Failed to create challenge" with no detail.
//
// One helper attaches the header and surfaces the server's error. This test is
// what keeps the tenth call site from being written by hand again.
func TestUIUsesApiFetchForEveryAPICall(t *testing.T) {
	root := repoRoot(t)
	pages, err := filepath.Glob(filepath.Join(root, "web", "static", "*.html"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("no pages under web/static (err=%v) — this test would pass vacuously", err)
	}

	// A raw fetch() whose URL is built from API_BASE, i.e. bypassing apiFetch.
	direct := regexp.MustCompile(`[^a-zA-Z]fetch\(\s*` + "`" + `\$\{API_BASE\}`)

	var offenders []string
	sawHelper := false
	for _, page := range pages {
		b, rerr := os.ReadFile(page)
		if rerr != nil {
			t.Fatalf("reading %s: %v", page, rerr)
		}
		src := string(b)
		if strings.Contains(src, "async function apiFetch") {
			sawHelper = true
		}
		// window.fetch inside the helper itself is the one legitimate raw call.
		src = strings.ReplaceAll(src, "window.fetch(`${API_BASE}${path}`", "")
		if direct.MatchString(src) {
			offenders = append(offenders, filepath.Base(page))
		}
	}

	if !sawHelper {
		t.Error("no apiFetch helper found in web/static — the session header and the " +
			"server's error message are then each call site's problem to remember, " +
			"which is how every game action ended up unauthenticated")
	}
	if len(offenders) > 0 {
		t.Errorf("page(s) call the API directly instead of through apiFetch: %s\n\n"+
			"A direct fetch sends no X-Session-ID, so the endpoint answers 401, and the "+
			"call site invents its own error message instead of showing the server's.",
			strings.Join(offenders, ", "))
	}
}

// THE UI MUST NOT USE alert() / confirm() / prompt().
//
// There were nine alert() calls. They block the entire page until dismissed,
// cannot show two things at once, cannot be styled, and on a rejected move
// interrupt the player mid-thought to say something that belonged in the
// corner. Two of them said "not yet implemented" behind live buttons.
//
// notify() and confirmAction() replace them. This gate exists because alert()
// is the single easiest thing to reach for when adding a message, and one of
// them undoes the whole point.
func TestUIDoesNotUseBlockingDialogs(t *testing.T) {
	root := repoRoot(t)
	pages, err := filepath.Glob(filepath.Join(root, "web", "static", "*.html"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("no pages under web/static (err=%v) — this test would pass vacuously", err)
	}

	// Word-boundary-ish: `alert(` but not `.alert(` or `notifyAlert(`.
	blocking := regexp.MustCompile(`(^|[^.\w])(alert|confirm|prompt)\s*\(`)

	var offenders []string
	for _, page := range pages {
		b, rerr := os.ReadFile(page)
		if rerr != nil {
			t.Fatalf("reading %s: %v", page, rerr)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments explain why these are banned; they are not uses.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "/*") {
				continue
			}
			if blocking.MatchString(line) {
				offenders = append(offenders,
					filepath.Base(page)+":"+strconv.Itoa(i+1)+"  "+trimmed)
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("the UI uses a blocking dialog in %d place(s):\n  %s\n\n"+
			"Use notify(message, 'error'|'success'|'info') instead, or "+
			"confirmAction(message, label) for something irreversible. "+
			"alert() blocks the page, cannot be styled, and cannot show two things at once.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// The page must not ship buttons wired to "not yet implemented". Two did —
// Offer Draw and Resign — while their endpoints had existed all along. A button
// that does nothing is worse than a missing one: it promises a feature and then
// wastes the player's move.
func TestUIHasNoNotImplementedActions(t *testing.T) {
	root := repoRoot(t)
	pages, _ := filepath.Glob(filepath.Join(root, "web", "static", "*.html"))
	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(b))
		for _, phrase := range []string{"not yet implemented", "todo: implement"} {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s still contains %q — either build it or remove the control that offers it",
					filepath.Base(page), phrase)
			}
		}
	}
}

// A LIST MUST NOT ASSERT EMPTINESS BEFORE IT HAS LOADED.
//
// The three sidebar lists shipped with "No active games" / "No pending
// challenges" / "No challenges sent" hardcoded in the markup. That is not a
// placeholder, it is a wrong answer: the games list derives every row (several
// repo scans each), so a player watched it say they had no games for as long as
// the fetch took. Reported 2026-08-30 — "shows empty until it loads".
//
// It is the same mistake as rendering a turn from a stale record, and the same
// rule fixes both: when you do not know yet, say so.
func TestListsShowLoadingNotEmptinessBeforeTheyLoad(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "web", "static", "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	src := string(b)

	// For each async list, the markup between its container and the next
	// closing div must be a loading state, not an empty-state claim.
	for _, id := range []string{"gamesList", "challengesList", "outgoingChallenges"} {
		i := strings.Index(src, `id="`+id+`"`)
		if i < 0 {
			t.Errorf("no element with id %q — the list was renamed and this gate stopped checking it", id)
			continue
		}
		end := strings.Index(src[i:], "</div>")
		if end < 0 {
			t.Errorf("%s: could not find the end of the container", id)
			continue
		}
		initial := src[i : i+end]
		if strings.Contains(initial, "no-items") {
			t.Errorf("%s asserts an empty state in static markup:\n  %s\n\n"+
				"It renders before any fetch completes, so it tells the player "+
				"they have nothing when the truth is that we have not looked yet. "+
				"Use the loading-row/spinner markup and let the loader replace it.",
				id, strings.TrimSpace(initial))
		}
		if !strings.Contains(initial, "loading-row") {
			t.Errorf("%s has no loading state; a slow list looks like an empty one", id)
		}
	}

	// And every loader must actually put one up, or the markup's loading state
	// is only correct until the first refresh.
	for _, fn := range []string{"loadActiveGames", "loadChallenges", "loadOutgoingChallenges"} {
		j := strings.Index(src, "async function "+fn+"(")
		if j < 0 {
			t.Errorf("loader %s not found; this gate has drifted from the code", fn)
			continue
		}
		window := src[j:min(j+400, len(src))]
		if !strings.Contains(window, "showLoading") {
			t.Errorf("%s does not show a loading state, so a refresh leaves the "+
				"previous contents (or an empty state) up while it fetches", fn)
		}
	}
}
