package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE DOCS MUST NOT CLAIM GAMES ARE COPIED BETWEEN REPOS.
//
// They did, in four files, for months: "Games stored in both players'
// repositories for redundancy". It is not a stale detail, it is impossible —
// AT Protocol has no way to write into someone else's repository — and it is
// the assumption behind three separate bugs on 2026-08-30: an empty games list,
// a challenge stuck on "pending" forever, and a turn indicator that contradicted
// the game view.
//
// The claim is attractive enough to be rewritten by anyone reasoning from
// "decentralized means replicated", so it gets a gate rather than just a fix.
// docs/data-model.md is exempt: it quotes the claim in order to correct it.
func TestDocsDoNotClaimGamesAreReplicated(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	// Phrases that only appear when someone believes records are duplicated.
	banned := []string{
		"both players' repositories",
		"both players repositories",
		"both players' PDS repositories",
		"for redundancy",
		"stored in both",
		"belong to both players",
	}

	var found []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "vendor/") {
			return nil
		}
		// The page whose job is to correct the claim has to be able to state it.
		if rel == filepath.Join("docs", "data-model.md") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		text := strings.ToLower(string(b))
		for _, phrase := range banned {
			if strings.Contains(text, strings.ToLower(phrase)) {
				found = append(found, rel+": "+phrase)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}

	if len(found) > 0 {
		t.Errorf("documentation claims game records are copied between repositories:\n  %s\n\n"+
			"AT Protocol does not permit writing to another repo, so this cannot be true. "+
			"A game record lives in one repo and each player's moves live in their own; "+
			"state is derived by replay. See docs/data-model.md.",
			strings.Join(found, "\n  "))
	}
}

// A grep-based gate that finds nothing because it is looking in the wrong place
// passes forever. This proves it is actually reading the docs.
func TestDocsClaimScanIsNotVacuous(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	var count int
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "vendor/") {
			return nil
		}
		count++
		return nil
	})
	if count < 5 {
		t.Errorf("only %d markdown files were scanned; the walk is not reaching the docs "+
			"and the claim gate above is decoration", count)
	}
	// The correcting page must exist, or the exemption above silently excuses nothing
	// while the explanation it points at is gone.
	if _, err := os.Stat(filepath.Join(root, "docs", "data-model.md")); err != nil {
		t.Error("docs/data-model.md is missing, but four files now point readers at it")
	}
}
