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

// The gateway crossing names the host forwards as explicit empties: a
// workspace .env must not lift the in-container address-class rule nor
// answer the catalog for a record the host never consulted.
func TestApplyDotEnvDoesNotFillTheGatewayCrossingNames(t *testing.T) {
	for _, name := range []string{"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "ITERION_OPENAI_COMPATIBLE_RESOLVED"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(name+"=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(name, "")
			loadDotEnvFromCwd()
			if v, ok := os.LookupEnv(name); !ok || v != "" {
				t.Fatalf("%s = (%q, %v), want the forwarded empty kept — the workspace .env would lift an in-container rule", name, v, ok)
			}
		})
	}
}
