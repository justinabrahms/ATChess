//go:build e2e

package harness

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Services holds the base URLs of the two protocol-service instances
// started by StartServices, one per player, each pointed at that player's
// own PDS. Each player must talk to their own instance: internal/web's
// LoginHandler authenticates against a single, config-supplied PDS URL, so
// one instance cannot serve both alice (on PDS-A) and bob (on PDS-B).
type Services struct {
	AliceURL string
	BobURL   string

	// AliceStateDir/BobStateDir are each instance's own FIREHOSE_STATE_DIR
	// (atchess-1c9.46 review fix): each protocol-service instance gets a
	// distinct, per-instance state directory rather than the package
	// default (./data/firehose relative to the repo root, shared by every
	// instance started from this repo). Without that, two instances
	// sharing one cursors.json clobber each other -- a short-lived
	// instance's periodic flush of "no cursor yet" (-1, which
	// CursorStore.Store treats as a delete) for a host can erase a cursor
	// a DIFFERENT, still-running instance legitimately persisted for that
	// same host key. See startProtocolService.
	AliceStateDir string
	BobStateDir   string

	// AliceChallengeDBPath/BobChallengeDBPath are each instance's own
	// CHALLENGE_DB_PATH (atchess-1c9.76/atchess-1c9.78): each
	// protocol-service instance gets a distinct, per-instance SQLite file
	// rather than the package default (./data/challenges.db relative to
	// the repo root, shared by every instance started from this repo).
	// Without that, one instance can observe challenge rows a DIFFERENT
	// instance's own HTTP handler inserted directly into the shared file,
	// which silently substitutes for -- and thereby masks the absence of
	// -- real cross-instance delivery via the firehose/backfill. Exposed
	// here, mirroring AliceStateDir/BobStateDir above, so tests can assert
	// the two paths actually differ and are not the same underlying file
	// (see startProtocolService).
	AliceChallengeDBPath string
	BobChallengeDBPath   string
}

// syncBuffer is a mutex-protected byte buffer used to capture a
// subprocess's combined stdout+stderr so it can be surfaced in a test
// failure message instead of vanishing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// repoRootDir resolves the repository root relative to this source file, so
// it works regardless of the test binary's working directory.
func repoRootDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// this file lives at <repo>/test/harness/services.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// freePort asks the OS for an ephemeral port by briefly binding to :0, then
// releases it immediately so the protocol-service subprocess can bind it.
// There is an inherent (small) TOCTOU race here; acceptable for a test
// harness.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// buildProtocolBinary compiles cmd/protocol once into a temp directory and
// returns the resulting binary path. Building once and starting it twice
// (once per player) is both faster and more representative of production
// than re-running `go run` for each instance.
func buildProtocolBinary(t *testing.T) string {
	t.Helper()
	root := repoRootDir()
	binPath := filepath.Join(t.TempDir(), "atchess-protocol-harness")

	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/protocol")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build protocol service binary from %s: %v\n%s", root, err, string(out))
	}
	return binPath
}

// serviceInstance tracks one running protocol-service subprocess.
type serviceInstance struct {
	label           string
	url             string
	stateDir        string
	challengeDBPath string
	cmd             *exec.Cmd
	logs            *syncBuffer

	mu       sync.Mutex
	exited   bool
	exitErr  error
	exitedCh chan struct{}
}

// localPLCDefaultPort is the host port the CI-mode dual-PDS stack
// (docker-compose.dual-pds.yml's local-plc service, profile "ci") publishes
// its hermetic did:plc directory on, matching that file's
// `${PLC_PORT:-2582}:2582` port mapping. Kept in one place, next to the
// other harness constants, and overridable via the same PLC_PORT env var
// used there (see resolvePLCDirectoryURL) so changing one changes both.
const localPLCDefaultPort = "2582"

// localPLCHealthTimeout bounds the one-shot probe resolvePLCDirectoryURL
// makes to detect whether a CI-mode local-plc container is up.
const localPLCHealthTimeout = 2 * time.Second

// dualPDSCABundleRelPath is where scripts/extract-dual-pds-ca.sh (invoked
// by `make test-federation-up`/`test-federation-up-ci`) writes the combined
// system-CA-bundle + dual-PDS-proxy-local-CA bundle used to verify
// alice.pds.test/bob.pds.test's TLS certificates (see
// docker-compose.dual-pds.yml's CERTIFICATES comment for the full
// derivation, and that script for why a COMBINED bundle -- not just the bare
// CA -- is required for Go's SSL_CERT_FILE, which replaces rather than adds
// to the default trust store).
const dualPDSCABundleRelPath = "certs/dual-pds/ca-bundle.pem"

