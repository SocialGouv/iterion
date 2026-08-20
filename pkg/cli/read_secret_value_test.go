package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An empty resolved secret must be an ERROR, not an accepted value: the
// server treats an empty `secret` on rotate/create as "leave unchanged", so
// accepting it here would report a clean success while the old (possibly
// compromised) key stays live. Every source path is covered.
func TestReadSecretValue_RejectsEmpty(t *testing.T) {
	t.Run("env set but empty", func(t *testing.T) {
		t.Setenv("ITER_EMPTY_SECRET", "")
		if _, err := ReadSecretValue("ITER_EMPTY_SECRET", "", false); err == nil {
			t.Fatal("empty env var accepted — a rotate would silently no-op")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "empty")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretValue("", f, false); err == nil {
			t.Fatal("empty file accepted — a rotate would silently no-op")
		}
	})

	t.Run("file with only a trailing newline", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "nl")
		if err := os.WriteFile(f, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretValue("", f, false); err == nil {
			t.Fatal("newline-only file accepted — trailing-newline trim left it empty")
		}
	})

	t.Run("a real value passes", func(t *testing.T) {
		t.Setenv("ITER_REAL_SECRET", "sk-real")
		v, err := ReadSecretValue("ITER_REAL_SECRET", "", false)
		if err != nil || v != "sk-real" {
			t.Fatalf("value=%q err=%v, want the real secret through", v, err)
		}
	})

	t.Run("unset env still reports not-set, not empty", func(t *testing.T) {
		os.Unsetenv("ITER_UNSET_SECRET")
		_, err := ReadSecretValue("ITER_UNSET_SECRET", "", false)
		if err == nil || !strings.Contains(err.Error(), "not set") {
			t.Fatalf("err=%v, want the not-set message (distinct from empty)", err)
		}
	})
}
