package kubernetes

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// ownerRefsOf extracts metadata.ownerReferences from a rendered manifest,
// or nil when the field is absent.
func ownerRefsOf(t *testing.T, manifest []byte) []map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(manifest, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	raw, ok := meta["ownerReferences"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("ownerReferences is %T, want []any", raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("ownerReference entry is %T, want map", e)
		}
		out = append(out, m)
	}
	return out
}

func assertOwnsRunnerPod(t *testing.T, refs []map[string]any) {
	t.Helper()
	if len(refs) != 1 {
		t.Fatalf("ownerReferences len = %d, want 1: %v", len(refs), refs)
	}
	r := refs[0]
	if r["kind"] != "Pod" || r["apiVersion"] != "v1" {
		t.Errorf("owner kind/apiVersion = %v/%v, want Pod/v1", r["kind"], r["apiVersion"])
	}
	if r["name"] != "iterion-runner-abc" {
		t.Errorf("owner name = %v, want iterion-runner-abc", r["name"])
	}
	if r["uid"] != "runner-uid-123" {
		t.Errorf("owner uid = %v, want runner-uid-123", r["uid"])
	}
}

var testOwner = &OwnerReference{APIVersion: "v1", Kind: "Pod", Name: "iterion-runner-abc", UID: "runner-uid-123"}

func TestPodManifest_OwnerReferenceAndDeadline(t *testing.T) {
	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace:             "iterion",
		Name:                  "iterion-run-x",
		RunID:                 "x",
		Spec:                  sandbox.Spec{Mode: sandbox.ModeInline, Image: "alpine:3"},
		Owner:                 testOwner,
		ActiveDeadlineSeconds: 7200,
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	assertOwnsRunnerPod(t, ownerRefsOf(t, manifest))

	var pod map[string]any
	if err := json.Unmarshal(manifest, &pod); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec := pod["spec"].(map[string]any)
	// JSON numbers unmarshal as float64.
	got, ok := spec["activeDeadlineSeconds"].(float64)
	if !ok {
		t.Fatalf("activeDeadlineSeconds missing or wrong type: %v", spec["activeDeadlineSeconds"])
	}
	if int64(got) != 7200 {
		t.Errorf("activeDeadlineSeconds = %d, want 7200", int64(got))
	}
}

func TestPodManifest_NoOwnerNoDeadlineByDefault(t *testing.T) {
	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace: "iterion",
		Name:      "iterion-run-x",
		RunID:     "x",
		Spec:      sandbox.Spec{Mode: sandbox.ModeInline, Image: "alpine:3"},
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	if refs := ownerRefsOf(t, manifest); refs != nil {
		t.Errorf("expected no ownerReferences, got %v", refs)
	}
	var pod map[string]any
	if err := json.Unmarshal(manifest, &pod); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec := pod["spec"].(map[string]any)
	if _, ok := spec["activeDeadlineSeconds"]; ok {
		t.Errorf("expected no activeDeadlineSeconds, got %v", spec["activeDeadlineSeconds"])
	}
}

func TestCASecret_OwnerReference(t *testing.T) {
	out, err := BuildCASecret("ns", "iterion-run-x-ca", "x", "friendly", testCAPEM(t), testOwner)
	if err != nil {
		t.Fatalf("BuildCASecret: %v", err)
	}
	assertOwnsRunnerPod(t, ownerRefsOf(t, out))
}

func TestSecretFilesSecret_OwnerReference(t *testing.T) {
	out, err := BuildSecretFilesSecret("ns", "iterion-run-x-secret-files", "x", "friendly",
		[]sandbox.SecretFileMount{{Name: "creds", Value: []byte("secret")}}, testOwner)
	if err != nil {
		t.Fatalf("BuildSecretFilesSecret: %v", err)
	}
	assertOwnsRunnerPod(t, ownerRefsOf(t, out))
}

func TestNetworkPolicy_OwnerReference(t *testing.T) {
	out, err := BuildNetworkPolicy(NetworkPolicyInput{
		Namespace:   "ns",
		Name:        "iterion-run-x",
		RunID:       "x",
		RunnerPodIP: "10.0.0.5",
		Owner:       testOwner,
	})
	if err != nil {
		t.Fatalf("BuildNetworkPolicy: %v", err)
	}
	assertOwnsRunnerPod(t, ownerRefsOf(t, out))
}

func TestOwnerReference_PartialIsIgnored(t *testing.T) {
	// A partial owner (missing UID) must NOT emit an invalid ownerReference —
	// k8s rejects an ownerReference without a uid, so we omit it entirely.
	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace: "ns",
		Name:      "n",
		RunID:     "x",
		Spec:      sandbox.Spec{Mode: sandbox.ModeInline, Image: "alpine:3"},
		Owner:     &OwnerReference{Name: "runner-only", UID: ""},
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	if refs := ownerRefsOf(t, manifest); refs != nil {
		t.Errorf("partial owner should be ignored, got %v", refs)
	}
}

func TestActiveDeadlineFor(t *testing.T) {
	if got := activeDeadlineFor(0); got != 0 {
		t.Errorf("activeDeadlineFor(0) = %d, want 0 (unbounded)", got)
	}
	if got := activeDeadlineFor(-5); got != 0 {
		t.Errorf("activeDeadlineFor(-5) = %d, want 0", got)
	}
	if got := activeDeadlineFor(3600); got != 3600+deadlineMarginSecs {
		t.Errorf("activeDeadlineFor(3600) = %d, want %d", got, 3600+deadlineMarginSecs)
	}
}

func TestRunnerPodOwner_FromEnv(t *testing.T) {
	t.Setenv(RunnerPodNameEnvVar, "iterion-runner-abc")
	t.Setenv(RunnerPodUIDEnvVar, "runner-uid-123")
	owner := runnerPodOwner()
	if owner == nil {
		t.Fatal("expected an owner when both env vars are set")
	}
	if owner.Name != "iterion-runner-abc" || owner.UID != "runner-uid-123" {
		t.Errorf("owner = %+v", owner)
	}

	t.Setenv(RunnerPodUIDEnvVar, "")
	if runnerPodOwner() != nil {
		t.Error("expected nil owner when UID env is empty")
	}
}
