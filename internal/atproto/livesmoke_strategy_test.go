//go:build livesmoke

package atproto

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestLiveSmokeWhichStrategy is a diagnostic companion to TestLiveSmoke: it
// calls each ResolveHandle strategy directly to report which one actually
// succeeds against real bsky.social handles (ResolveHandle itself only
// reports the final winner, not which layer it came from).
func TestLiveSmokeWhichStrategy(t *testing.T) {
	accounts := loadLiveAccounts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpClient := &http.Client{Timeout: 15 * time.Second}
	bootstrap := &Client{pdsURL: "https://bsky.social", httpClient: httpClient}

	for _, h := range []string{accounts.Alice.Handle, accounts.Bob.Handle} {
		if did, err := bootstrap.resolveHandleSamePDS(ctx, h); err == nil {
			t.Logf("%s: same-PDS(bsky.social) SUCCEEDED did=%s", h, did)
		} else {
			t.Logf("%s: same-PDS(bsky.social) failed: %v", h, err)
		}
		if did, err := resolveHandleViaDNS(ctx, h); err == nil {
			t.Logf("%s: DNS TXT SUCCEEDED did=%s", h, did)
		} else {
			t.Logf("%s: DNS TXT failed: %v", h, err)
		}
		if did, err := resolveHandleViaWellKnown(ctx, httpClient, h); err == nil {
			t.Logf("%s: HTTPS well-known SUCCEEDED did=%s", h, did)
		} else {
			t.Logf("%s: HTTPS well-known failed: %v", h, err)
		}
	}
}
