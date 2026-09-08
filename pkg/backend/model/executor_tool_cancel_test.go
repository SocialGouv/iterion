//go:build unix

package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// grandchildRecipe returns a shell snippet that backgrounds a heartbeat
// loop (the GRANDCHILD: a child of the node's own shell, not of the test)
// and then blocks on `wait`. The loop appends to heartPath every 100ms, so
// its liveness is observable from the outside without inspecting the
// process table — a file a dead process cannot keep growing, and which a
// zombie (reparented but unreaped) cannot touch either.
//
// The oracle has to be the grandchild: killing only the shell leaves the
// loop running AND holding the inherited stdout pipe, which is exactly the
// shape where the cancel stops the wait without stopping the work.
//
// The loop is bounded at ~20s rather than infinite so a REGRESSION (or the
// canary that falsifies this guard) leaves no process spinning on the host
// after the test binary exits. The assertion window is under a second, so
// the bound never reaches the oracle.
func grandchildRecipe(heartPath string) string {
	return `( i=0; while [ "$i" -lt 200 ]; do printf 'tick\n' >> "` + heartPath + `"; ` +
		`i=$((i+1)); sleep 0.1; done ) & wait`
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// waitForHeartbeat blocks until the grandchild has written at least one
// tick, proving it is running before the test cancels anything.
func waitForHeartbeat(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fileSize(t, path) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild never wrote a heartbeat to %s — the recipe did not start", path)
}

// assertGrandchildStopped fails when the heartbeat file keeps growing after
// cancellation: the shell died, the loop it spawned did not.
func assertGrandchildStopped(t *testing.T, path string) {
	t.Helper()
	// One grace interval for a tick already in flight when the signal
	// landed, then the file must be frozen.
	time.Sleep(300 * time.Millisecond)
	before := fileSize(t, path)
	time.Sleep(600 * time.Millisecond)
	after := fileSize(t, path)
	if after != before {
		t.Fatalf("the grandchild is still running after cancellation: heartbeat grew %d -> %d bytes in 600ms (%s)",
			before, after, path)
	}
}

// TestToolNodeCancelStopsTheGrandchild is the ticket's oracle: after
// cancelling a tool node whose shell backgrounded a process, no descendant
// survives. Asserting only that Execute returned would prove we stopped
// waiting, not that we stopped the work.
func TestToolNodeCancelStopsTheGrandchild(t *testing.T) {
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")

	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "cancelled_shell"},
		Command:  grandchildRecipe(heart),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, node, map[string]any{})
		done <- err
	}()

	waitForHeartbeat(t, heart)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return within 5s of cancellation — the node's read is still held by a surviving descendant")
	}

	assertGrandchildStopped(t, heart)
}

// TestToolNodeScriptCancelStopsTheGrandchild is the same oracle on the
// `script:` recipe path, which builds its *exec.Cmd through a different
// constructor (toolNodeScriptCommand) and would otherwise drift.
func TestToolNodeScriptCancelStopsTheGrandchild(t *testing.T) {
	dir := t.TempDir()
	heart := filepath.Join(dir, "heartbeat")

	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "cancelled_script"},
		Language: "sh",
		Script:   grandchildRecipe(heart),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, node, map[string]any{})
		done <- err
	}()

	waitForHeartbeat(t, heart)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return within 5s of cancellation — the script node's read is still held by a surviving descendant")
	}

	assertGrandchildStopped(t, heart)
}

// TestToolNodeUncancelledRunIsUnaffected is the negative case: a node that
// finishes on its own keeps its output and its exit status, and nothing
// kills a process group behind its back. Without it, "no descendant
// survives" would also be satisfied by killing every tool node.
func TestToolNodeUncancelledRunIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir))
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "normal_shell"},
		// A backgrounded child that the recipe itself waits for: the node
		// completes normally and its descendant completes with it.
		Command: `( sleep 0.2; printf 'done\n' > "` + marker + `" ) & wait; printf 'ok\n'`,
	}

	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, _ := out["result"].(string); got != "ok" {
		t.Fatalf("output = %#v, want the node's own stdout %q", out, "ok")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the backgrounded child did not complete: %v", statErr)
	}
}
