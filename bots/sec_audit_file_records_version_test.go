package bots

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func filterCachedFilesScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	for _, blk := range commandBackticks(string(raw)) {
		if !strings.Contains(blk, "cache_hit_pct") {
			continue
		}
		const marker = "python3 -I -c \""
		start := strings.Index(blk, marker)
		if start < 0 {
			t.Fatal("filter_cached_files embeds no python3 -I -c body")
		}
		start += len(marker)
		end := strings.LastIndex(blk, "\"")
		if end <= start {
			t.Fatal("unterminated python3 -I -c body in filter_cached_files")
		}
		return blk[start:end]
	}
	t.Fatal("filter_cached_files command not found")
	return ""
}

type filterCachedResult struct {
	FreshCandidates  []map[string]any `json:"fresh_candidates"`
	CachedCandidates []map[string]any `json:"cached_candidates"`
	CachedVerdicts   []map[string]any `json:"cached_verdicts"`
	CacheStats       struct {
		Fresh  int `json:"fresh"`
		Cached int `json:"cached"`
	} `json:"cache_stats"`
}

// Mutation: restore the lexical string compare and 0.1.9 sorts after 0.1.11,
// so the first subtest replays a verdict the version bump was meant to retire.
func TestFilterCachedFilesComparesScannerVersionsNumerically(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	scriptPath := filepath.Join(t.TempDir(), "filter_cached.py")
	if err := os.WriteFile(scriptPath, []byte(filterCachedFilesScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	records := filepath.Join(base, "records")
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	const rel = "src/a.ts"
	content := []byte("export const a = 1;\n")
	if err := os.WriteFile(filepath.Join(ws, rel), content, 0o644); err != nil {
		t.Fatal(err)
	}
	contentDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	recordDigest := sha1.Sum([]byte(rel))
	recordName := fmt.Sprintf("%x.json", recordDigest[:8])
	recordPath := filepath.Join(records, recordName)
	candidate := []map[string]any{{"id": "C-1", "file": rel}}
	verdict := []map[string]any{{"candidate_id": "C-1", "verdict": "confirmed"}}

	run := func(t *testing.T, recordVersion, scannerVersion string) filterCachedResult {
		t.Helper()
		writeJSON(t, recordPath, map[string]any{
			"path": rel,
			"history": []map[string]any{{
				"content_hash":    contentDigest,
				"analysed_at":     time.Now().UTC().Format(time.RFC3339),
				"scanner_version": recordVersion,
				"candidates":      candidate,
				"verdicts":        verdict,
				"issues_created":  []string{},
			}},
		})
		candidatesJSON, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", scriptPath)
		cmd.Env = append(os.Environ(),
			"RECORDS_DIR="+records,
			"WS="+ws,
			"CANDIDATES="+string(candidatesJSON),
			"SCANNER_V="+scannerVersion,
			"TTL=30",
		)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("filter_cached_files exited non-zero (%v): %q", err, raw)
		}
		var got filterCachedResult
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("filter_cached_files output is not JSON: %v (%q)", err, raw)
		}
		return got
	}

	t.Run("0.1.9 is older than 0.1.11", func(t *testing.T) {
		got := run(t, "sec-audit-source@0.1.9", "sec-audit-source@0.1.11")
		if got.CacheStats.Fresh != 1 || got.CacheStats.Cached != 0 || len(got.FreshCandidates) != 1 {
			t.Errorf("0.1.9 verdict was replayed under 0.1.11: %+v", got.CacheStats)
		}
	})
	t.Run("the package prefix is outside the version comparison", func(t *testing.T) {
		got := run(t, "other@0.1.9", "sec-audit-source@0.1.11")
		if got.CacheStats.Fresh != 1 || got.CacheStats.Cached != 0 {
			t.Errorf("the comparator did not isolate the version after @: %+v", got.CacheStats)
		}
	})
	t.Run("0.1.10 is older than 0.1.11", func(t *testing.T) {
		got := run(t, "sec-audit-source@0.1.10", "sec-audit-source@0.1.11")
		if got.CacheStats.Fresh != 1 || got.CacheStats.Cached != 0 {
			t.Errorf("0.1.10 verdict was replayed under 0.1.11: %+v", got.CacheStats)
		}
	})
	t.Run("equal versions replay the cached verdict", func(t *testing.T) {
		got := run(t, "sec-audit-source@0.1.11", "sec-audit-source@0.1.11")
		if got.CacheStats.Fresh != 0 || got.CacheStats.Cached != 1 {
			t.Fatalf("equal scanner versions were not replayed: %+v", got.CacheStats)
		}
		if !reflect.DeepEqual(got.CachedVerdicts, verdict) {
			t.Errorf("cached verdict changed on replay\ngot:  %#v\nwant: %#v", got.CachedVerdicts, verdict)
		}
	})
	t.Run("a newer record remains reusable", func(t *testing.T) {
		got := run(t, "sec-audit-source@0.1.11", "sec-audit-source@0.1.9")
		if got.CacheStats.Fresh != 0 || got.CacheStats.Cached != 1 {
			t.Errorf("a newer record should satisfy an older scanner version: %+v", got.CacheStats)
		}
	})
	t.Run("an unknown record version fails closed", func(t *testing.T) {
		got := run(t, "custom-before-metadata", "sec-audit-source@0.1.11")
		if got.CacheStats.Fresh != 1 || got.CacheStats.Cached != 0 {
			t.Errorf("an unparsable record version was replayed: %+v", got.CacheStats)
		}
	})
	t.Run("an unknown current version fails closed", func(t *testing.T) {
		got := run(t, "sec-audit-source@0.1.11", "custom-after-metadata")
		if got.CacheStats.Fresh != 1 || got.CacheStats.Cached != 0 {
			t.Errorf("an unparsable current version accepted a cache hit: %+v", got.CacheStats)
		}
	})
	t.Run("exact equality still replays a non-numeric version", func(t *testing.T) {
		got := run(t, "custom-before-metadata", "custom-before-metadata")
		if got.CacheStats.Fresh != 0 || got.CacheStats.Cached != 1 {
			t.Errorf("exact non-numeric equality should replay: %+v", got.CacheStats)
		}
	})
}
