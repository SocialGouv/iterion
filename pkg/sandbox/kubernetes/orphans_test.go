package kubernetes

import "testing"

func TestParseManagedList(t *testing.T) {
	data := []byte(`{
	  "items": [
	    {"metadata": {"name": "iterion-run-a", "labels": {"iterion.io/managed": "true", "iterion.io/run-id": "run-a"}}},
	    {"metadata": {"name": "iterion-run-b", "labels": {"iterion.io/managed": "true"}}},
	    {"metadata": {"name": "", "labels": {}}}
	  ]
	}`)
	got, err := parseManagedList("pod", data)
	if err != nil {
		t.Fatalf("parseManagedList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (empty-name item dropped): %+v", len(got), got)
	}
	if got[0].Kind != "pod" || got[0].Name != "iterion-run-a" || got[0].RunID != "run-a" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].RunID != "" {
		t.Errorf("got[1].RunID = %q, want empty (missing label)", got[1].RunID)
	}
}

func TestParseManagedList_Empty(t *testing.T) {
	got, err := parseManagedList("secret", []byte(`{"items": []}`))
	if err != nil {
		t.Fatalf("parseManagedList: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}

func TestParseManagedList_BadJSON(t *testing.T) {
	if _, err := parseManagedList("pod", []byte("not json")); err == nil {
		t.Fatal("expected error on bad JSON")
	}
}

func TestReapOrphanResources_RequiresArgs(t *testing.T) {
	if _, err := ReapOrphanResources(nil, "", func(string) bool { return true }); err == nil {
		t.Error("expected error on empty namespace")
	}
	if _, err := ReapOrphanResources(nil, "ns", nil); err == nil {
		t.Error("expected error on nil predicate")
	}
}

func TestReapKindsCoverAllPerRunResources(t *testing.T) {
	// Regression guard: every per-run resource type the driver creates
	// (pod, CA/file-secrets Secret, NetworkPolicy) must be in the sweep,
	// since none is owned by the sandbox pod (owner is the runner pod) and
	// so none cascades off deleting the pod.
	want := map[string]bool{"pod": true, "secret": true, "networkpolicy": true}
	if len(reapKinds) != len(want) {
		t.Fatalf("reapKinds = %v, want kinds %v", reapKinds, want)
	}
	for _, k := range reapKinds {
		if !want[k] {
			t.Errorf("unexpected reap kind %q", k)
		}
	}
}
