package worktreepool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidGitfilePathCannotBecomeOrphan(t *testing.T) {
	for _, name := range []string{"ordinary", "not a git repository", "not a working tree"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if e := os.Mkdir(dir, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git"), []byte("malformed gitfile\n"), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, "precious"), []byte("uncommitted work"), 0600); e != nil {
				t.Fatal(e)
			}
			got := inspectGitForScan(t.Context(), dir, false, false)
			t.Logf("landing=%q err=%v", got.landing, got.err)
			if got.landing != LandingNowhere || got.err == nil {
				t.Fatalf("invalid gitfile became deletion candidate: %+v", got)
			}
		})
	}
}
func TestInvalidGitfileIsNeverSwept(t *testing.T) {
	store := filepath.Join(t.TempDir(), "not a working tree")
	dir := filepath.Join(store, "worktrees", "run-probe")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{".git": "malformed gitfile\n", "precious": "uncommitted work"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	opts := ScanOptions{Level: LevelAggressive}
	entries, e := Scan(store, opts)
	if e != nil {
		t.Fatal(e)
	}
	result := Sweep(entries, SweepOptions{ScanOptions: opts, Apply: true})
	t.Logf("entries=%+v deleted=%+v spared=%+v", entries, result.Deleted, result.Spared)
	if _, e := os.Stat(filepath.Join(dir, "precious")); e != nil {
		t.Fatalf("malformed repository content destroyed: %v", e)
	}
}