// dualPDSCACertEnv returns the SSL_CERT_FILE override needed for the
// spawned protocol-service subprocess to do real TLS verification against
// pdsURL, if pdsURL is https. Fails the test loudly (rather than silently
// falling through to the system default trust store, which would produce a
// confusing "certificate signed by unknown authority" failure much later,
// deep inside a move/challenge call) if pdsURL is https but the CA bundle
// scripts/extract-dual-pds-ca.sh produces has not been generated yet.
func dualPDSCACertEnv(t *testing.T, pdsURL string) []string {
	t.Helper()

	if !strings.HasPrefix(pdsURL, "https://") {
		return nil
	}

	bundlePath := filepath.Join(repoRootDir(), dualPDSCABundleRelPath)
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("protocol-service instance needs SSL_CERT_FILE to verify %s over TLS, but the CA bundle %s does not exist (%v). Run scripts/extract-dual-pds-ca.sh (or 'make test-federation-up'/'test-federation-up-ci', which runs it for you) before this test.", pdsURL, bundlePath, err)
	}

	return []string{"SSL_CERT_FILE=" + bundlePath}
}

// resolvePLCDirectoryURL determines which PLC directory the harness's
// spawned protocol-service instances should be told to resolve did:plc
// identities against (ATPROTO_PLC_DIRECTORY_URL; see internal/config and
// internal/atproto.identityResolver), so the documented
// `make test-federation-up-ci` -> `go test -tags=e2e -run TestFederation`
// sequence is green with no manual env var (atchess-1c9.10 review gap 1).
//
// Resolution order:
//  1. ATCHESS_TEST_PLC_URL, if set, is used verbatim -- a full manual
//     override for anyone running the stack on a non-default port.
//  2. Otherwise, derive http://localhost:<PLC_PORT> (PLC_PORT env var,
//     defaulting to localPLCDefaultPort -- the same variable and default
//     docker-compose.dual-pds.yml's local-plc service publishes on) and
//     probe GET <url>/_health. If it answers healthy, this is CI mode and
//     that URL is returned.
//  3. If the probe fails (no local-plc container running), this is LOCAL
//     ad-hoc mode (`make test-federation-up`), whose PDS containers are
//     themselves configured to use the real public https://plc.directory
//     (see docker-compose.dual-pds.yml). "" is returned so the spawned
//     instances fall back to that same real directory, matching the mode
//     they were actually brought up in, rather than forcing CI routing onto
//     a stack that isn't running local-plc at all.
func resolvePLCDirectoryURL(t *testing.T) string {
	t.Helper()

	if override := os.Getenv("ATCHESS_TEST_PLC_URL"); override != "" {
		return override
	}

	port := os.Getenv("PLC_PORT")
	if port == "" {
		port = localPLCDefaultPort
	}
	candidate := fmt.Sprintf("http://localhost:%s", port)

	client := &http.Client{Timeout: localPLCHealthTimeout}
	resp, err := client.Get(candidate + "/_health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return candidate
}

// pdsSubscribeReposURL converts a PDS's HTTP(S) base URL into its
// com.atproto.sync.subscribeRepos websocket URL (https -> wss, http -> ws;
// every AT Protocol PDS -- not just a relay -- serves this endpoint
// directly for its own repos). Used to build FIREHOSE_URL for
// startProtocolService: see StartServices' doc comment for why the harness
// points every instance at BOTH PDSes directly instead of a relay.
func pdsSubscribeReposURL(pdsURL string) string {
	ws := pdsURL
	switch {
	case strings.HasPrefix(ws, "https://"):
		ws = "wss://" + strings.TrimPrefix(ws, "https://")
	case strings.HasPrefix(ws, "http://"):
		ws = "ws://" + strings.TrimPrefix(ws, "http://")
	}
	return strings.TrimRight(ws, "/") + "/xrpc/com.atproto.sync.subscribeRepos"
}

// startProtocolService launches one protocol-service instance configured
// for account, listening on port, and blocks until it reports healthy via
// GET /api/health (bounded by healthTimeout) or fails.
//
// Configuration is passed entirely via environment variables that
// internal/config.Load binds ahead of its own config-file defaults
// (SERVER_PORT, ATPROTO_PDS_URL, ATPROTO_HANDLE, ATPROTO_PASSWORD,
// ATPROTO_USE_DPOP, and -- when a CI-mode local-plc directory was detected,
// see resolvePLCDirectoryURL -- ATPROTO_PLC_DIRECTORY_URL) -- this is what
// makes it possible to run two instances, one per PDS, from the same
// binary.
//
// firehoseURLs is a comma-separated list of com.atproto.sync.subscribeRepos
// endpoints this instance should subscribe to for challenge delivery
// (atchess-1c9.11) -- see StartServices for why it always names BOTH
// players' PDSes, not just this instance's own.
func startProtocolService(t *testing.T, binPath string, account Account, port int, label string, plcDirectoryURL string, firehoseURLs string) *serviceInstance {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	// FIREHOSE_STATE_DIR (atchess-1c9.46 review fix): must be per-instance.
	// t.TempDir() returns a fresh, unique directory on every call (even
	// repeated calls against the same t), so alice's instance, bob's
	// instance, and any additional instance started later in the same test
	// (e.g. the OfflineBackfill subtest's second bob instance) each get
	// their own cursors.json rather than sharing (and clobbering) the
	// package-default ./data/firehose under the repo root -- see Services'
	// doc comment for what that sharing broke.
	stateDir := filepath.Join(t.TempDir(), "firehose-state")

	// CHALLENGE_DB_PATH (atchess-1c9.76): must be per-instance, for exactly
	// the same reason as FIREHOSE_STATE_DIR above. internal/config.Load
	// binds CHALLENGE_DB_PATH ahead of its own package default
	// (./data/challenges.db, relative to the repo root -- see
	// internal/challenge.NewStore's caller, cmd/protocol/main.go). Without
	// a per-instance override here, every protocol-service instance this
	// harness starts -- from every test in the package, across every run --
	// opens that ONE shared SQLite file (cmd.Dir is the repo root, so a
	// relative default resolves there), which lets one instance observe
	// challenge rows a DIFFERENT instance's own HTTP handler inserted
	// directly, without any firehose delivery or backfill-on-login ever
	// occurring. That is not cross-instance delivery; it is two processes
	// sharing one database file. t.TempDir() gives an absolute path, so
	// this cannot silently re-resolve against cmd.Dir the way a relative
	// path would.
	challengeDBPath := filepath.Join(t.TempDir(), "challenge-state", "challenges.db")
	if err := os.MkdirAll(filepath.Dir(challengeDBPath), 0o755); err != nil {
		t.Fatalf("failed to create per-instance challenge DB directory %s: %v", filepath.Dir(challengeDBPath), err)
	}

	cmd := exec.Command(binPath)
	cmd.Dir = repoRootDir()
	cmd.Env = append(os.Environ(),
		"SERVER_HOST=127.0.0.1",
		fmt.Sprintf("SERVER_PORT=%d", port),
		"SERVER_BASE_URL=",
		"ATPROTO_PDS_URL="+account.PDSURL,
		"ATPROTO_HANDLE="+account.Handle,
		"ATPROTO_PASSWORD="+account.Password,
		"ATPROTO_USE_DPOP=false",
		"FIREHOSE_ENABLED=true",
		"FIREHOSE_URL="+firehoseURLs,
		"FIREHOSE_STATE_DIR="+stateDir,
		"CHALLENGE_DB_PATH="+challengeDBPath,
	)
	if plcDirectoryURL != "" {
		cmd.Env = append(cmd.Env, "ATPROTO_PLC_DIRECTORY_URL="+plcDirectoryURL)
	}
	cmd.Env = append(cmd.Env, dualPDSCACertEnv(t, account.PDSURL)...)

	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs

	inst := &serviceInstance{
		label:           label,
		url:             url,
		stateDir:        stateDir,
		challengeDBPath: challengeDBPath,
		cmd:             cmd,
		logs:            logs,
		exitedCh:        make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start protocol service for %s (%s): %v", label, account.Handle, err)
	}

	go func() {
		err := cmd.Wait()
		inst.mu.Lock()
		inst.exited = true
		inst.exitErr = err
		inst.mu.Unlock()
		close(inst.exitedCh)
	}()

	t.Cleanup(func() {
		inst.mu.Lock()
		alreadyExited := inst.exited
		inst.mu.Unlock()
		if !alreadyExited && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-inst.exitedCh:
		case <-time.After(5 * time.Second):
			t.Logf("protocol service for %s did not exit within 5s of being killed", label)
		}
	})

	waitForHealthy(t, inst, 30*time.Second)

	return inst
}

