package blob

import "testing"

func TestIRBlobKey(t *testing.T) {
	key, err := IRBlobKey("run_123")
	if err != nil {
		t.Fatalf("IRBlobKey: %v", err)
	}
	if key != "ir/run_123.json" {
		t.Fatalf("key = %q, want ir/run_123.json", key)
	}
}

func TestIRBlobKeyRejectsTraversal(t *testing.T) {
	if _, err := IRBlobKey("../etc"); err == nil {
		t.Fatal("expected error on traversal run id")
	}
}

func TestValidateIRBlobKey(t *testing.T) {
	// Round-trips its own canonical key.
	key, err := IRBlobKey("run_abc")
	if err != nil {
		t.Fatalf("IRBlobKey: %v", err)
	}
	got, err := validateIRBlobKey(key)
	if err != nil {
		t.Fatalf("validateIRBlobKey(%q): %v", key, err)
	}
	if got != key {
		t.Fatalf("got %q, want %q", got, key)
	}
}

func TestValidateIRBlobKeyRejectsTampered(t *testing.T) {
	cases := []string{
		"ir/../secret.json",       // traversal in the run component
		"tools/run/output",        // wrong prefix
		"ir/run.txt",              // wrong suffix
		"ir/run/nested.json",      // extra path segment
		"run.json",                // no prefix
		"ir/.json",                // empty run id
	}
	for _, c := range cases {
		if _, err := validateIRBlobKey(c); err == nil {
			t.Errorf("validateIRBlobKey(%q): expected error, got nil", c)
		}
	}
}
