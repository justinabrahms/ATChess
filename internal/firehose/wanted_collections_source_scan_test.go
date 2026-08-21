package firehose

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestWantedCollectionsMatchesWrittenNSIDs is the "adding a lexicon without
// registering it" gate atchess-1c9.58 asks for. WantedCollections
// (client.go) is the CLOSED, server-side Jetstream filter: a collection
// missing from it is never delivered at all, not delivered late. The prior
// test (expectedWantedCollections in jetstream_test.go) only pins
// WantedCollections against an independent hardcoded literal, so it catches
// an accidental EDIT to WantedCollections but not an ADDITION elsewhere: a
// ninth app.atchess.* record type could be added to internal/atproto and
// nothing would go red, while that record type silently never reaches the
// firehose consumer in production.
//
// This test closes that gap by deriving, via a go/ast source scan of
// non-test *.go files in internal/atproto, the set of app.atchess.* NSIDs
// that are actually WRITTEN (i.e. appear as the literal "collection" value
// in a com.atproto.repo.createRecord/putRecord request body map), and
// asserting that set is exactly WantedCollections.
//
// Why go/ast instead of regex: a regex over the source text would also
// match the app.atchess.* NSIDs that appear in doc comments (e.g. the
// "Deliberately does NOT include app.atchess.challengeAcceptance..."
// comment on WantedCollections itself, in this package's own client.go) --
// go/ast's *ast.BasicLit walk only sees actual Go literals, comments are a
// separate node kind entirely, so that trap doesn't apply here.
//
// Why "collection" key with a bare string Value, specifically: every write
// call site in internal/atproto builds a `map[string]interface{}{"repo":
// ..., "collection": "app.atchess.X", ...}` body for createRecord/
// putRecord, where the collection Value is a direct *ast.BasicLit string.
// Every READ call site (getRecord/listRecords/deleteRecord) instead builds
// a url.Values{"collection": {"app.atchess.X"}, ...} -- because url.Values
// is map[string][]string, that Value is an *ast.CompositeLit (a one-element
// slice literal) wrapping the BasicLit, not a bare BasicLit. Requiring
// Value to be a *ast.BasicLit directly therefore selects write sites and
// skips read/dispatch sites structurally, without needing to special-case
// any particular read call. (Verified against every "app.atchess."
// occurrence in internal/atproto/client.go as of this writing: 12
// direct-string-Value literals across 8 distinct NSIDs -- some collections
// are written from more than one call site -- and no read site among them.
// The test dedupes into a set, so the 12-vs-8 difference does not matter
// to the comparison.)
//
// Note this map shape is a property of how the write sites happen to be
// written today, not a semantic definition of "a write". See KNOWN LIMITS.
//
// Known, deliberate exclusions -- do NOT add these to make the sets match:
//   - app.atchess.challengeAcceptance: never written anywhere in
//     internal/atproto; an "accept" is instead expressed via the
//     app.atchess.game record's optional "challenge" strongRef field. It
//     appears only in internal/firehose/client.go, as a doc comment and in
//     a strings.Contains read/dispatch branch -- neither is a write site,
//     and neither is in the internal/atproto tree this test scans anyway.
//   - app.atchess.gameIndex: an unused lexicon, written nowhere.
//
// KNOWN LIMITS of this gate (documented rather than overclaimed):
//   - It only scans internal/atproto. An NSID written from some OTHER
//     package (e.g. a future internal/web call straight to the PDS,
//     bypassing internal/atproto) would not be seen by this scan and would
//     NOT redden this test.
//   - It only matches a "collection" key whose value is a literal string.
//     An NSID assembled via concatenation, fmt.Sprintf, a const+suffix, or
//     any other non-literal expression would not be seen either.
//   - It only matches that ONE syntactic shape. A write whose collection
//     travels via a const or variable, a struct field (rather than a
//     map[string]interface{} entry), or a positional helper argument is
//     NOT seen. All four misses were confirmed empirically during
//     atchess-1c9.58's review; only the copy-an-existing-createX shape,
//     which is how all 12 current write sites and realistically any 9th
//     one are written, is caught.
//
// These gaps are structural (the source scan can only see what's
// syntactically a literal, in the one directory it's told to look at); the
// shared-registry alternative (a single exported set both the write path
// and WantedCollections derive from) would close them structurally instead
// of just detecting them, but was not chosen here -- see atchess-1c9.58's
// notes for why (risk of an internal/atproto <-> internal/firehose import
// cycle, and a larger surface change than this bead's scope).
func TestWantedCollectionsMatchesWrittenNSIDs(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	atprotoDir := filepath.Join(filepath.Dir(thisFile), "..", "atproto")

	entries, err := os.ReadDir(atprotoDir)
	if err != nil {
		t.Fatalf("failed to read %s: %v", atprotoDir, err)
	}

	written := map[string]bool{}
	fset := token.NewFileSet()
	scannedAnyFile := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scannedAnyFile = true

		path := filepath.Join(atprotoDir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			keyLit, ok := kv.Key.(*ast.BasicLit)
			if !ok || keyLit.Kind != token.STRING {
				return true
			}
			key, err := strconv.Unquote(keyLit.Value)
			if err != nil || key != "collection" {
				return true
			}
			// Require the Value to be a BARE string literal (not wrapped
			// in a url.Values-style []string{...} composite literal) --
			// this is what selects write-request-body sites over
			// read/list/delete call sites. See the doc comment above.
			valLit, ok := kv.Value.(*ast.BasicLit)
			if !ok || valLit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(valLit.Value)
			if err != nil || !strings.HasPrefix(val, "app.atchess.") {
				return true
			}
			written[val] = true
			return true
		})
	}
	if !scannedAnyFile {
		t.Fatalf("found no non-test .go files under %s -- test setup is broken", atprotoDir)
	}

	// Sanity check on the deliberate exclusions documented above: the scan
	// must not have picked these up. If it has, something about
	// internal/atproto changed (a genuine new write site) and these NSIDs
	// need real consideration, not silent inclusion here.
	for _, orphan := range []string{"app.atchess.challengeAcceptance", "app.atchess.gameIndex"} {
		if written[orphan] {
			t.Fatalf("source scan found %q written in internal/atproto, but it is documented here as a deliberate orphan (never written); update this test's exclusion list and its reasoning, don't just let the sets match silently", orphan)
		}
	}

	gotList := make([]string, 0, len(written))
	for nsid := range written {
		gotList = append(gotList, nsid)
	}
	sort.Strings(gotList)

	wantList := append([]string(nil), WantedCollections...)
	sort.Strings(wantList)

	if len(gotList) != len(wantList) {
		t.Fatalf("internal/atproto writes %d distinct app.atchess.* collections %v, but WantedCollections has %d entries %v -- a lexicon was added/removed on one side without updating the other", len(gotList), gotList, len(wantList), wantList)
	}
	for i := range gotList {
		if gotList[i] != wantList[i] {
			t.Fatalf("mismatch at index %d: internal/atproto writes %v, WantedCollections has %v", i, gotList, wantList)
		}
	}
}
