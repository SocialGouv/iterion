package kubernetes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// twoKubectls plants two executables named kubectl, each announcing which
// directory it came from, and points the in-cluster namespace mount at a
// readable scratch file so [Detect] reaches its answer off-cluster. Returns
// the two directories; the caller decides which one PATH names, and when.
func twoKubectls(t *testing.T) (dirA, dirB string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	isolateExecEnvironment(t)
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	dirA, dirB = t.TempDir(), t.TempDir()
	for dir, says := range map[string]string{dirA: "A", dirB: "B"} {
		// `echo` is a shell builtin: the child resolves no PATH of its own,
		// so what it prints names the binary that ran and nothing else.
		if err := os.WriteFile(filepath.Join(dir, kubeBinaryName), []byte("#!/bin/sh\necho "+says+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The namespace mount is a scratch file, not the machine's: on a
	// pod-shaped host (every ARC runner) the real mount exists and the
	// verdict would depend on where the test runs.
	nsMount := inClusterNamespacePath
	ns := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(ns, []byte("iterion-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inClusterNamespacePath = ns
	t.Cleanup(func() { inClusterNamespacePath = nsMount })
	return dirA, dirB
}

// The binary the probe accepted is the binary that runs. [Detect] resolves
// kubectl through the envtrust seam and returns its ABSOLUTE path; every
// kubectl invocation in this package hangs off that value, so no spawn asks
// the live PATH a second time.
//
// The second lookup is what this witness denies. PATH is not frozen for the
// life of a process — a run's own environment carries what the launcher
// gave it, and a spawn resolving `kubectl` at exec.Command() time answers
// with whatever PATH says THEN, which is a different question from the one
// Detect answered. Here PATH moves between the probe and the spawn, and the
// spawn must still run what was probed.
//
// Red when Kubectl.command goes back to spawning the bare kubeBinaryName:
// the impostor in dirB is then what runs, and the assertion reports B.
func TestTheKubectlDetectResolvedIsTheOneThatSpawns(t *testing.T) {
	dirA, dirB := twoKubectls(t)
	t.Setenv("PATH", dirA)

	kubectl, namespace, err := Detect()
	if err != nil {
		t.Fatalf("Detect on a host with kubectl and a namespace mount: %v", err)
	}
	if namespace != "iterion-test" {
		t.Fatalf("Detect namespace = %q, want the scratch mount's", namespace)
	}
	if want := Kubectl(filepath.Join(dirA, kubeBinaryName)); kubectl != want {
		t.Fatalf("Detect kubectl = %q, want the absolute path it resolved (%q): a bare name hands the question back to the PATH", kubectl, want)
	}

	// The probe is over. PATH now names another kubectl entirely.
	t.Setenv("PATH", dirB)
	out, err := kubectl.cmdContext(context.Background(), "version").Output()
	if err != nil {
		t.Fatalf("spawning the kubectl Detect resolved: %v", err)
	}
	if got := string(out); got != "A\n" {
		t.Errorf("the spawn ran the kubectl that says %q, want the one Detect accepted in %s (which says %q) — a spawn "+
			"that re-asks the PATH does not run the binary the probe certified; PATH now names %s", got, dirA, "A\n", dirB)
	}
	// The exec.Cmd names it too, so a caller inspecting the command (the
	// workspace copy pipes two of them together) sees a path, not a name.
	if got := kubectl.cmdContext(context.Background(), "version").Path; got != filepath.Join(dirA, kubeBinaryName) {
		t.Errorf("cmd.Path = %q, want %q", got, filepath.Join(dirA, kubeBinaryName))
	}
}

// The doctor's own entry point answers the same way. [PingContext] resolves
// kubectl through the seam and hands it OUT, so the probes it drives — and
// the node listing the doctor runs after it — spawn the binary this ping
// accepted instead of re-resolving a bare name per probe (the
// check-then-ask-again shape: between the check and the spawn, the answer
// can change).
//
// Red when PingContext discards the path it resolved: the probe is then
// driven by a zero Kubectl, and the recorded receiver is not the planted
// binary.
func TestPingContextHandsOutTheKubectlItResolved(t *testing.T) {
	dirA, _ := twoKubectls(t)
	t.Setenv("PATH", dirA)

	var probed []Kubectl
	old := runKubectlProbe
	t.Cleanup(func() { runKubectlProbe = old })
	runKubectlProbe = func(_ context.Context, kubectl Kubectl, _ ...string) ([]byte, error) {
		probed = append(probed, kubectl)
		return []byte("Kubernetes control plane is running\n"), nil
	}

	kubectl, kctx, namespace, err := PingContext(context.Background())
	if err != nil {
		t.Fatalf("PingContext with kubectl planted and a namespace mount: %v", err)
	}
	if kctx != "in-cluster service account" || namespace != "iterion-test" {
		t.Fatalf("PingContext = %q / %q, want the in-cluster branch", kctx, namespace)
	}
	want := Kubectl(filepath.Join(dirA, kubeBinaryName))
	if kubectl != want {
		t.Errorf("PingContext kubectl = %q, want the absolute path it resolved (%q)", kubectl, want)
	}
	if len(probed) == 0 {
		t.Fatal("the ping drove no probe: the stub was never consulted, so this proves nothing about what it runs")
	}
	for i, got := range probed {
		if got != want {
			t.Errorf("probe %d ran %q, want %q — the binary the lookup accepted is the one the probe must run", i, got, want)
		}
	}
}

// A Kubectl nobody resolved refuses rather than falling back to the bare
// name: the fallback would consult exactly the PATH this type exists to
// stop consulting, and silently. The only constructors are Detect and
// PingContext.
func TestAZeroKubectlRefusesInsteadOfSpawningABareName(t *testing.T) {
	cmd := Kubectl("").cmdContext(context.Background(), "get", "nodes")
	if cmd.Err == nil {
		t.Fatal("a zero Kubectl built a runnable command: a spawn with no resolved path would resolve one from the live PATH")
	}
	if err := cmd.Run(); err == nil {
		t.Error("running a command built from a zero Kubectl must fail, loudly")
	}
}
