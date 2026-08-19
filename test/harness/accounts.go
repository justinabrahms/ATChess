//go:build e2e

// Package harness provides a multi-identity test harness for exercising
// ATChess across two independent PDS instances, one per player. Unlike
// test/e2e (which drives both players through a single protocol-service
// instance backed by one config-supplied AT Protocol identity), this
// harness starts one protocol-service instance per player, each pointed at
// that player's own PDS, and talks to each player strictly through the
// public HTTP surface -- the same surface a real browser uses.
//
// It requires the CI-mode dual-PDS stack (`make test-federation-up-ci`) to
// be running; see LoadAccounts.
package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Account holds one player's credentials and identity as written by
// scripts/create-dual-pds-accounts.sh to test/.harness-accounts.json.
type Account struct {
	Handle   string `json:"handle"`
	DID      string `json:"did"`
	PDSURL   string `json:"pdsUrl"`
	Password string `json:"password"`
}

// Accounts holds both harness players' accounts.
type Accounts struct {
	Alice Account `json:"alice"`
	Bob   Account `json:"bob"`
}

// accountsFilePath returns the absolute path to test/.harness-accounts.json,
// resolved relative to this source file so it works regardless of the
// caller's working directory (go test runs with cwd set to the package
// directory, but callers outside this package -- or `go vet`/tooling -- may
// not).
func accountsFilePath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		// Fall back to a relative path; accountsFilePath is only used by
		// LoadAccounts, which will report a clear error either way.
		return filepath.Join("test", "harness", "..", ".harness-accounts.json")
	}
	// this file lives at <repo>/test/harness/accounts.go
	return filepath.Join(filepath.Dir(thisFile), "..", ".harness-accounts.json")
}

// LoadAccounts reads the dual-PDS harness account data written by
// scripts/create-dual-pds-accounts.sh (invoked via `make
// test-federation-up-ci`). If the file is absent, it skips the calling test
// with a message pointing at that command -- the stack simply isn't up,
// which is not a test failure.
func LoadAccounts(t *testing.T) *Accounts {
	t.Helper()

	path := accountsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("dual-PDS harness stack is not up (missing %s); run `make test-federation-up-ci` first", path)
		}
		t.Fatalf("failed to read harness accounts file %s: %v", path, err)
	}

	var accounts Accounts
	if err := json.Unmarshal(data, &accounts); err != nil {
		t.Fatalf("failed to parse harness accounts file %s: %v", path, err)
	}

	if accounts.Alice.DID == "" || accounts.Alice.PDSURL == "" || accounts.Alice.Handle == "" || accounts.Alice.Password == "" {
		t.Fatalf("harness accounts file %s has an incomplete alice entry: %+v", path, accounts.Alice)
	}
	if accounts.Bob.DID == "" || accounts.Bob.PDSURL == "" || accounts.Bob.Handle == "" || accounts.Bob.Password == "" {
		t.Fatalf("harness accounts file %s has an incomplete bob entry: %+v", path, accounts.Bob)
	}

	return &accounts
}
