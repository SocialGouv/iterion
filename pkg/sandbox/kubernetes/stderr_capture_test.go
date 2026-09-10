package kubernetes

import (
	"io"
	"os"
	"testing"
)

// captureStderr redirects os.Stderr to a pipe for the caller's scope,
// returns a `read` callback that drains everything so far, and a
// `restore` function to undo the redirect. Used by the workspace-copy
// timeout tests to verify the E7 (garbage env warn) and E8 (halfway /
// overrun) stderr-side signals.
func captureStderr(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	}
	read = func() string {
		// Close the write side so ReadAll sees EOF, then re-open a
		// fresh pipe so the caller can drain multiple times without
		// blocking. Order matters — close BEFORE reopening.
		os.Stderr = orig
		_ = w.Close()
		buf, _ := io.ReadAll(r)
		_ = r.Close()
		// Prepare a fresh pipe so a subsequent read is possible.
		nr, nw, perr := os.Pipe()
		if perr == nil {
			r, w = nr, nw
			os.Stderr = w
		}
		return string(buf)
	}
	return read, restore
}
