package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
)

func benchInput(seccomp string) Input {
	return Input{
		Namespace: "ns",
		Name:      "cm6-bench-1-bench",
		RunLabel:  "cm6-bench-1",
		Image:     "registry.example/bench@sha256:" + strings.Repeat("a", 64),
		Seccomp:   seccomp,
		Command:   []string{"/bench/inpodbench"},
		Args:      []string{"-binary", "/usr/local/bin/iterion", "-require-uid", "1000"},
		Resources: kubernetes.PodResources{
			Requests: kubernetes.ResourceList{CPU: "100m", Memory: "256Mi"},
			Limits:   kubernetes.ResourceList{CPU: "2", Memory: "2Gi"},
		},
		DeadlineSeconds: 900,
	}
}

// driverPod is what the driver's own builder renders for the same input,
// read back as the generic object the renderer edits.
func driverPod(t *testing.T, in Input) map[string]any {
	t.Helper()
	raw, err := kubernetes.BuildPodManifest(kubernetes.PodManifestInput{
		Namespace:             in.Namespace,
		Name:                  in.Name,
		RunID:                 in.RunLabel,
		Spec:                  sandbox.Spec{Image: in.Image, User: in.User},
		Resources:             in.Resources,
		ActiveDeadlineSeconds: in.DeadlineSeconds,
	})
	if err != nil {
		t.Fatalf("the driver's builder refused the bench input: %v", err)
	}
	var pod map[string]any
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// withoutSubstitutions removes the fields the bench is DECLARED to replace,
// so what remains is compared whole: a renderer that touched anything else —
// a capability, no_new_privs, the user, a volume — fails the comparison
// without this test having to name the field.
func withoutSubstitutions(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	var cp map[string]any
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	delete(cp["metadata"].(map[string]any), "labels")
	c := cp["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	delete(c, "command")
	delete(c, "args")
	return cp
}

func TestTheBenchPodIsTheDriversPodButForTheDeclaredSubstitutions(t *testing.T) {
	in := benchInput(seccompRD)
	got, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := withoutSubstitutions(t, driverPod(t, in)); !reflect.DeepEqual(withoutSubstitutions(t, got), want) {
		g, _ := json.MarshalIndent(withoutSubstitutions(t, got), "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("the bench pod differs from the driver's beyond labels, command and args:\n got: %s\nwant: %s", g, w)
	}
	c := got["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(c["command"], []any{"/bench/inpodbench"}) {
		t.Fatalf("command = %v: the pod would sleep and measure nothing", c["command"])
	}
	if !reflect.DeepEqual(c["args"], toAny(in.Args)) {
		t.Fatalf("args = %v, want %v", c["args"], in.Args)
	}
}

// The posture the bench certifies is the driver's: pin the terms a run's pod
// actually carries, read from the RENDERED pod, so a driver that loosened one
// shows here as well as in its own suite.
func TestTheBenchPodCarriesTheSandboxPosture(t *testing.T) {
	got, err := Render(benchInput(seccompRD))
	if err != nil {
		t.Fatal(err)
	}
	spec := got["spec"].(map[string]any)
	podSec := spec["securityContext"].(map[string]any)
	if podSec["seccompProfile"].(map[string]any)["type"] != seccompRD {
		t.Fatalf("pod seccompProfile = %v, want RuntimeDefault", podSec["seccompProfile"])
	}
	if podSec["runAsNonRoot"] != true {
		t.Fatalf("pod runAsNonRoot = %v", podSec["runAsNonRoot"])
	}
	if spec["automountServiceAccountToken"] != false {
		t.Fatalf("automountServiceAccountToken = %v: a bench pod would hold the namespace's token", spec["automountServiceAccountToken"])
	}
	cSec := spec["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
	if cSec["allowPrivilegeEscalation"] != false {
		t.Fatalf("allowPrivilegeEscalation = %v", cSec["allowPrivilegeEscalation"])
	}
	if !reflect.DeepEqual(cSec["capabilities"], map[string]any{"drop": []any{"ALL"}}) {
		t.Fatalf("capabilities = %v", cSec["capabilities"])
	}
	if _, set := cSec["runAsUser"]; set {
		t.Fatalf("runAsUser = %v for a spec naming no user: the image's USER must run, as in a real run", cSec["runAsUser"])
	}
	if spec["activeDeadlineSeconds"] != float64(900) {
		t.Fatalf("activeDeadlineSeconds = %v: a pod the script fails to delete must stop on its own", spec["activeDeadlineSeconds"])
	}
}

// A bench pod in a runner namespace must be invisible to the runner: the
// orphan reaper lists `iterion.io/managed=true`, and the spread constraint
// counts the component label.
func TestTheBenchPodCarriesNoneOfTheDriversLabels(t *testing.T) {
	got, err := Render(benchInput(seccompRD))
	if err != nil {
		t.Fatal(err)
	}
	labels := got["metadata"].(map[string]any)["labels"].(map[string]any)
	for _, k := range []string{kubernetes.LabelManaged, kubernetes.LabelComponent, kubernetes.LabelRunID, kubernetes.LabelRunName} {
		if _, set := labels[k]; set {
			t.Fatalf("label %s = %v: the runner would take the bench pod for a run's", k, labels[k])
		}
	}
	if labels[labelApp] != appBench || labels[labelRun] != "cm6-bench-1" {
		t.Fatalf("labels = %v: the bench finds and removes its pods by these", labels)
	}
}

// The twin varies the pod's seccomp profile and NOTHING else.
func TestTheUnconfinedTwinVariesOnlyTheSeccompProfile(t *testing.T) {
	measured, err := Render(benchInput(seccompRD))
	if err != nil {
		t.Fatal(err)
	}
	twin, err := Render(benchInput(seccompUnc))
	if err != nil {
		t.Fatal(err)
	}
	if got := twin["spec"].(map[string]any)["securityContext"].(map[string]any)["seccompProfile"]; !reflect.DeepEqual(got, map[string]any{"type": seccompUnc}) {
		t.Fatalf("twin seccompProfile = %v", got)
	}
	twin["spec"].(map[string]any)["securityContext"].(map[string]any)["seccompProfile"] = map[string]any{"type": seccompRD}
	if !reflect.DeepEqual(twin, measured) {
		t.Fatal("the twin differs from the measured pod beyond its seccomp profile, so Seccomp 2 vs 0 attributes to nothing")
	}
}

// The pull policy is replaced only when asked, and then nothing else moves.
func TestThePullPolicyIsReplacedOnlyWhenAsked(t *testing.T) {
	kept, err := Render(benchInput(seccompRD))
	if err != nil {
		t.Fatal(err)
	}
	in := benchInput(seccompRD)
	in.PullPolicy = "Never"
	never, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	c := never["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["imagePullPolicy"] != "Never" {
		t.Fatalf("imagePullPolicy = %v, want Never", c["imagePullPolicy"])
	}
	c["imagePullPolicy"] = kept["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["imagePullPolicy"]
	if !reflect.DeepEqual(never, kept) {
		t.Fatal("replacing the pull policy moved another field")
	}
}

// The kind witness lane loads a TAGGED image no node pulls: accepted there,
// and only there.
func TestATagIsAcceptedOnlyWhenNothingPulls(t *testing.T) {
	in := benchInput(seccompRD)
	in.Image = "codemode-bench-image:local"
	in.PullPolicy = "Never"
	if _, err := Render(in); err != nil {
		t.Fatalf("a tag under pull policy Never was refused: %v", err)
	}
}

func TestRenderRefusesWhatWouldMeasureNothing(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no command":      func(in *Input) { in.Command = nil },
		"no run label":    func(in *Input) { in.RunLabel = "" },
		"a third profile": func(in *Input) { in.Seccomp = "Localhost" },
		"no image":        func(in *Input) { in.Image = "" },
		"a tag, pulled":   func(in *Input) { in.Image = "registry.example/bench:v1" },
	} {
		t.Run(name, func(t *testing.T) {
			in := benchInput(seccompRD)
			mutate(&in)
			if _, err := Render(in); err == nil {
				t.Fatal("rendered a pod that measures nothing")
			}
		})
	}
}
