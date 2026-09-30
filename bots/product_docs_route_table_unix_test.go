//go:build unix

package bots

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestProductDocsRouteTableDiesWithACancelledNode: the engine cancels a node by
// killing its process group. The probe runs in a group of its own, so the
// node's death must reach it — nothing the probe does outlives a cancelled run.
func TestProductDocsRouteTableDiesWithACancelledNode(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	dir := t.TempDir()
	started, late := filepath.Join(dir, "STARTED"), filepath.Join(dir, "LATE")
	probeScriptNet(t, ws, "import time\nopen("+pyString(t, started)+", 'w').write('x')\ntime.sleep(2)\nopen("+pyString(t, late)+", 'w').write('x')\nprint('GET /')\n")
	base := snapshotRunBase(t, ws)
	cmd := exec.Command("sh", "-c", routeTableCommand(t, ws, filepath.Join(ws, ".golden-master"), base, "routes.txt", shippedProbeTimeout))
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "TMPDIR="+t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatal("the probe never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(late); err == nil {
		t.Fatal("the probe outlived its cancelled node and wrote after it")
	}
}
