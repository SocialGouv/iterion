package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSchedulingFromEnvDefaultsToNodeSpreadAndNoResources(t *testing.T) {
	s, err := schedulingFromEnv(envOf(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.resources != (PodResources{}) {
		t.Fatalf("resources must be unset by default, got %+v", s.resources)
	}
	if s.spreadKey != defaultSpreadTopologyKey {
		t.Fatalf("spread key = %q, want %q by default", s.spreadKey, defaultSpreadTopologyKey)
	}
}

func TestSchedulingFromEnvParsesQuantities(t *testing.T) {
	s, err := schedulingFromEnv(envOf(map[string]string{
		RequestsCPUEnvVar:    " 2 ",
		RequestsMemoryEnvVar: "4Gi",
		LimitsCPUEnvVar:      "1.5",
		LimitsMemoryEnvVar:   "1e3",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := PodResources{
		Requests: ResourceList{CPU: "2", Memory: "4Gi"},
		Limits:   ResourceList{CPU: "1.5", Memory: "1e3"},
	}
	if s.resources != want {
		t.Fatalf("resources = %+v, want %+v", s.resources, want)
	}
	for _, v := range []string{"500m", "128Mi", "3", "0.25", "2Ti", "100k"} {
		if _, err := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: v})); err != nil {
			t.Errorf("%q must be accepted as a quantity: %v", v, err)
		}
	}
}

// A typo must fail with the variable and the value named, never be rendered
// for the API server to reject on every pod.
func TestSchedulingFromEnvRejectsMalformedQuantities(t *testing.T) {
	for _, v := range []string{"2 cores", "4GB", "-1", "1,5", "Gi", "two"} {
		_, err := schedulingFromEnv(envOf(map[string]string{LimitsMemoryEnvVar: v}))
		if err == nil {
			t.Errorf("%q must be refused", v)
			continue
		}
		if !strings.Contains(err.Error(), LimitsMemoryEnvVar) || !strings.Contains(err.Error(), v) {
			t.Errorf("error for %q must name the variable and the value, got %q", v, err)
		}
	}
}

func TestSchedulingFromEnvSpreadSelector(t *testing.T) {
	cases := map[string]string{
		"":                            defaultSpreadTopologyKey,
		"hostname":                    defaultSpreadTopologyKey,
		"none":                        "",
		"off":                         "",
		"topology.kubernetes.io/zone": "topology.kubernetes.io/zone",
	}
	for v, want := range cases {
		s, err := schedulingFromEnv(envOf(map[string]string{SpreadEnvVar: v}))
		if err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
			continue
		}
		if s.spreadKey != want {
			t.Errorf("%q: spread key = %q, want %q", v, s.spreadKey, want)
		}
	}
	if _, err := schedulingFromEnv(envOf(map[string]string{SpreadEnvVar: "not a key"})); err == nil || !strings.Contains(err.Error(), SpreadEnvVar) {
		t.Fatalf("a key with whitespace must be refused naming the variable, got %v", err)
	}
}

// The factory's preference walk skips any constructor error and lands on the
// noop driver, so a malformed value must fail each run at Start instead —
// loudly, with the variable named, before any kubectl call.
func TestStartRefusesWhenSchedulingIsMalformed(t *testing.T) {
	_, schedErr := schedulingFromEnv(envOf(map[string]string{RequestsCPUEnvVar: "two"}))
	if schedErr == nil {
		t.Fatal("precondition: malformed quantity must produce an error")
	}
	d := &Driver{namespace: "test", kubectl: "/nonexistent/kubectl", schedErr: schedErr}
	_, err := d.Start(context.Background(), &Prepared{spec: sandbox.Spec{Image: "img", User: "1000:1000"}, workspace: "/workspace"}, sandbox.RunInfo{RunID: "r"})
	if err == nil || !strings.Contains(err.Error(), RequestsCPUEnvVar) {
		t.Fatalf("Start must surface the scheduling error naming %s, got %v", RequestsCPUEnvVar, err)
	}
}
