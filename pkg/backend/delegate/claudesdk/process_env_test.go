package claudesdk

import (
	"strings"
	"testing"
)

// mergeCmdEnv must REPLACE inherited keys (no duplicates) and treat an empty
// override value as suppression. The claude CLI prefers ANTHROPIC_API_KEY over
// the CLAUDE_CONFIG_DIR OAuth token, so a per-run forfait that sets
// ANTHROPIC_API_KEY="" to shadow a runner's inherited key relies on this: a
// plain append would leave a duplicate whose resolution is not guaranteed.
func TestMergeCmdEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-dead-inherited",
		"ANTHROPIC_BASE_URL=https://stale.zai",
		"HOME=/root",
	}
	override := map[string]string{
		"CLAUDE_CONFIG_DIR":    "/tmp/iter-oauth-xyz",
		"ANTHROPIC_API_KEY":    "", // suppress the inherited dead key
		"ANTHROPIC_BASE_URL":   "", // suppress stale z.ai base
		"ANTHROPIC_AUTH_TOKEN": "",
	}
	got := mergeCmdEnv(base, override)

	// No inherited ANTHROPIC_API_KEY may survive, in any form.
	for _, kv := range got {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("ANTHROPIC_API_KEY must be suppressed entirely, found %q", kv)
		}
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			t.Errorf("ANTHROPIC_BASE_URL must be suppressed entirely, found %q", kv)
		}
	}
	// The forfait dir and untouched inherited vars survive exactly once.
	if n := countKey(got, "CLAUDE_CONFIG_DIR"); n != 1 || !contains(got, "CLAUDE_CONFIG_DIR=/tmp/iter-oauth-xyz") {
		t.Errorf("CLAUDE_CONFIG_DIR not set once: n=%d env=%v", n, got)
	}
	if !contains(got, "PATH=/usr/bin") || !contains(got, "HOME=/root") {
		t.Errorf("inherited unrelated vars dropped: %v", got)
	}
}

// A non-empty override replaces (not duplicates) an inherited key.
func TestMergeCmdEnv_ReplaceNonEmpty(t *testing.T) {
	got := mergeCmdEnv([]string{"K=old", "OTHER=x"}, map[string]string{"K": "new"})
	if countKey(got, "K") != 1 || !contains(got, "K=new") {
		t.Errorf("K should be replaced once with new: %v", got)
	}
	if contains(got, "K=old") {
		t.Errorf("stale K=old survived: %v", got)
	}
}

func TestMergeCmdEnv_EmptyOverrideReturnsBase(t *testing.T) {
	base := []string{"A=1"}
	if got := mergeCmdEnv(base, nil); len(got) != 1 || got[0] != "A=1" {
		t.Errorf("empty override should return base unchanged: %v", got)
	}
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func contains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
