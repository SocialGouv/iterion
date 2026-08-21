package delegate

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionFilesRootPiUsesStateDir(t *testing.T) {
	storeDir := t.TempDir()
	task := Task{StoreDir: storeDir, WorkDir: t.TempDir()}
	got := SessionFilesRoot(context.Background(), task, BackendPi)
	want, _ := task.StateDir(BackendPi)
	if got != want {
		t.Fatalf("pi root = %q, want StateDir %q", got, want)
	}
	if got != filepath.Join(storeDir, BackendPi) && got != filepath.Join(storeDir, "pi") {
		// StateDir joins storeDir with backend name.
		if filepath.Base(got) != BackendPi {
			t.Fatalf("pi root base = %q, want %q", filepath.Base(got), BackendPi)
		}
	}
}

func TestSessionFilesRootUnknownBackendEmpty(t *testing.T) {
	if got := SessionFilesRoot(context.Background(), Task{}, "kimi"); got != "" {
		t.Fatalf("kimi root = %q, want empty", got)
	}
}
