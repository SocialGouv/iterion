package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// kubectlCaptureShim puts a fake `kubectl` first on PATH that records every
// manifest piped to `apply -f -` into the capture file and succeeds at
// everything else, so Start can be driven end to end without a cluster and
// the pod it actually applies can be read back.
func kubectlCaptureShim(t *testing.T) (capture string) {
	t.Helper()
	dir := t.TempDir()
	capture = filepath.Join(dir, "applied.jsonl")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\" apply \"*) cat >> \"$ITERION_TEST_KUBECTL_CAPTURE\"; printf '\\n---\\n' >> \"$ITERION_TEST_KUBECTL_CAPTURE\"; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_TEST_KUBECTL_CAPTURE", capture)
	return capture
}

// appliedPod returns the Pod manifest Start piped to `kubectl apply`.
func appliedPod(t *testing.T, capture string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("nothing was applied: %v", err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("applied manifest is not JSON: %v\n%s", err, doc)
		}
		if obj["kind"] == "Pod" {
			return obj
		}
	}
	t.Fatalf("no Pod among the applied manifests:\n%s", raw)
	return nil
}

// The parsed policy must reach the pod Start applies — the manifest tests
// render PodManifestInput directly, so only this test covers the one
// production composition site (the BuildPodManifest call in Start).
func TestStartAppliesTheSchedulingPolicy(t *testing.T) {
	capture := kubectlCaptureShim(t)
	sched, err := schedulingFromEnv(envOf(map[string]string{
		RequestsCPUEnvVar:    "2",
		RequestsMemoryEnvVar: "4Gi",
		SpreadEnvVar:         "hostname",
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	_, startErr := d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})

	pod := appliedPod(t, capture)
	spec, _ := pod["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("applied pod has %d containers: %v", len(containers), spec)
	}
	c, _ := containers[0].(map[string]any)
	res, _ := c["resources"].(map[string]any)
	req, _ := res["requests"].(map[string]any)
	if req["cpu"] != "2" || req["memory"] != "4Gi" {
		t.Fatalf("applied pod must carry the parsed requests, got resources=%v (Start error: %v)", res, startErr)
	}
	spread, _ := spec["topologySpreadConstraints"].([]any)
	if len(spread) != 1 {
		t.Fatalf("applied pod must carry the spread constraint, got %v (Start error: %v)", spec["topologySpreadConstraints"], startErr)
	}
	if startErr != nil {
		// The shim answers every later kubectl call with an empty success;
		// a Start that still fails after the apply is a change in what Start
		// needs from the cluster and must be looked at, not ignored.
		t.Fatalf("Start failed after applying the pod: %v", startErr)
	}
}

// Without a policy the applied pod is the manifest of a driver that never
// had the knobs.
func TestStartAppliesNoPolicyByDefault(t *testing.T) {
	capture := kubectlCaptureShim(t)
	sched, _ := schedulingFromEnv(envOf(nil))
	d := &Driver{namespace: "ns", kubectl: "kubectl", logger: iterlog.New(iterlog.LevelInfo, io.Discard), sched: sched}
	prepared := &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}
	_, _ = d.Start(context.Background(), prepared, sandbox.RunInfo{RunID: "01a0-run"})
	pod := appliedPod(t, capture)
	spec, _ := pod["spec"].(map[string]any)
	c, _ := spec["containers"].([]any)[0].(map[string]any)
	if _, present := c["resources"]; present {
		t.Fatalf("no resources must be rendered by default, got %v", c["resources"])
	}
	if _, present := spec["topologySpreadConstraints"]; present {
		t.Fatalf("no spread must be rendered by default, got %v", spec["topologySpreadConstraints"])
	}
}

func TestNodeLabelCoverageCountsLabelledNodes(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"get nodes \"*) printf '%s' '{\"items\":[{\"metadata\":{\"labels\":{\"kubernetes.io/hostname\":\"a\",\"topology.kubernetes.io/zone\":\"z1\"}}},{\"metadata\":{\"labels\":{\"kubernetes.io/hostname\":\"b\"}}},{\"metadata\":{\"labels\":{\"kubernetes.io/hostname\":\"c\",\"topology.kubernetes.io/zone\":\"z2\"}}}]}'; exit 0 ;;\n" +
		"esac\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	labelled, total, err := NodeLabelCoverage(context.Background(), "topology.kubernetes.io/zone")
	if err != nil || labelled != 2 || total != 3 {
		t.Fatalf("coverage = %d/%d, %v; want 2/3", labelled, total, err)
	}
	labelled, total, err = NodeLabelCoverage(context.Background(), "example.com/rack")
	if err != nil || labelled != 0 || total != 3 {
		t.Fatalf("coverage for an absent key = %d/%d, %v; want 0/3", labelled, total, err)
	}
}

// The chart's runner Role cannot list nodes; the error must carry kubectl's
// reason, not a bare exit status the doctor cannot explain.
func TestNodeLabelCoverageReportsKubectlStderr(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"get nodes \"*) echo 'Error from server (Forbidden): nodes is forbidden: User \"system:serviceaccount:ns:runner\" cannot list resource \"nodes\" in API group \"\" at the cluster scope' >&2; exit 1 ;;\n" +
		"esac\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, err := NodeLabelCoverage(context.Background(), "topology.kubernetes.io/zone")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") || !strings.Contains(err.Error(), "cannot list resource") {
		t.Fatalf("the error must carry kubectl's stderr, got %v", err)
	}
}

// A failure with nothing on stderr (a killed probe) must not end in a
// dangling separator.
func TestNodeLabelCoverageWithoutStderrKeepsTheExitError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, err := NodeLabelCoverage(context.Background(), "topology.kubernetes.io/zone")
	if err == nil || err.Error() != "kubectl get nodes: exit status 1" {
		t.Fatalf("want the bare exit error, got %q", err)
	}
}
