package runview

import "testing"

type runFilesReadOnlyRuns struct{ RunLoader }

func TestRunFilesBuildGrantCapabilityVariants(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		spec, s := runFilesViewSpec(t, true)
		spec.Store = s
		spec.Runs = nil
		if readOnly {
			spec.Runs = runFilesReadOnlyRuns{s.base}
		}
		e, err := BuildExecutor(spec)
		if err != nil {
			t.Fatalf("legitimate store variant refused: %v", err)
		}
		if e.RunFilesGrant() != s.grant || s.opens != 1 {
			t.Fatal("variant lost grant")
		}
		if err = e.Close(); err != nil {
			t.Fatal(err)
		}
		if s.grant.closed != 1 {
			t.Fatal("variant owner not closed")
		}
	}
}
