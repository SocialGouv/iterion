package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFingerprintStore_FirstDiscovery(t *testing.T) {
	dir := t.TempDir()
	fs := NewFingerprintStore(dir)

	schema := json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"}}}`)
	change := fs.Check("mcp.server1.Read", "server1", "Read", schema)
	if change == nil {
		t.Fatal("expected SchemaChange for first discovery, got nil")
	}
	if !change.IsNew {
		t.Error("expected IsNew=true for first discovery")
	}
	if change.QualifiedName != "mcp.server1.Read" {
		t.Errorf("QualifiedName: got %q, want %q", change.QualifiedName, "mcp.server1.Read")
	}
	if change.CurrentFingerprint == "" {
		t.Error("expected non-empty CurrentFingerprint")
	}
	if change.PreviousFingerprint != "" {
		t.Errorf("expected empty PreviousFingerprint for new tool, got %q", change.PreviousFingerprint)
	}
}

func TestFingerprintStore_Unchanged(t *testing.T) {
	dir := t.TempDir()
	fs := NewFingerprintStore(dir)

	schema := json.RawMessage(`{"type":"string"}`)
	_ = fs.Check("mcp.s.t", "s", "t", schema)

	// Second check with same schema should return nil (no change).
	change := fs.Check("mcp.s.t", "s", "t", schema)
	if change != nil {
		t.Errorf("expected nil for unchanged schema, got %+v", change)
	}
}

func TestFingerprintStore_Changed(t *testing.T) {
	dir := t.TempDir()
	fs := NewFingerprintStore(dir)

	v1 := json.RawMessage(`{"type":"string"}`)
	v2 := json.RawMessage(`{"type":"number"}`)

	initial := fs.Check("mcp.s.t", "s", "t", v1)
	if initial == nil || !initial.IsNew {
		t.Fatal("expected IsNew on first check")
	}

	change := fs.Check("mcp.s.t", "s", "t", v2)
	if change == nil {
		t.Fatal("expected SchemaChange for modified schema, got nil")
	}
	if change.IsNew {
		t.Error("expected IsNew=false for changed schema")
	}
	if change.PreviousFingerprint == "" {
		t.Error("expected non-empty PreviousFingerprint")
	}
	if change.CurrentFingerprint == "" {
		t.Error("expected non-empty CurrentFingerprint")
	}
	if change.PreviousFingerprint == change.CurrentFingerprint {
		t.Error("previous and current fingerprints should differ")
	}
}

func TestFingerprintStore_SaveReload(t *testing.T) {
	dir := t.TempDir()

	// Create store, add entries, save.
	fs1 := NewFingerprintStore(dir)
	schema := json.RawMessage(`{"type":"object"}`)
	_ = fs1.Check("mcp.s.t", "s", "t", schema)
	if err := fs1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists.
	path := filepath.Join(dir, "mcp-cache", "schema-fingerprints.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fingerprint file not created: %v", err)
	}

	// Reload into new store.
	fs2 := NewFingerprintStore(dir)
	change := fs2.Check("mcp.s.t", "s", "t", schema)
	if change != nil {
		t.Errorf("expected nil after reload with same schema, got %+v", change)
	}
}

func TestFingerprintStore_ConcurrentChecks(t *testing.T) {
	dir := t.TempDir()
	fs := NewFingerprintStore(dir)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			schema := json.RawMessage(`{"id":` + string(rune('0'+n%10)) + `}`)
			_ = fs.Check("mcp.s.t", "s", "t", schema)
		}(i)
	}
	wg.Wait()

	// If we get here without a race detector complaint, concurrency is safe.
	if err := fs.Save(); err != nil {
		t.Fatalf("Save after concurrent checks: %v", err)
	}
}
