package dryrun

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"testing"
)

func TestPreparedSimulationCancellationKeepsReport(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "cancelled"}[cancelled], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			wf := &ir.Workflow{Name: "prepared-simulation", Entry: "done", Worktree: "none", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
			report, err := Run(ctx, wf, Options{})
			if err != nil || report == nil || len(report.Passes) != 2 {
				t.Fatalf("simulation result lost: %+v %v", report, err)
			}
			for _, pass := range report.Passes {
				if cancelled {
					if !pass.TimedOut || pass.Status != "error" {
						t.Fatalf("cancelled report=%+v", pass)
					}
				} else if pass.Status != "finished" || pass.TimedOut {
					t.Fatalf("normal report=%+v", pass)
				}
			}
		})
	}
}
