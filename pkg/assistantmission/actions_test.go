package assistantmission

import "testing"

func TestParseProposalsRejectsWholeMalformedList(t *testing.T) {
	_, err := ParseProposals([]any{
		map[string]any{"id": ActionResume, "args": map[string]any{"run_id": "r"}},
		map[string]any{"id": ActionRewind, "surprise": true},
	})
	if err == nil {
		t.Fatal("ParseProposals accepted a partially malformed action list")
	}
}

func TestParseProposalsAcceptsStringifiedGenericContract(t *testing.T) {
	got, err := ParseProposals(`[{"id":"run.resume","intent":"suggested","args":{"run_id":"r"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ActionResume || got[0].Args["run_id"] != "r" {
		t.Fatalf("unexpected proposals: %#v", got)
	}
}

func TestValidateSingleSupportedEnforcesAllowlistAndCardinality(t *testing.T) {
	allowed := map[string]bool{ActionResume: true}
	if _, err := ValidateSingleSupported([]Proposal{{ID: ActionRewind}}, allowed); err == nil {
		t.Fatal("out-of-policy rewind accepted")
	}
	if _, err := ValidateSingleSupported([]Proposal{{ID: ActionResume}, {ID: ActionResume}}, allowed); err == nil {
		t.Fatal("multiple actions accepted")
	}
}