// waitForHealthy polls GET {inst.url}/api/health until it returns HTTP 200
// or deadline elapses. If the process dies before becoming healthy, it
// fails immediately (rather than waiting out the full deadline) and
// surfaces the captured stdout/stderr -- an opaque timeout here would waste
// days on the downstream conformance beads this harness exists to unblock.
func waitForHealthy(t *testing.T, inst *serviceInstance, timeout time.Duration) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		inst.mu.Lock()
		exited := inst.exited
		exitErr := inst.exitErr
		inst.mu.Unlock()
		if exited {
			t.Fatalf("protocol service for %s exited before becoming healthy (%v); output:\n%s",
				inst.label, exitErr, inst.logs.String())
		}

		resp, err := client.Get(inst.url + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		select {
		case <-inst.exitedCh:
			// Process died between our exited-check above and this poll;
			// loop again immediately to hit the exited branch and report
			// with logs rather than falling through to the timeout below.
			continue
		case <-time.After(200 * time.Millisecond):
		}
	}

	t.Fatalf("protocol service for %s at %s did not become healthy within %s (last error: %v); output:\n%s",
		inst.label, inst.url, timeout, lastErr, inst.logs.String())
}

// StartServices builds the protocol-service binary once and starts one
// instance per player, each configured against that player's own PDS and
// listening on its own dynamically-allocated port. Both instances are
// health-gated (GET /api/health) before StartServices returns, and are
// killed and reaped via t.Cleanup at test end.
//
// Both instances are also given FIREHOSE_URL naming BOTH alice's and bob's
// PDS com.atproto.sync.subscribeRepos endpoints (atchess-1c9.11): a
// challenge record only ever lives in its CHALLENGER's own repo, so the
// challenged player's instance can only discover it by watching whichever
// PDS the challenger happens to be on -- and in this two-player harness
// either player might challenge the other. There is no relay/Jetstream
// aggregator in this harness to watch instead (see the package doc comment
// on internal/firehose and atchess-1c9.11's own notes for why); a real
// deployment would watch a network-wide aggregator rather than an explicit,
// closed list of known PDSes.
func StartServices(t *testing.T, accounts *Accounts) *Services {
	t.Helper()

	binPath := buildProtocolBinary(t)
	plcDirectoryURL := resolvePLCDirectoryURL(t)
	firehoseURLs := AllPDSFirehoseURLs(accounts)

	aliceURL, aliceStateDir, aliceChallengeDBPath := StartPlayerService(t, binPath, accounts.Alice, "alice", plcDirectoryURL, firehoseURLs)
	bobURL, bobStateDir, bobChallengeDBPath := StartPlayerService(t, binPath, accounts.Bob, "bob", plcDirectoryURL, firehoseURLs)

	return &Services{
		AliceURL:             aliceURL,
		BobURL:               bobURL,
		AliceStateDir:        aliceStateDir,
		BobStateDir:          bobStateDir,
		AliceChallengeDBPath: aliceChallengeDBPath,
		BobChallengeDBPath:   bobChallengeDBPath,
	}
}

