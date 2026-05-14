package ir

import (
	"testing"
)

func TestExpandEnvWithDefault(t *testing.T) {
	t.Run("plain var unset returns empty", func(t *testing.T) {
		t.Setenv("TEST_PLAIN_UNSET", "")
		got := ExpandEnvWithDefault("${TEST_PLAIN_UNSET}")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("plain var set returns value", func(t *testing.T) {
		t.Setenv("TEST_PLAIN_SET", "hello")
		got := ExpandEnvWithDefault("${TEST_PLAIN_SET}")
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("unset with default returns default", func(t *testing.T) {
		t.Setenv("TEST_DEFAULT_UNSET", "")
		got := ExpandEnvWithDefault("${TEST_DEFAULT_UNSET:-fallback}")
		if got != "fallback" {
			t.Errorf("got %q, want %q", got, "fallback")
		}
	})

	t.Run("set with default returns value", func(t *testing.T) {
		t.Setenv("TEST_DEFAULT_SET", "actual")
		got := ExpandEnvWithDefault("${TEST_DEFAULT_SET:-fallback}")
		if got != "actual" {
			t.Errorf("got %q, want %q", got, "actual")
		}
	})

	t.Run("nested default both unset returns innermost", func(t *testing.T) {
		t.Setenv("TEST_A", "")
		t.Setenv("TEST_B", "")
		got := ExpandEnvWithDefault("${TEST_A:-${TEST_B:-final}}")
		if got != "final" {
			t.Errorf("got %q, want %q", got, "final")
		}
	})

	t.Run("nested default outer set wins", func(t *testing.T) {
		t.Setenv("TEST_A", "outer-value")
		t.Setenv("TEST_B", "inner-value")
		got := ExpandEnvWithDefault("${TEST_A:-${TEST_B:-final}}")
		if got != "outer-value" {
			t.Errorf("got %q, want %q", got, "outer-value")
		}
	})

	t.Run("nested default outer unset inner set", func(t *testing.T) {
		t.Setenv("TEST_A", "")
		t.Setenv("TEST_B", "inner-value")
		got := ExpandEnvWithDefault("${TEST_A:-${TEST_B:-final}}")
		if got != "inner-value" {
			t.Errorf("got %q, want %q", got, "inner-value")
		}
	})

	t.Run("triple nesting", func(t *testing.T) {
		t.Setenv("TEST_X", "")
		t.Setenv("TEST_Y", "")
		t.Setenv("TEST_Z", "")
		got := ExpandEnvWithDefault("${TEST_X:-${TEST_Y:-${TEST_Z:-zfallback}}}")
		if got != "zfallback" {
			t.Errorf("got %q, want %q", got, "zfallback")
		}
	})

	t.Run("non-env-substituted string returns unchanged", func(t *testing.T) {
		got := ExpandEnvWithDefault("plain text without vars")
		if got != "plain text without vars" {
			t.Errorf("got %q, want unchanged", got)
		}
	})
}
