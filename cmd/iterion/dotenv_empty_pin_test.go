package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplyDotEnvDoesNotFillASetEmptyName pins the runner half of the
// empty-endpoint contract (#2315): a var the process already holds — empty
// included — is "existing" for applyDotEnv (LookupEnv), so a workspace .env
// cannot fill a name the host forwarded as an explicit empty.
func TestApplyDotEnvDoesNotFillASetEmptyName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("XAI_BASE_URL=https://repo.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XAI_BASE_URL", "")
	loadDotEnvFromCwd()
	if v, ok := os.LookupEnv("XAI_BASE_URL"); !ok || v != "" {
		t.Fatalf("XAI_BASE_URL = (%q, %v), want the forwarded empty kept — the workspace .env picked the endpoint", v, ok)
	}
}
