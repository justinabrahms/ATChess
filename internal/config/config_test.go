package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempConfigDir chdirs the test into a fresh temp directory containing
// a minimal config.yaml, restoring the original working directory on
// cleanup. Load() only honours environment variable overrides (via
// viper.AutomaticEnv/BindEnv) on the path where viper.ReadInConfig()
// actually finds a config file -- if it returns ConfigFileNotFoundError,
// Load() short-circuits to loadDefaults() and env vars are never
// consulted. So these tests need a real (if otherwise empty) config.yaml
// on the working directory viper searches, not just an env var set from an
// arbitrary cwd.
func withTempConfigDir(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server:\n  host: localhost\n"), 0o644); err != nil {
		t.Fatalf("failed to write temp config.yaml: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("os.Chdir(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatalf("os.Chdir(%q) on cleanup: %v", origWD, err)
		}
	})
}

// TestLoad_ChallengePruneInterval_RejectsNonPositive is the regression test
// for atchess-1c9.62: CHALLENGE_PRUNE_INTERVAL values that parse fine as a
// time.Duration but are unusable as a ticker interval (zero, or negative)
// must be rejected by Load() with a clear error naming
// 'challenge.prune_interval', rather than reaching
// cmd/protocol/main.go's pruneChallengesPeriodically and panicking inside
// time.NewTicker.
func TestLoad_ChallengePruneInterval_RejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0s", "-5m"} {
		t.Run(raw, func(t *testing.T) {
			withTempConfigDir(t)
			t.Setenv("CHALLENGE_PRUNE_INTERVAL", raw)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with CHALLENGE_PRUNE_INTERVAL=%q returned no error; want an error naming challenge.prune_interval", raw)
			}
			if !strings.Contains(err.Error(), "challenge.prune_interval") {
				t.Fatalf("Load() error = %q; want it to name challenge.prune_interval", err.Error())
			}
		})
	}
}

// TestLoad_ChallengePruneInterval_ValidIntervalHonored is the control for
// TestLoad_ChallengePruneInterval_RejectsNonPositive: it proves the fix is
// a guard against non-positive values specifically, not a clamp that
// silently discards every configured value (which would make the
// rejection test above pass for the wrong reason).
func TestLoad_ChallengePruneInterval_ValidIntervalHonored(t *testing.T) {
	withTempConfigDir(t)
	t.Setenv("CHALLENGE_PRUNE_INTERVAL", "30m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with CHALLENGE_PRUNE_INTERVAL=30m returned unexpected error: %v", err)
	}
	if cfg.Challenge.PruneInterval != 30*time.Minute {
		t.Fatalf("cfg.Challenge.PruneInterval = %v; want 30m", cfg.Challenge.PruneInterval)
	}
}
