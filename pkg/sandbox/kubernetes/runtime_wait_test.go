package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kubectlShim puts a fake `kubectl` first on PATH whose `wait` times out and
// whose `get pod … -o jsonpath=…` answers with the given scheduling
// condition, so the wait error path can be exercised without a cluster.
func kubectlShim(t *testing.T, scheduledCondition string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\" wait \"*) echo 'error: timed out waiting for the condition on pods/iterion-run-x'; exit 1 ;;\n" +
		"  *\" get pod \"*) printf '%s' '" + scheduledCondition + "'; exit 0 ;;\n" +
		"esac\n" +
		"exit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A pod that never becomes Ready must say why: a request no node can hold
// reads differently from a slow image pull, and the fix differs.
func TestWaitForPodRunningNamesTheSchedulingReason(t *testing.T) {
	kubectlShim(t, "False Unschedulable: 0/3 nodes are available: 3 Insufficient memory.")
	err := waitForPodRunning(context.Background(), "ns", "iterion-run-x", 1)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out waiting for the condition") {
		t.Fatalf("the kubectl wait output must be kept, got %q", msg)
	}
	if !strings.Contains(msg, "scheduling: False Unschedulable: 0/3 nodes are available: 3 Insufficient memory.") {
		t.Fatalf("the PodScheduled condition must be appended, got %q", msg)
	}
}

// A scheduled pod that never became Ready has a status but no reason and no
// message; the decoration must not end in the jsonpath's dangling colon.
func TestWaitForPodRunningScheduledPodReportsTheStatusOnly(t *testing.T) {
	kubectlShim(t, "True : ")
	err := waitForPodRunning(context.Background(), "ns", "iterion-run-x", 1)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	msg := err.Error()
	if !strings.HasSuffix(msg, "\nscheduling: True") {
		t.Fatalf("a scheduled pod must report `scheduling: True` and nothing after it, got %q", msg)
	}
}

func TestWaitForPodRunningWithoutConditionKeepsTheWaitError(t *testing.T) {
	kubectlShim(t, "")
	err := waitForPodRunning(context.Background(), "ns", "iterion-run-x", 1)
	if err == nil || strings.Contains(err.Error(), "scheduling:") {
		t.Fatalf("an unreadable condition must not decorate the error, got %v", err)
	}
}
