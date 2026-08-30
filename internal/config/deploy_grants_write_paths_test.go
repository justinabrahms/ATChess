package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE UNIT FILE MUST GRANT EVERY PATH THE SERVICE WRITES.
//
// Measured 2026-08-30. A deploy of a correct, fully-tested build took
// atchess.abrah.ms down for twenty minutes:
//
//	{"level":"fatal","error":"creating challenge store directory data:
//	 mkdir data: read-only file system","dbPath":"./data/challenges.db"}
//
// The build had gained a SQLite challenge store. The systemd unit runs with
// ProtectSystem=strict and granted only the log directory, because the
// previous build never wrote anything to disk. Every Go test passed, the race
// detector was clean, and the binary ran fine by hand — the defect was
// entirely in the gap between what the code needs from its host and what the
// host is configured to allow, and nothing in this repository looked at that
// gap.
//
// This test is that look. It reads the two declarations and requires them to
// agree:
//
//	config.go        SetDefault("challenge.db_path", ...)  — what we write
//	*.service        Environment= / ReadWritePaths=        — what we may write
//
// WHY IT CHECKS THE ENV OVERRIDE RATHER THAN THE DEFAULT. The defaults are
// relative (./data/...), and a relative path cannot be granted by a unit file
// at all: the grant is absolute, the default depends on the working directory,
// and ProtectSystem=strict resolves the difference by refusing. So the rule is
// not "the default must be granted" — it is "the unit must pin every write
// path to an absolute location, and must grant that location."

var (
	// SetDefault("challenge.db_path", "./data/challenges.db")
	writeDefault = regexp.MustCompile(`SetDefault\("([a-z_]+\.(?:db_path|state_dir))",\s*"([^"]+)"\)`)
	// BindEnv("challenge.db_path", "CHALLENGE_DB_PATH", ...)
	bindEnv = regexp.MustCompile(`BindEnv\("([a-z_]+\.(?:db_path|state_dir))",\s*"([A-Z_]+)"`)
	unitEnv = regexp.MustCompile(`(?m)^Environment="([A-Z_]+)=([^"]+)"`)
	unitRW  = regexp.MustCompile(`(?m)^ReadWritePaths=(.+)$`)
)

func readOrFail(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// TestUnitGrantsEveryWritePath is the gate.
func TestUnitGrantsEveryWritePath(t *testing.T) {
	cfg := readOrFail(t, filepath.Join("internal", "config", "config.go"))
	unit := readOrFail(t, filepath.Join("deploy", "systemd", "atchess-protocol.service"))

	// Which config keys name something we write, and which env var sets each.
	envFor := map[string]string{}
	for _, m := range bindEnv.FindAllStringSubmatch(cfg, -1) {
		envFor[m[1]] = m[2]
	}
	var writeKeys []string
	for _, m := range writeDefault.FindAllStringSubmatch(cfg, -1) {
		writeKeys = append(writeKeys, m[1])
	}
	if len(writeKeys) == 0 {
		t.Fatal("found no write-path defaults in config.go — the scrape is broken, so this gate is vacuous")
	}

	// What the unit pins, and what it permits.
	pinned := map[string]string{}
	for _, m := range unitEnv.FindAllStringSubmatch(unit, -1) {
		pinned[m[1]] = m[2]
	}
	var granted []string
	for _, m := range unitRW.FindAllStringSubmatch(unit, -1) {
		granted = append(granted, strings.Fields(m[1])...)
	}
	if len(granted) == 0 {
		t.Fatal("the unit declares no ReadWritePaths; with ProtectSystem=strict nothing could be written at all")
	}

	for _, key := range writeKeys {
		env := envFor[key]
		if env == "" {
			t.Errorf("%s is a write path with no environment binding, so the unit cannot pin it "+
				"to an absolute location; add a BindEnv for it", key)
			continue
		}
		path, ok := pinned[env]
		if !ok {
			t.Errorf("the service writes %s but the unit does not set %s.\n"+
				"Its default is relative, and a relative path cannot be granted by a unit file — "+
				"ProtectSystem=strict will refuse it and the service will crash-loop on startup. "+
				"Add: Environment=\"%s=/srv/atchess/data/...\" and grant it in ReadWritePaths.",
				key, env, env)
			continue
		}
		if !strings.HasPrefix(path, "/") {
			t.Errorf("%s pins %s to %q, which is relative; it must be absolute", key, env, path)
			continue
		}
		if !anyGrants(granted, path) {
			t.Errorf("the unit pins %s=%s but ReadWritePaths (%s) does not cover it.\n"+
				"The service will start, then die the first time it writes there.",
				env, path, strings.Join(granted, " "))
		}
	}
}

// anyGrants reports whether some granted directory contains path.
func anyGrants(granted []string, path string) bool {
	for _, g := range granted {
		g = strings.TrimSuffix(g, "/")
		if path == g || strings.HasPrefix(path, g+"/") {
			return true
		}
	}
	return false
}

// A gate that scrapes two files is one rename away from matching nothing and
// passing forever. This is what stops that being silent.
func TestWritePathScrapeIsNotVacuous(t *testing.T) {
	cfg := readOrFail(t, filepath.Join("internal", "config", "config.go"))

	found := map[string]bool{}
	for _, m := range writeDefault.FindAllStringSubmatch(cfg, -1) {
		found[m[1]] = true
	}
	// The two that exist today. If either stops being found, the scrape has
	// drifted from the config and the gate above is decoration.
	for _, want := range []string{"challenge.db_path", "firehose.state_dir"} {
		if !found[want] {
			t.Errorf("the scrape no longer finds %q in config.go; the write-path gate is no longer checking it", want)
		}
	}
}
