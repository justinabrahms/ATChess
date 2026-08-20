//go:build e2e

package harness

import (
	"os"
	"path/filepath"
)

// init sets SSL_CERT_FILE (if the dual-PDS CA bundle exists) for THIS test
// binary's own process, before any test runs. This matters separately from
// dualPDSCACertEnv in services.go: that one only covers the SPAWNED
// protocol-service subprocess's environment, but Player.RepoGetRecord/
// RepoListRecords (player.go) make direct-to-PDS HTTPS calls IN this
// process (deliberately -- see those functions' doc comments -- to read
// records independent of what the protocol service claims), using
// http.DefaultTransport, which lazily builds Go's root CA pool from
// SSL_CERT_FILE on first use and caches it for the life of the process. If
// the bundle does not exist yet (harness not brought up via `make
// test-federation-up[-ci]`), this is a silent no-op: LoadAccounts already
// gives a clear, actionable skip message in that case, and duplicating that
// check here (with no *testing.T available at init time to fail loudly
// against) would be redundant, not additive.
func init() {
	bundlePath := filepath.Join(repoRootDir(), dualPDSCABundleRelPath)
	if _, err := os.Stat(bundlePath); err == nil {
		os.Setenv("SSL_CERT_FILE", bundlePath)
	}
}
