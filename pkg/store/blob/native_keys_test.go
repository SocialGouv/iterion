package blob

import (
	"strings"
	"testing"
)

func TestNativeBlobKeysAreOutsideLegacyFamilies(t *testing.T) {
	const id = "pc1_run"
	keys := []struct {
		name string
		key  func() (string, error)
	}{
		{"artifact", func() (string, error) { return artifactKey(id, "node", 1) }},
		{"attachment", func() (string, error) { return attachmentKey(id, "input", "a.txt") }},
		{"attachment prefix", func() (string, error) { return attachmentRunPrefix(id) }},
		{"tool", func() (string, error) { return toolBlobKey(id, "call", "output") }},
		{"tool prefix", func() (string, error) { return toolBlobRunPrefix(id) }},
		{"IR", func() (string, error) { return irBlobKey(id) }},
		{"session", func() (string, error) { return backendSessionKey(id, "session") }},
		{"session prefix", func() (string, error) { return backendSessionRunPrefix(id) }},
		{"file", func() (string, error) { return runFileKey(id, "nested/report.md") }},
		{"file prefix", func() (string, error) { return runFileRunPrefix(id) }},
	}
	for _, tc := range keys {
		t.Run(tc.name, func(t *testing.T) {
			key, err := tc.key()
			if err != nil || !strings.HasPrefix(key, "ports-v1/") || !strings.Contains(key, id) {
				t.Fatalf("key=%q, %v", key, err)
			}
		})
	}
	if got, err := validateIRBlobKey("ports-v1/ir/pc1_run.json"); err != nil || got != "ports-v1/ir/pc1_run.json" {
		t.Fatalf("native IR: %q, %v", got, err)
	}
	for _, key := range []string{"ir/pc1_run.json", "ports-v1/ir/legacy.json", "ports-v1/ir/pc2_future.json", "ports-v1/../ir/pc1_run.json"} {
		if _, err := validateIRBlobKey(key); err == nil {
			t.Errorf("accepted IR key %s", key)
		}
	}
}
