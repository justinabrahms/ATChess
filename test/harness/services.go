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
	label string
	url   string
	cmd   *exec.Cmd
	logs  *syncBuffer

	mu       sync.Mutex
	exited   bool
	exitErr  error
	exitedCh chan struct{}
}

// startProtocolService launches one protocol-service instance configured
// for account, listening on port, and blocks until it reports healthy via
// GET /api/health (bounded by healthTimeout) or fails.
//
// Configuration is passed entirely via environment variables that
// internal/config.Load binds ahead of its own config-file defaults
// (SERVER_PORT, ATPROTO_PDS_URL, ATPROTO_HANDLE, ATPROTO_PASSWORD,
// ATPROTO_USE_DPOP) -- this is what makes it possible to run two instances,
// one per PDS, from the same binary.
func startProtocolService(t *testing.T, binPath string, account Account, port int, label string) *serviceInstance {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d", port)

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
		"FIREHOSE_ENABLED=false",
	)

	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs

	inst := &serviceInstance{
		label:    label,
		url:      url,
		cmd:      cmd,
		logs:     logs,
		exitedCh: make(chan struct{}),
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
func StartServices(t *testing.T, accounts *Accounts) *Services {
	t.Helper()

	binPath := buildProtocolBinary(t)

	alicePort := freePort(t)
	bobPort := freePort(t)

	aliceInst := startProtocolService(t, binPath, accounts.Alice, alicePort, "alice")
	bobInst := startProtocolService(t, binPath, accounts.Bob, bobPort, "bob")

	return &Services{
		AliceURL: aliceInst.url,
		BobURL:   bobInst.url,
	}
}
