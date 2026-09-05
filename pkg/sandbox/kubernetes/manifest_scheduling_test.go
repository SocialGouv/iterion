package kubernetes

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func renderPodSpec(t *testing.T, in PodManifestInput) map[string]any {
	t.Helper()
	raw, err := BuildPodManifest(in)
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	var pod struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return pod.Spec
}

func schedulingInput() PodManifestInput {
	return PodManifestInput{
		Namespace: "iterion",
		Name:      "iterion-run-test",
		RunID:     "test",
		Spec:      sandbox.Spec{Image: "ghcr.io/x/sandbox:edge", User: "1000:1000"},
	}
}

func workloadContainer(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	containers, _ := spec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("want exactly one container, got %v", spec["containers"])
	}
	c, _ := containers[0].(map[string]any)
	return c
}

// A pod that requests nothing scores every node the same — the manifest must
// carry exactly the quantities the operator set, and nothing it did not.
func TestManifestRendersOnlySetResources(t *testing.T) {
	in := schedulingInput()
	in.Resources = PodResources{
		Requests: ResourceList{CPU: "2", Memory: "4Gi"},
		Limits:   ResourceList{Memory: "8Gi"},
	}
	c := workloadContainer(t, renderPodSpec(t, in))
	want := map[string]any{
		"requests": map[string]any{"cpu": "2", "memory": "4Gi"},
		"limits":   map[string]any{"memory": "8Gi"},
	}
	if got := c["resources"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %v, want %v", got, want)
	}
}

func TestManifestOmitsResourcesWhenNothingSet(t *testing.T) {
	c := workloadContainer(t, renderPodSpec(t, schedulingInput()))
	if _, present := c["resources"]; present {
		t.Fatalf("resources key must be absent when nothing is set, got %v", c["resources"])
	}
	in := schedulingInput()
	in.Resources = PodResources{Limits: ResourceList{CPU: "4"}}
	c = workloadContainer(t, renderPodSpec(t, in))
	res, _ := c["resources"].(map[string]any)
	if _, present := res["requests"]; present {
		t.Fatalf("an unset requests side must be absent, got %v", res)
	}
	if !reflect.DeepEqual(res["limits"], map[string]any{"cpu": "4"}) {
		t.Fatalf("limits = %v, want cpu 4 only", res["limits"])
	}
}

// The spread must select the sandbox-run component label — every run pod of
// the deployment, and only them — and stay soft so it never blocks scheduling.
// The pod itself must carry that label: a selector matching no pod counts
// zero in every domain, and the constraint steers nothing without a single
// Kubernetes error.
func TestManifestSpreadConstraintOverSandboxRunPods(t *testing.T) {
	in := schedulingInput()
	in.SpreadTopologyKey = "kubernetes.io/hostname"
	raw, err := BuildPodManifest(in)
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	var pod struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if pod.Metadata.Labels[LabelComponent] != ComponentSandboxRun {
		t.Fatalf("the pod must carry the label its own spread selector matches, labels = %v", pod.Metadata.Labels)
	}
	spec := renderPodSpec(t, in)
	want := []any{
		map[string]any{
			"maxSkew":           float64(1),
			"topologyKey":       "kubernetes.io/hostname",
			"whenUnsatisfiable": "ScheduleAnyway",
			"labelSelector": map[string]any{
				"matchLabels": map[string]any{LabelComponent: ComponentSandboxRun},
			},
		},
	}
	if got := spec["topologySpreadConstraints"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("topologySpreadConstraints = %v, want %v", got, want)
	}
}

func TestManifestNoSpreadWithoutTopologyKey(t *testing.T) {
	spec := renderPodSpec(t, schedulingInput())
	if _, present := spec["topologySpreadConstraints"]; present {
		t.Fatalf("topologySpreadConstraints must be absent without a key, got %v", spec["topologySpreadConstraints"])
	}
}
