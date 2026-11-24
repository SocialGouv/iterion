package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheckpointContextTransitionsPreserveCaller(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		for _, f := range []ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", p, f), func(t *testing.T) {
				s, c := checkpointBindingFixture(t, f, p)
				cp := &Checkpoint{NodeID: "gate", InteractionID: "question-original"}
				if e := s.UpdateRunStatus(c, "r", RunStatusRunning, ""); e != nil {
					t.Fatal(e)
				}
				if e := s.SaveCheckpoint(c, "r", cp); e != nil {
					t.Fatal(e)
				}
				r, e := s.LoadRun(c, "r")
				if e != nil || r.Status != RunStatusRunning || r.Checkpoint.InteractionID != "" || cp.InteractionID != "question-original" {
					t.Fatal("save stripped caller or retained stale pause pointer", r, e, cp)
				}
				if e = s.PauseRun(c, "r", cp); e != nil {
					t.Fatal(e)
				}
				r, e = s.LoadRun(c, "r")
				if e != nil || r.Status != RunStatusPausedWaitingHuman || r.Checkpoint.InteractionID != cp.InteractionID || r.FinishedAt != nil {
					t.Fatal("pause lost live pointer", r, e)
				}
				if e = s.FailRunResumable(c, "r", cp, "interrupted", ""); e != nil {
					t.Fatal(e)
				}
				r, e = s.LoadRun(c, "r")
				if e != nil || r.Status != RunStatusFailedResumable || r.Checkpoint.NodeID != "gate" || r.Checkpoint.InteractionID != "" || r.FinishedAt == nil || r.Error != "interrupted" || cp.InteractionID != "question-original" {
					t.Fatal("failure transition or caller changed", r, e, cp)
				}
				seq := r.OutcomeSeq
				if e = s.FailRunResumable(c, "r", cp, "", ""); e != nil {
					t.Fatal(e)
				}
				r, e = s.LoadRun(c, "r")
				if e != nil || r.OutcomeSeq != seq || r.Error != "interrupted" {
					t.Fatal("repeat failure invents transition", r, e)
				}
				if e = s.UpdateRunStatus(c, "r", RunStatusCancelled, "operator"); e != nil {
					t.Fatal(e)
				}
				before, e := s.LoadRun(c, "r")
				if e != nil {
					t.Fatal(e)
				}
				for _, apply := range []func(context.Context, string, *Checkpoint, string, FailureCode) error{s.FailRunResumable, s.FailRunTerminal} {
					if e = apply(c, "r", &Checkpoint{NodeID: "stale"}, "late", ""); e != nil {
						t.Fatal(e)
					}
					after, e := s.LoadRun(c, "r")
					if e != nil || !reflect.DeepEqual(before, after) {
						t.Fatal("failure defeated operator cancel", e)
					}
				}
			})
		}
	}
}
func TestAttachmentTombstoneContextRejectsMissingAndWrongBinding(t *testing.T) {
	if e := RequireRunDataStorage(IncarnationRunData); e != nil {
		t.Skip(e)
	}
	for _, f := range []ValueFormat{1, 2} {
		s, c := checkpointBindingFixture(t, f, 1)
		if e := os.WriteFile(filepath.Join(s.Root(), "runs", "r", deletionMarkerName), []byte("deleted"), 0600); e != nil {
			t.Fatal(e)
		}
		_, _, e := s.OpenAttachment(context.Background(), "r", "absent")
		if !errors.Is(e, ErrRunDataBindingRequired) {
			t.Fatal("header tombstone adopted unbound context", e)
		}
		other := LegacyValueFormat
		if f == LegacyValueFormat {
			other = ExactValueFormat
		}
		wrong, e := WithRunValueFormat(c, RunValueFormat{RunID: "r", Format: other})
		if e != nil {
			t.Fatal(e)
		}
		_, _, e = s.OpenAttachment(wrong, "r", "absent")
		if !errors.Is(e, ErrValueFormatChanged) && !errors.Is(e, ErrRunDataIdentityChanged) {
			t.Fatal("numeric conflict hidden by tombstone", e)
		}
		_, _, e = s.OpenAttachment(c, "absent", "absent")
		if e == nil || errors.Is(e, ErrAttachmentNotFound) {
			t.Fatal("different run binding adopted", e)
		}
	}
}
