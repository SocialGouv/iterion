//go:build unix

package model

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The cleanup of a dispatch's copy takes the writer's private temp files
// with it — a write stopped half-way leaves auth.json.tmp.<pid> — and never
// what the node wrote there itself.
func TestTheForfaitCleanupTakesTheWritersTempFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "dispatch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth.json", "auth.json.tmp.4242", "node-notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("sh", "-c", removeHostCodexForfaitScript, "sh", dir).CombinedOutput(); err != nil {
		t.Fatalf("the cleanup failed: %v: %s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "node-notes.txt" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("after the cleanup the dispatch dir holds %v, want only the node's own file", names)
	}
}
