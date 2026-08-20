package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// withTempConfigDir chdirs the test into a fresh temp directory containing
// a minimal config.yaml, restoring the original working directory on
// cleanup. Also resets global viper state (see resetViper) so accumulated
// config paths/bindings from a prior test in the same process cannot leak
// into this one.
func withTempConfigDir(t *testing.T) {
	t.Helper()
	resetViper(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server:\n  host: localhost\n"), 0o644); err != nil {
		t.Fatalf("failed to write temp config.yaml: %v", err)
	}

	chdir(t, dir)
}

// withNoConfigDir chdirs the test into a fresh, empty temp directory (no
// config.yaml/.yml anywhere viper would look), restoring the original
// working directory on cleanup, and resets global viper state so
// accumulated config paths/bindings from a prior test cannot leak in (in
// particular, so a config.yaml written by a previous withTempConfigDir
// call -- whose temp dir may not even be removed yet if that test hasn't
// finished cleanup -- is never found via a stale AddConfigPath entry).
func withNoConfigDir(t *testing.T) {
	t.Helper()
	resetViper(t)

	dir := t.TempDir()
	chdir(t, dir)
}

// resetViper clears all global viper state (config paths, bound env vars,
// defaults, the previously read config). Load() uses the package-level
// default viper instance, so without this, config paths and bindings
// registered by one test's Load() call accumulate onto the next test's
// call within the same test binary -- go test -shuffle=on would otherwise
// be able to produce order-dependent failures.
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

func chdir(t *testing.T, dir string) {
	t.Helper()

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

// TestLoad_NoConfigFile_DefaultsControl is the control for the
// TestLoad_NoConfigFile_* tests below (atchess-1c9.64): with no config
// file AND no relevant env vars set, Load() must still return the
// documented defaults. Without this control, an implementation that
// simply always reads env vars (ignoring viper.SetDefault) could pass the
// env-var-honoured tests for the wrong reason.
func TestLoad_NoConfigFile_DefaultsControl(t *testing.T) {
	withNoConfigDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with no config file and no env vars returned unexpected error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("cfg.Server.Port = %d; want 8080 (default)", cfg.Server.Port)
	}
	if cfg.Firehose.Enabled != false {
		t.Errorf("cfg.Firehose.Enabled = %v; want false (default)", cfg.Firehose.Enabled)
	}
	if cfg.ATProto.PDSURL != "http://localhost:3000" {
		t.Errorf("cfg.ATProto.PDSURL = %q; want %q (default)", cfg.ATProto.PDSURL, "http://localhost:3000")
	}
	if cfg.Challenge.PruneInterval != time.Hour {
		t.Errorf("cfg.Challenge.PruneInterval = %v; want 1h (default)", cfg.Challenge.PruneInterval)
	}
}

// TestLoad_NoConfigFile_EnvVarsHonored is the regression test for
// atchess-1c9.64: Load() previously short-circuited to a hardcoded
// loadDefaults() whenever no config.yaml/.yml was found, which consulted
// no viper state -- so every env var binding was silently inert on that
// path. This asserts env vars of several mapstructure-decoded types (a
// string, a bool, an int, and a time.Duration) all reach the returned
// Config when no config file is present, covering both the unprefixed and
// ATCHESS_-prefixed spellings that config.go binds for each key.
func TestLoad_NoConfigFile_EnvVarsHonored(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want func(*testing.T, *Config)
	}{
		{
			name: "unprefixed spellings",
			env: map[string]string{
				"ATPROTO_PDS_URL":          "https://example.invalid",
				"SERVER_PORT":              "9999",
				"FIREHOSE_ENABLED":         "true",
				"CHALLENGE_PRUNE_INTERVAL": "30m",
			},
			want: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.ATProto.PDSURL != "https://example.invalid" {
					t.Errorf("cfg.ATProto.PDSURL = %q; want %q", cfg.ATProto.PDSURL, "https://example.invalid")
				}
				if cfg.Server.Port != 9999 {
					t.Errorf("cfg.Server.Port = %d; want 9999", cfg.Server.Port)
				}
				if cfg.Firehose.Enabled != true {
					t.Errorf("cfg.Firehose.Enabled = %v; want true", cfg.Firehose.Enabled)
				}
				if cfg.Challenge.PruneInterval != 30*time.Minute {
					t.Errorf("cfg.Challenge.PruneInterval = %v; want 30m", cfg.Challenge.PruneInterval)
				}
			},
		},
		{
			name: "ATCHESS_-prefixed spellings",
			env: map[string]string{
				"ATCHESS_ATPROTO_PDS_URL":          "https://example.invalid",
				"ATCHESS_SERVER_PORT":              "9999",
				"ATCHESS_FIREHOSE_ENABLED":         "true",
				"ATCHESS_CHALLENGE_PRUNE_INTERVAL": "30m",
				"ATCHESS_ATPROTO_HANDLE":           "probe.example",
			},
			want: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.ATProto.PDSURL != "https://example.invalid" {
					t.Errorf("cfg.ATProto.PDSURL = %q; want %q", cfg.ATProto.PDSURL, "https://example.invalid")
				}
				if cfg.Server.Port != 9999 {
					t.Errorf("cfg.Server.Port = %d; want 9999", cfg.Server.Port)
				}
				if cfg.Firehose.Enabled != true {
					t.Errorf("cfg.Firehose.Enabled = %v; want true", cfg.Firehose.Enabled)
				}
				if cfg.Challenge.PruneInterval != 30*time.Minute {
					t.Errorf("cfg.Challenge.PruneInterval = %v; want 30m", cfg.Challenge.PruneInterval)
				}
				if cfg.ATProto.Handle != "probe.example" {
					t.Errorf("cfg.ATProto.Handle = %q; want %q", cfg.ATProto.Handle, "probe.example")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withNoConfigDir(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with no config file returned unexpected error: %v", err)
			}
			tc.want(t, cfg)
		})
	}
}

// TestLoad_NoConfigFile_ChallengePruneInterval_RejectsNonPositive asserts
// validate() (added by atchess-1c9.62) runs on the no-config-file path
// too, not just the config-file-found path -- the two Load() exits must
// share one validation step rather than have it silently bypassed when no
// file is present (atchess-1c9.64).
func TestLoad_NoConfigFile_ChallengePruneInterval_RejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0s", "-5m"} {
		t.Run(raw, func(t *testing.T) {
			withNoConfigDir(t)
			t.Setenv("CHALLENGE_PRUNE_INTERVAL", raw)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with no config file and CHALLENGE_PRUNE_INTERVAL=%q returned no error; want an error naming challenge.prune_interval", raw)
			}
			if !strings.Contains(err.Error(), "challenge.prune_interval") {
				t.Fatalf("Load() error = %q; want it to name challenge.prune_interval", err.Error())
			}
		})
	}
}
