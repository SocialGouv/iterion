package privacy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestVault_OpenOrCreate_Empty(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := v.Len(); got != 0 {
		t.Fatalf("Len=%d, want 0", got)
	}
}

func TestVault_AddPersists(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := v.Add("PII_aaaaaaaa", "alice@example.com", "email"); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Re-open and verify entry survives.
	v2, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	val, cat, ok := v2.Get("PII_aaaaaaaa")
	if !ok || val != "alice@example.com" || cat != "email" {
		t.Fatalf("get after reopen: val=%q cat=%q ok=%v", val, cat, ok)
	}
}

func TestVault_Idempotent(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := v.Add("PII_aaaaaaaa", "alice@example.com", "email"); err != nil {
		t.Fatalf("add 1: %v", err)
	}
	// Second add of same token must not error and must not duplicate.
	if err := v.Add("PII_aaaaaaaa", "alice@example.com", "email"); err != nil {
		t.Fatalf("add 2: %v", err)
	}
	if got := v.Len(); got != 1 {
		t.Fatalf("Len=%d, want 1", got)
	}
}

func TestVault_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := v.Add("PII_aaaaaaaa", "x", "email"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// After Save, no .tmp residual must remain in the run dir.
	entries, err := os.ReadDir(filepath.Dir(v.Path()))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("found residual tmp file: %s", e.Name())
		}
	}
}

func TestVault_Permissions0600(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := v.Add("PII_aaaaaaaa", "x", "email"); err != nil {
		t.Fatalf("add: %v", err)
	}
	st, err := os.Stat(v.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("vault perm = %v, want 0o600", mode)
	}
}

func TestVault_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs", "run-1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bogus := vaultFile{
		Version:   2,
		RunID:     "run-1",
		CreatedAt: "2026-01-01T00:00:00Z",
		Entries:   map[string]vaultEntry{},
	}
	body, _ := json.Marshal(bogus)
	if err := os.WriteFile(filepath.Join(runDir, "pii_vault.json"), body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := OpenOrCreate("run-1", dir); err == nil {
		t.Fatalf("expected error on version mismatch, got nil")
	}
}

func TestVault_Concurrent(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenOrCreate("run-1", dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(n int) {
			defer wg.Done()
			tok := tokenFromInt(n)
			_ = v.Add(tok, "value", "email")
		}(i)
	}
	wg.Wait()
	if got := v.Len(); got != N {
		t.Fatalf("Len after concurrent adds = %d, want %d", got, N)
	}
}

func TestVault_RejectsTraversalRunID(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenOrCreate("../escape", dir); err == nil {
		t.Fatalf("expected error for traversal run ID")
	}
	if _, err := OpenOrCreate("foo/bar", dir); err == nil {
		t.Fatalf("expected error for separator in run ID")
	}
	if _, err := OpenOrCreate("", dir); err == nil {
		t.Fatalf("expected error for empty run ID")
	}
}

// tokenFromInt produces a deterministic 8-hex token from an int —
// used by TestVault_Concurrent to spawn N distinct tokens.
func tokenFromInt(n int) string {
	const hex = "0123456789abcdef"
	out := []byte("PII_00000000")
	idx := 4
	for i := 0; i < 8; i++ {
		out[idx+i] = hex[(n>>(4*(7-i)))&0xF]
	}
	return string(out)
}
