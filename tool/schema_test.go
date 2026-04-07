package tool

import (
	"encoding/json"
	"testing"
)

func TestSchemaFingerprint_NilEmpty(t *testing.T) {
	if got := SchemaFingerprint(nil); got != "" {
		t.Errorf("nil input: got %q, want empty", got)
	}
	if got := SchemaFingerprint(json.RawMessage{}); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}

func TestSchemaFingerprint_SameWithDifferentWhitespace(t *testing.T) {
	compact := json.RawMessage(`{"a":1,"b":2}`)
	spaced := json.RawMessage(`{  "a" : 1 ,  "b" : 2  }`)

	fp1 := SchemaFingerprint(compact)
	fp2 := SchemaFingerprint(spaced)
	if fp1 == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	if fp1 != fp2 {
		t.Errorf("whitespace difference should not affect fingerprint:\n  compact: %s\n  spaced:  %s", fp1, fp2)
	}
}

func TestSchemaFingerprint_DifferentKeyOrder(t *testing.T) {
	ab := json.RawMessage(`{"a":1,"b":2}`)
	ba := json.RawMessage(`{"b":2,"a":1}`)

	fp1 := SchemaFingerprint(ab)
	fp2 := SchemaFingerprint(ba)
	if fp1 != fp2 {
		t.Errorf("key ordering should not affect fingerprint:\n  ab: %s\n  ba: %s", fp1, fp2)
	}
}

func TestSchemaFingerprint_DifferentSchemas(t *testing.T) {
	s1 := json.RawMessage(`{"type":"string"}`)
	s2 := json.RawMessage(`{"type":"number"}`)

	fp1 := SchemaFingerprint(s1)
	fp2 := SchemaFingerprint(s2)
	if fp1 == fp2 {
		t.Errorf("different schemas should produce different fingerprints: both %s", fp1)
	}
}

func TestSchemaFingerprint_InvalidJSON(t *testing.T) {
	invalid := json.RawMessage(`{not valid json`)
	fp := SchemaFingerprint(invalid)
	if fp == "" {
		t.Error("invalid JSON should fall back to raw-byte hashing, got empty")
	}
	// Verify determinism: same invalid input produces same hash.
	fp2 := SchemaFingerprint(json.RawMessage(`{not valid json`))
	if fp != fp2 {
		t.Errorf("raw-byte fallback not deterministic: %s vs %s", fp, fp2)
	}
}
