package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStaticFiles_ReferenceDerivationIncomplete is a WEAK guard test, not a
// behavioural one. There is no JS test harness in this repo (no
// package.json / jest / vitest / etc.) and this task deliberately does not
// add one. All this test does is confirm the literal string
// "derivationIncomplete" still appears in web/static/index.html and
// web/static/spectator.html -- i.e. that something in each page still
// reads the flag atchess-1c9.53 put on the wire and atchess-1c9.66 wired
// into the UI. It proves the reference wasn't silently deleted; it proves
// NOTHING about correctness (whether the banner is shown/hidden at the
// right time, whether it's legible, whether the absent-flag case degrades
// sanely, etc.). Do not mistake a pass here for behavioural coverage.
func TestStaticFiles_ReferenceDerivationIncomplete(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's own path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	for _, rel := range []string{
		filepath.Join("web", "static", "index.html"),
		filepath.Join("web", "static", "spectator.html"),
	} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		if !strings.Contains(string(data), "derivationIncomplete") {
			t.Errorf("%s no longer references \"derivationIncomplete\" -- the atchess-1c9.66 unverified-status wiring appears to have been removed", rel)
		}
	}
}
