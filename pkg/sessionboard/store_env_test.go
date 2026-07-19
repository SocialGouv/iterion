package sessionboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"on", true},
		{"true", true},
		{"1", true},
		{"yes", true},
		{"ON", true},
		{"TRUE", true},
		{" on ", true}, // trimmed
		{"", false},
		{"off", false},
		{"0", false},
		{"false", false},
		{"enabled", false}, // only the four literals count
	}
	for _, tc := range tests {
		t.Run("val="+tc.val, func(t *testing.T) {
			t.Setenv("ITERION_SESSION_BOARD", tc.val)
			if got := Enabled(); got != tc.want {
				t.Errorf("Enabled() with %q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestModelFromEnv(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "  anthropic/claude-haiku-4-5  ")
	if got := ModelFromEnv(); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("ModelFromEnv() = %q, want trimmed model spec", got)
	}
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "")
	if got := ModelFromEnv(); got != "" {
		t.Errorf("ModelFromEnv() unset = %q, want empty", got)
	}
}

func TestFileStoreErrors(t *testing.T) {
	t.Run("empty base dir rejected", func(t *testing.T) {
		if _, err := NewFileStore(""); err == nil {
			t.Error("NewFileStore(\"\") should error")
		}
	})

	dir := t.TempDir()
	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("empty run id rejected", func(t *testing.T) {
		if _, err := st.Load(""); err == nil {
			t.Error("Load(\"\") should error")
		}
		if err := st.Save("", Spec{}); err == nil {
			t.Error("Save(\"\") should error")
		}
	})

	t.Run("corrupt spec surfaces decode error", func(t *testing.T) {
		runDir := filepath.Join(dir, "runs", "run_bad")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "sessionboard.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := st.Load("run_bad")
		if err == nil || !strings.Contains(err.Error(), "decode spec") {
			t.Errorf("corrupt spec: err = %v, want decode error", err)
		}
	})
}

func TestPropsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]any
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs empty map", nil, map[string]any{}, true},
		{"same values", map[string]any{"x": 1, "y": "s"}, map[string]any{"y": "s", "x": 1}, true},
		// JSON round-trip makes int 1 and float64 1 indistinguishable.
		{"int vs float same numeral", map[string]any{"x": 1}, map[string]any{"x": float64(1)}, true},
		{"different value", map[string]any{"x": 1}, map[string]any{"x": 2}, false},
		{"different keys same len", map[string]any{"x": 1}, map[string]any{"y": 1}, false},
		{"different len", map[string]any{"x": 1}, map[string]any{"x": 1, "y": 2}, false},
		{"nested equal", map[string]any{"d": map[string]any{"a": 1}}, map[string]any{"d": map[string]any{"a": 1}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := propsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("propsEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
