package docker

import "testing"

func TestParseManagedPsOutput(t *testing.T) {
	raw := "abc123\titerion-run_1\trun_1\n" +
		"def456\titerion-run_2\trun_2\n" +
		"\n" +
		"\t\t\n" + // malformed, drop
		"ghi789\titerion-no-label\t" + // run-id label absent
		"\n"
	got := parseManagedPsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].ID != "abc123" || got[0].Name != "iterion-run_1" || got[0].RunID != "run_1" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[2].ID != "ghi789" || got[2].RunID != "" {
		t.Errorf("entry 2 missing-label case = %+v", got[2])
	}
}

func TestParseManagedPsOutputEmpty(t *testing.T) {
	if got := parseManagedPsOutput(""); len(got) != 0 {
		t.Fatalf("empty input must yield no entries, got %+v", got)
	}
	if got := parseManagedPsOutput("\n\n"); len(got) != 0 {
		t.Fatalf("whitespace-only must yield no entries, got %+v", got)
	}
}

func TestReapOrphanContainersRequiresPredicate(t *testing.T) {
	_, err := CLI{rt: RuntimeDocker, path: "docker"}.ReapOrphanContainers(t.Context(), nil)
	if err == nil {
		t.Fatal("expected error when isTerminal predicate is nil")
	}
}
