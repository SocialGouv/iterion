package store

import "testing"

func TestBankStateUsesDurableFieldsWithoutChangingWorkflowOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *Run
		want BankState
	}{
		{"absent", nil, ""}, {"workless", &Run{Status: RunStatusFinished}, ""},
		{"unconfirmed commit", &Run{FinalCommit: "abc"}, ""},
		{"unconfirmed branch", &Run{FinalBranch: "work"}, ""},
		{"ready", &Run{Status: RunStatusFinished, FinalCommit: "abc", FinalBranch: "work"}, BankStateReady},
		{"failed push", &Run{Status: RunStatusFinished, FinalCommit: "abc", FinalBranchError: "exit 12"}, BankStateFailed},
		{"error authoritative", &Run{FinalCommit: "abc", FinalBranch: "work", FinalBranchError: "cannot verify"}, BankStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run.BankState(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
