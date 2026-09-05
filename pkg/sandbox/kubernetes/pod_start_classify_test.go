package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The classification is the whole contract of the resumable lane: a
// capacity wait must be retried, a broken image must NOT be (it re-fails
// identically on every pod and would burn the delivery budget), and no
// evidence must claim nothing.
func TestClassifyPodStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state podStartState
		want  bool
	}{
		{
			"unschedulable — the fleet is full, another pod later fits",
			podStartState{Readable: true, Phase: "Pending", Scheduled: false, ScheduledReason: "Unschedulable"},
			true,
		},
		{
			"pending, unscheduled, no reason yet — still a placement failure",
			podStartState{Readable: true, Phase: "Pending", Scheduled: false},
			true,
		},
		{
			"scheduled but the node is still creating the container",
			podStartState{Readable: true, Phase: "Pending", Scheduled: true, WaitingReasons: []string{"ContainerCreating"}},
			true,
		},
		{
			"scheduled, PodInitializing",
			podStartState{Readable: true, Phase: "Pending", Scheduled: true, WaitingReasons: []string{"PodInitializing"}},
			true,
		},
		{
			"scheduled, pending, kubelet has reported no container status at all",
			podStartState{Readable: true, Phase: "Pending", Scheduled: true},
			true,
		},
		{
			"a bad image reference re-fails on every pod",
			podStartState{Readable: true, Phase: "Pending", Scheduled: true, WaitingReasons: []string{"ErrImagePull"}},
			false,
		},
		{
			"ImagePullBackOff is the same broken reference, backed off",
			podStartState{Readable: true, Phase: "Pending", Scheduled: true, WaitingReasons: []string{"ImagePullBackOff"}},
			false,
		},
		{
			"an invalid spec is terminal even while unscheduled",
			podStartState{Readable: true, Phase: "Pending", Scheduled: false, ScheduledReason: "Unschedulable", WaitingReasons: []string{"CreateContainerConfigError"}},
			false,
		},
		{
			"a crash-looping container ran: not a placement failure",
			podStartState{Readable: true, Phase: "Running", Scheduled: true, WaitingReasons: []string{"CrashLoopBackOff"}},
			false,
		},
		{
			"scheduled and running but never Ready — the container is up, nothing says capacity",
			podStartState{Readable: true, Phase: "Running", Scheduled: true},
			false,
		},
		{
			"no evidence claims nothing",
			podStartState{},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := classifyPodStart(tc.state)
			if got != tc.want {
				t.Fatalf("resumable = %v, want %v (why %q)", got, tc.want, why)
			}
			if got && strings.TrimSpace(why) == "" {
				t.Fatal("a resumable classification must say what the cluster showed — an operator reading `SANDBOX_CAPACITY` needs the evidence")
			}
		})
	}
}

// kubectlPodJSONShim puts a fake `kubectl` first on PATH whose `wait` times
// out and whose `get pod` answers with the given pod JSON.
func kubectlPodJSONShim(t *testing.T, podJSON string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\" wait \"*) echo 'error: timed out waiting for the condition on pods/iterion-run-x'; exit 1 ;;\n" +
		"  *\" get pod \"*) cat \"$ITERION_TEST_POD_JSON\"; exit 0 ;;\n" +
		"esac\n" +
		"exit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	podFile := filepath.Join(dir, "pod.json")
	if err := os.WriteFile(podFile, []byte(podJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_TEST_POD_JSON", podFile)
}

const unschedulablePodJSON = `{"status":{"phase":"Pending",
 "conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable",
   "message":"0/12 nodes are available: 1 node(s) had untolerated taint {pool: ci}, 11 Insufficient cpu."}]}}`

const badImagePodJSON = `{"status":{"phase":"Pending",
 "conditions":[{"type":"PodScheduled","status":"True"}],
 "containerStatuses":[{"name":"sandbox","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}`

// The measured production shape (#699): every worker at 88–94 % CPU
// requested, the pod waits on the autoscaler and the deadline expires. The
// error must carry sandbox.ErrCapacity so the run parks failed_resumable
// instead of dying terminal and losing an hourly sentinel's tick.
func TestWaitForPodRunning_UnschedulableCarriesTheCapacitySentinel(t *testing.T) {
	kubectlPodJSONShim(t, unschedulablePodJSON)
	err := waitForPodRunning(context.Background(), "ns", "iterion-run-x", 1)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	if !errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("errors.Is(err, sandbox.ErrCapacity) = false — the run hard-fails and nothing retries it: %v", err)
	}
	if !strings.Contains(err.Error(), "Insufficient cpu") {
		t.Fatalf("the cluster's own reason must survive into the error, got %q", err)
	}
}

// The other half of the classification, and the one that keeps it honest:
// a broken image reference re-fails identically on every pod, so it must
// stay terminal rather than burn the whole delivery budget.
func TestWaitForPodRunning_BadImageStaysTerminal(t *testing.T) {
	kubectlPodJSONShim(t, badImagePodJSON)
	err := waitForPodRunning(context.Background(), "ns", "iterion-run-x", 1)
	if err == nil {
		t.Fatal("expected the wait to fail")
	}
	if errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("a broken image reference must NOT be resumable — every redelivery re-hits it: %v", err)
	}
}
