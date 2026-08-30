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
	// Any SetDefault whose VALUE looks like a filesystem path.
	//
	// DETECTED BY VALUE, NOT BY NAME, and that distinction is the whole point.
	// This pattern used to be `(db_path|state_dir)` — a list of the two key
	// names that existed when it was written. Adding session.store_path, a
	// third path the service writes, sailed straight past it: the gate reported
	// green on exactly the omission it exists to catch, because the new key was
	// not named like the old ones.
	//
	// A name-based check only ever knows about the mistakes already made. This
	// one asks what the value IS.
	writeDefault = regexp.MustCompile(`SetDefault\("([a-z_.]+)",\s*"((?:\./|/)[^"]*)"\)`)
	// BindEnv("challenge.db_path", "CHALLENGE_DB_PATH", ...)
	//
	// Matches ANY key, for the same reason writeDefault does: the previous
	// version enumerated the two key names that existed when it was written,
	// so a third write path reported "no environment binding" while its
	// BindEnv sat three lines above the ones it did recognise.
	bindEnv     = regexp.MustCompile(`BindEnv\("([a-z_.]+)",\s*"([A-Z_]+)"`)
	unitEnv     = regexp.MustCompile(`(?m)^Environment="([A-Z_]+)=([^"]+)"`)
	unitRW      = regexp.MustCompile(`(?m)^ReadWritePaths=(.+)$`)
	unitWorkDir = regexp.MustCompile(`(?m)^WorkingDirectory=(.+)$`)
)

func readOrFail(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// TestUnitGrantsEveryWritePath is the gate: everything the service writes must
// land somewhere the unit permits it to write.
//
// IT RESOLVES RELATIVE PATHS RATHER THAN BANNING THEM. An earlier version
// demanded every write path be pinned absolutely by an Environment= line,
// because "a relative default cannot be granted by a unit file". That is not
// true: a relative path resolves against WorkingDirectory=, which the unit
// states, so ./data/x under WorkingDirectory=/srv/atchess/app is
// /srv/atchess/app/data — grantable, and in fact already granted.
//
// The cost of getting that wrong was not theoretical. It made every new state
// path require an Environment= line, which meant editing the unit, which the
// deploy deliberately cannot install, which meant a human running an install
// command by hand. Twice in one afternoon, for paths that were already inside
// a granted directory. A gate that generates toil for no safety gets ignored,
// and this one was well on its way.
func TestUnitGrantsEveryWritePath(t *testing.T) {
	cfg := readOrFail(t, filepath.Join("internal", "config", "config.go"))
	unit := readOrFail(t, filepath.Join("deploy", "systemd", "atchess-protocol.service"))

	envFor := map[string]string{}
	for _, m := range bindEnv.FindAllStringSubmatch(cfg, -1) {
		envFor[m[1]] = m[2]
	}
	defaults := map[string]string{}
	for _, m := range writeDefault.FindAllStringSubmatch(cfg, -1) {
		defaults[m[1]] = m[2]
	}
	if len(defaults) == 0 {
		t.Fatal("found no write-path defaults in config.go — the scrape is broken, so this gate is vacuous")
	}

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

	workdir := ""
	if m := unitWorkDir.FindStringSubmatch(unit); m != nil {
		workdir = m[1]
	}
	if workdir == "" {
		t.Fatal("the unit sets no WorkingDirectory, so a relative write path resolves somewhere nobody can predict")
	}

	for key, def := range defaults {
		// An explicit Environment= pin wins, when there is one.
		effective := def
		if env := envFor[key]; env != "" {
			if p, ok := pinned[env]; ok {
				effective = p
			}
		}
		if !strings.HasPrefix(effective, "/") {
			effective = filepath.Join(workdir, effective)
		}
		if !anyGrants(granted, effective) {
			t.Errorf("the service writes %s, which resolves to %s, but ReadWritePaths (%s) does not cover it.\n"+
				"With ProtectSystem=strict the service will start and then die the first time it writes there.\n"+
				"Either put it under a granted directory, or grant the one it is in.",
				key, effective, strings.Join(granted, " "))
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
	for _, want := range []string{"challenge.db_path", "firehose.state_dir", "session.store_path"} {
		if !found[want] {
			t.Errorf("the scrape no longer finds %q in config.go; the write-path gate is no longer checking it", want)
		}
	}
}
