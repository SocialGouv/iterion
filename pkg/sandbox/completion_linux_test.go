//go:build linux

package sandbox

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("setsid is missing: %v", err)
		}
		t.Skipf("setsid is missing: %v", err)
	}
}

// The command leads its own session and process group, as an exec'd command
// does: its `kill 0` and `kill -- -$$` reach its own processes only.
func TestCompletionCommandLeadsItsOwnSession(t *testing.T) {
	requireSetsid(t)
	c := NewCompletion("t")
	stdout, _ := runWrapped(t, c, "", "sh", "-c", `read -r pid comm state ppid pgrp sid rest < /proc/self/stat; printf '%s %s %s' "$pid" "$pgrp" "$sid"`)
	clean, _, ended := c.Settle(stdout)
	if !ended {
		t.Fatalf("no completion line in %q", stdout)
	}
	f := strings.Fields(string(clean))
	if len(f) != 3 || f[0] != f[1] || f[0] != f[2] {
		t.Fatalf("pid, process group, session = %v: the command does not lead its own session", f)
	}
}

// A TERM sent to the wrapper — by the command's own cleanup, by a pattern
// kill — does not end it before it reports the command's end.
func TestCompletionOutlivesASignalToTheWrapper(t *testing.T) {
	requireSetsid(t)
	c := NewCompletion("t")
	stdout, code := runWrapped(t, c, "", "sh", "-c", `kill -TERM "$PPID"; sleep 0.2; printf done`)
	clean, status, ended := c.Settle(stdout)
	if !ended {
		t.Fatalf("a TERM to the wrapper lost the completion line: %q (exit %d)", stdout, code)
	}
	if status != 0 || string(clean) != "done" {
		t.Fatalf("status %d, stdout %q: want 0 and the command's own output", status, clean)
	}
}

// TestCompletionHelperFillsANonBlockingStdout is not a test: run as the
// command of TestCompletionRetriesALineOnAFullStdout, it makes the shared
// stdout non-blocking, fills the pipe and exits — the way a program using
// asyncio's write pipe leaves it.
func TestCompletionHelperFillsANonBlockingStdout(t *testing.T) {
	if os.Getenv("ITERION_COMPLETION_HELPER") != "fill" {
		t.Skip("the helper runs only as the command of TestCompletionRetriesALineOnAFullStdout")
	}
	if err := syscall.SetNonblock(1, true); err != nil {
		os.Exit(2)
	}
	buf := bytes.Repeat([]byte("x"), 65536)
	for len(buf) > 0 {
		n, err := syscall.Write(1, buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err == syscall.EAGAIN {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}

// A command that leaves stdout non-blocking and full does not lose the
// completion line: the wrapper retries it until the reader drains the pipe.
func TestCompletionRetriesALineOnAFullStdout(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewCompletion("t")
	wrapped := c.Wrap([]string{self, "-test.run=^TestCompletionHelperFillsANonBlockingStdout$"})
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Env = append(os.Environ(), "ITERION_COMPLETION_HELPER=fill")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// A slow reader: the pipe is full when the command exits.
	time.Sleep(time.Second)
	stdout, rerr := io.ReadAll(out)
	if werr := cmd.Wait(); werr != nil || rerr != nil {
		t.Fatalf("the wrapped helper failed: wait %v, read %v", werr, rerr)
	}
	clean, status, ended := c.Settle(stdout)
	if !ended {
		t.Fatalf("a full non-blocking stdout lost the completion line (%d bytes arrived)", len(stdout))
	}
	if status != 0 || len(clean) != 65536 || strings.Trim(string(clean), "x") != "" {
		t.Fatalf("status %d, %d bytes of output: want 0 and the helper's 65536 bytes", status, len(clean))
	}
}