// AllPDSFirehoseURLs returns the comma-separated FIREHOSE_URL value naming
// every account's PDS com.atproto.sync.subscribeRepos endpoint (see
// StartServices' doc comment for why every instance needs to watch every
// known PDS in this relay-less harness).
func AllPDSFirehoseURLs(accounts *Accounts) string {
	return strings.Join([]string{
		pdsSubscribeReposURL(accounts.Alice.PDSURL),
		pdsSubscribeReposURL(accounts.Bob.PDSURL),
	}, ",")
}

// BuildProtocolBinary compiles cmd/protocol once and returns the resulting
// binary path, for callers (e.g. an offline-backfill test that needs to
// start one player's instance well after the other's, rather than both
// together via StartServices) that need to start protocol-service instances
// individually and staggered in time. Safe to call multiple times per test;
// each call performs its own build into a fresh t.TempDir().
func BuildProtocolBinary(t *testing.T) string {
	t.Helper()
	return buildProtocolBinary(t)
}

// ResolvePLCDirectoryURL exposes resolvePLCDirectoryURL for callers that
// need to start protocol-service instances individually via
// StartPlayerService rather than together via StartServices.
func ResolvePLCDirectoryURL(t *testing.T) string {
	t.Helper()
	return resolvePLCDirectoryURL(t)
}

// StartPlayerService starts a single protocol-service instance for account
// (using binPath, built by BuildProtocolBinary/buildProtocolBinary),
// listening on a freshly allocated port, configured to watch
// firehoseURLs (see AllPDSFirehoseURLs) for challenge delivery. It blocks
// until the instance reports healthy and returns its base URL and its own
// (per-instance, see startProtocolService) FIREHOSE_STATE_DIR and
// CHALLENGE_DB_PATH. Exported (unlike the underlying startProtocolService)
// so tests that need to start one player's instance independently of the
// other -- notably an offline-backfill test, where one player's instance
// must NOT be running yet when the other player issues a challenge -- can
// do so directly, rather than only via StartServices' both-at-once
// topology.
func StartPlayerService(t *testing.T, binPath string, account Account, label string, plcDirectoryURL string, firehoseURLs string) (url string, stateDir string, challengeDBPath string) {
	t.Helper()
	port := freePort(t)
	inst := startProtocolService(t, binPath, account, port, label, plcDirectoryURL, firehoseURLs)
	return inst.url, inst.stateDir, inst.challengeDBPath
}
