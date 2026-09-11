//go:build unix

package runview

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// TestMergeBundlePrompts_RefusesAFifoPromptWithoutReadingIt: a `.md` that
// is not a regular file is refused by name BEFORE the read. Reading a fifo
// blocks until a writer shows up, and the merge runs inside the studio's
// /api/validate handler and the cloud publisher — an unbounded wait there,
// where the same entry under any other name is simply skipped.
func TestMergeBundlePrompts_RefusesAFifoPromptWithoutReadingIt(t *testing.T) {
	dir := t.TempDir()
	prompts := filepath.Join(dir, bundle.DirPrompts)
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(prompts, "pipe.md"), 0o644); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	b := &bundle.Bundle{Dir: dir, PromptsDir: prompts}
	done := make(chan error, 1)
	go func() { done <- MergeBundlePrompts(&ast.File{}, b) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fifo named like a prompt merged in silence")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MergeBundlePrompts is blocked reading the fifo; it must refuse it by name before the read")
	}
}
