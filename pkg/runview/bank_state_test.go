package runview

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestBankStateOnRunReadModels(t *testing.T) {
	for _, failed := range []bool{false, true} {
		r := &store.Run{ID: "bank", Status: store.RunStatusFinished, FinalCommit: "abc", FinalBranch: "iterion/run-bank"}
		want := "banked"
		if failed {
			r.FinalBranch = ""
			r.FinalBranchError = "push exit 12"
			want = "bank_failed"
		}
		for _, view := range []any{headerFromRun(r), summarizeRun(r, false)} {
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got["bank_state"] != want || got["status"] != "finished" || got["final_commit"] != "abc" {
				t.Fatalf("view=%s", raw)
			}
		}
	}
}
