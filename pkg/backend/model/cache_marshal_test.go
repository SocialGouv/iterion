package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// TestSystemBlockCacheControlMarshals verifies that when the claw backend
// builds a request with a SystemBlocks ContentBlock carrying CacheControl,
// the generated JSON wire body — when marshaled with the same encoder
// strategy used by the public Message type — preserves the cache_control
// marker as the Anthropic API expects it (`{"type":"ephemeral"}`).
//
// This is a defensive test against silent regressions: the recursive
// Property fix (api/types.go) and any future field reshuffles must not
// drop the cache_control field, otherwise the prompt cache will silently
// miss in production with no error.
func TestSystemBlockCacheControlMarshals(t *testing.T) {
	block := api.ContentBlock{
		Type:         "text",
		Text:         "You are a careful assistant.",
		CacheControl: api.EphemeralCacheControl(),
	}

	out, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("system block JSON missing cache_control marker.\n  got: %s\n  expected substring: \"cache_control\":{\"type\":\"ephemeral\"}", got)
	}

	// Round-trip: decode back and confirm CacheControl survives.
	var decoded api.ContentBlock
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.CacheControl == nil {
		t.Fatal("CacheControl dropped in decode")
	}
	if decoded.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type = %q, want \"ephemeral\"", decoded.CacheControl.Type)
	}
}
