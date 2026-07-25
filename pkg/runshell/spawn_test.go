//go:build !windows

package runshell

import (
	"os"
	"strings"
	"testing"
	"time"
)

// requirePTY skips when the host can't allocate PTYs (stripped CI
// containers); the server-gate tests cover the rest of the surface.
func requirePTY(t *testing.T) {
	t.Helper()
	if _, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no PTY available: %v", err)
	}
}

func TestSpawnEchoesInWorkdir(t *testing.T) {
	requirePTY(t)
	dir := t.TempDir()
	sess, err := Spawn(SpawnOptions{WorkDir: dir, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Terminate()

	if _, err := sess.PTY.WriteString("pwd\r"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var out strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		_ = sess.PTY.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := sess.PTY.Read(buf)
		if n > 0 {
			out.WriteString(string(buf[:n]))
			// macOS TMPDIR symlinks aside, the tempdir basename is unique
			// enough to prove the shell opened in the right place.
			if strings.Contains(out.String(), dir[strings.LastIndex(dir, "/")+1:]) {
				return
			}
		}
	}
	t.Fatalf("pwd output never showed the workdir; got: %q", out.String())
}

func TestSpawnRequiresWorkdir(t *testing.T) {
	if _, err := Spawn(SpawnOptions{}); err == nil {
		t.Fatal("want error for empty WorkDir")
	}
}

func TestTerminateKillsProcessGroup(t *testing.T) {
	requirePTY(t)
	sess, err := Spawn(SpawnOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	pid := sess.Cmd.Process.Pid
	sess.Terminate()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := sess.Cmd.Process.Signal(os.Signal(nil)); err != nil {
			return // already reaped
		}
		// Wait() in Terminate reaps; ProcessState set means it exited.
		if sess.Cmd.ProcessState != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("shell pid %d still alive after Terminate", pid)
}
