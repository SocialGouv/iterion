package apikit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFnv1aGoldenVectors(t *testing.T) {
	type vector struct {
		Input string `json:"input"`
		Hash  uint64 `json:"hash"`
	}

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "fnv1a_vectors.json")
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}

	var vectors []vector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("unmarshal golden file: %v", err)
	}

	for _, v := range vectors {
		got := StableHashBytes([]byte(v.Input))
		if got != v.Hash {
			t.Errorf("StableHashBytes(%q) = %d, want %d", v.Input, got, v.Hash)
		}
	}
}

func TestFnv1aEmptySlice(t *testing.T) {
	// Empty input should return FNV offset basis.
	if got := StableHashBytes(nil); got != fnvOffsetBasis {
		t.Errorf("StableHashBytes(nil) = %d, want %d", got, fnvOffsetBasis)
	}
	if got := StableHashBytes([]byte{}); got != fnvOffsetBasis {
		t.Errorf("StableHashBytes([]) = %d, want %d", got, fnvOffsetBasis)
	}
}

func TestFnv1aStabilityAcrossRuns(t *testing.T) {
	// The same input must always produce the same hash (deterministic).
	input := []byte("stability test input")
	h1 := StableHashBytes(input)
	h2 := StableHashBytes(input)
	if h1 != h2 {
		t.Errorf("hash not stable: %d != %d", h1, h2)
	}
}

func TestRequestFingerprintStability(t *testing.T) {
	req := &CacheRequest{
		Model:    "claude-sonnet-4-6",
		System:   "You are a helpful assistant.",
		Messages: []map[string]string{{"role": "user", "content": "Hello"}},
		Tools:    nil,
	}

	hash1 := requestHashHex(req)
	hash2 := requestHashHex(req)
	if hash1 != hash2 {
		t.Errorf("request hash not stable: %q != %q", hash1, hash2)
	}
	if hash1 == "" {
		t.Error("hash should not be empty")
	}

	// A different request must produce a different hash.
	req2 := &CacheRequest{
		Model:    "claude-sonnet-4-6",
		System:   "You are a different assistant.",
		Messages: []map[string]string{{"role": "user", "content": "Hello"}},
	}
	hash3 := requestHashHex(req2)
	if hash3 == hash1 {
		t.Errorf("different requests should have different hashes")
	}
}

func TestFingerprintsFromRequest(t *testing.T) {
	req := &CacheRequest{
		Model:    "claude-sonnet-4-6",
		System:   "system prompt",
		Messages: []map[string]string{{"role": "user", "content": "test"}},
		Tools:    []string{"tool1"},
	}

	fp1 := fingerprintsFromRequest(req)
	fp2 := fingerprintsFromRequest(req)

	if fp1 != fp2 {
		t.Error("fingerprints should be identical for same request")
	}

	// Change model → model hash changes, others stay.
	req2 := &CacheRequest{
		Model:    "claude-opus-4-6",
		System:   "system prompt",
		Messages: []map[string]string{{"role": "user", "content": "test"}},
		Tools:    []string{"tool1"},
	}
	fp3 := fingerprintsFromRequest(req2)
	if fp3.model == fp1.model {
		t.Error("different model should produce different model hash")
	}
	if fp3.system != fp1.system {
		t.Error("same system should produce same system hash")
	}
	if fp3.messages != fp1.messages {
		t.Error("same messages should produce same messages hash")
	}
	if fp3.tools != fp1.tools {
		t.Error("same tools should produce same tools hash")
	}
}
