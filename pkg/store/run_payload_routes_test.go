package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIncarnationRoutesLifecycleAndValidation(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			missing, err := s.runPayloadPath(ctx, "r", "route_decisions.json")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.FinishRouteDecision(ctx, "r", 1, RouteDecisionSucceeded, ""); err == nil {
				t.Fatal("missing finish accepted")
			}
			if _, err = os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
				t.Fatal("missing finish created namespace", err)
			}
			bad := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			if ok, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 1, FinishedAt: &bad}, time.Time{}); err == nil || ok {
				t.Fatal("invalid new row accepted", ok, err)
			}
			if _, err = os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
				t.Fatal("invalid first claim created namespace", err)
			}
			if ok, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 1, ClaimedAt: bad, Reason: "original"}, time.Time{}); err != nil || !ok {
				t.Fatal("overwritten timestamp rejected", ok, err)
			}
			if ok, row, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 1, FinishedAt: &bad}, time.Time{}); err != nil || ok || row == nil {
				t.Fatal("unused timestamp rejected", ok, row, err)
			}
			if err = s.FinishRouteDecision(ctx, "r", 1, RouteDecisionFailed, "retry"); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 1, FinishedAt: &bad, Reason: "retry"}, time.Time{}); err != nil || !ok {
				t.Fatal("reclaim unused timestamp rejected", ok, err)
			}
			if err = s.FinishRouteDecision(ctx, "r", 1, RouteDecisionSucceeded, ""); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(s.root)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := restarted.ListRouteDecisions(ctx, "r")
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			if rows[0].State != RouteDecisionSucceeded || rows[0].Attempts != 2 || rows[0].Reason != "retry" {
				t.Fatalf("wrong persisted row: %+v", rows[0])
			}
		})
	}
}

func TestIncarnationRoutesRejectDirectoryReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, operation := range []string{"claim", "finish", "list"} {
			t.Run(fmt.Sprintf("%d/%s", format, operation), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				if ok, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 1}, time.Time{}); err != nil || !ok {
					t.Fatal(ok, err)
				}
				key, err := s.runPayloadPath(ctx, "r", "route_decisions.json")
				if err != nil {
					t.Fatal(err)
				}
				parent := filepath.Dir(key)
				if err = os.Rename(parent, parent+"-retired"); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				victim := filepath.Join(outside, "route_decisions.json")
				raw := []byte(`[{"run_id":"r","outcome_seq":1,"state":"claimed"}]`)
				if err = os.WriteFile(victim, raw, 0644); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(outside, parent); err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "claim":
					_, _, err = s.ClaimRouteDecision(ctx, RouteDecision{RunID: "r", OutcomeSeq: 2}, time.Time{})
				case "finish":
					err = s.FinishRouteDecision(ctx, "r", 1, RouteDecisionSucceeded, "")
				case "list":
					_, err = s.ListRouteDecisions(ctx, "r")
				}
				if err == nil {
					t.Fatal("replacement symlink accepted")
				}
				got, e := os.ReadFile(victim)
				if e != nil || string(got) != string(raw) {
					t.Fatal("outside changed", string(got), e)
				}
			})
		}
	}
}
