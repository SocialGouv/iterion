package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func checkpointBindingFixture(t *testing.T, f ValueFormat, p RunDataProtocol) (*FilesystemRunStore, context.Context) {
	t.Helper()
	if p == IncarnationRunData {
		if e := RequireRunDataStorage(p); e != nil {
			t.Skip(e)
		}
	}
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(context.Background(), RunCreateSpec{ID: "r", Format: f, DataProtocol: p})
	if e != nil {
		t.Fatal(e)
	}
	b, e := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: f, Protocol: p, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	c, e := WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	c, e = WithRunValueFormat(c, RunValueFormat{RunID: "r", Format: f})
	if e != nil {
		t.Fatal(e)
	}
	return s, c
}
func TestCheckpointBindingLifecycleAndSuccessor(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		for _, f := range []ValueFormat{1, 2} {
			for _, op := range []string{"save", "pause", "resumable", "terminal"} {
				t.Run(fmt.Sprintf("p%d/f%d/%s", p, f, op), func(t *testing.T) {
					s, c := checkpointBindingFixture(t, f, p)
					apply := func(c context.Context) error {
						cp := &Checkpoint{NodeID: "done"}
						switch op {
						case "save":
							return s.SaveCheckpoint(c, "r", cp)
						case "pause":
							return s.PauseRun(c, "r", cp)
						case "resumable":
							return s.FailRunResumable(c, "r", cp, "failure", "")
						default:
							return s.FailRunTerminal(c, "r", cp, "failure", "")
						}
					}
					cancelled, cancel := context.WithCancel(c)
					cancel()
					if e := apply(cancelled); !errors.Is(e, context.Canceled) {
						t.Fatal("cancel", e)
					}
					before, e := s.LoadRun(c, "r")
					if e != nil || before.Checkpoint != nil {
						t.Fatal("cancel mutated checkpoint", e)
					}
					if e := apply(c); e != nil {
						t.Fatal("original mutation", e)
					}
					r, e := s.LoadRun(c, "r")
					if e != nil || r.Checkpoint == nil || r.Checkpoint.NodeID != "done" {
						t.Fatal("checkpoint missing", e)
					}
					if p == 1 {
						if e = s.DeleteRun(c, "r"); e != nil {
							t.Fatal(e)
						}
						if _, e = s.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); e != nil {
							t.Fatal(e)
						}
						successor, e := s.CreateRunWithValueFormat(context.Background(), RunCreateSpec{ID: "r", Format: f, DataProtocol: p})
						if e != nil {
							t.Fatal(e)
						}
						if e = apply(c); !errors.Is(e, ErrRunDataIdentityChanged) {
							t.Fatal("successor not refused", e)
						}
						sb, _ := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: f, Protocol: p, BirthID: successor.BirthID})
						sc, _ := WithRunDataBinding(context.Background(), sb)
						sc, _ = WithRunValueFormat(sc, RunValueFormat{RunID: "r", Format: f})
						after, e := s.LoadRun(sc, "r")
						if e != nil || after.Checkpoint != nil || after.Status != successor.Status {
							t.Fatal("refusal mutated successor", e)
						}
					}
				})
			}
		}
	}
}
func TestAttachmentTombstoneBindingCompatibility(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		for _, f := range []ValueFormat{1, 2} {
			for _, retained := range []bool{false, true} {
				t.Run(fmt.Sprintf("p%d/f%d/header%v", p, f, retained), func(t *testing.T) {
					s, c := checkpointBindingFixture(t, f, p)
					if retained {
						if e := os.WriteFile(filepath.Join(s.Root(), "runs", "r", deletionMarkerName), []byte("deleted"), 0600); e != nil {
							t.Fatal(e)
						}
					} else {
						if e := s.DeleteRun(c, "r"); e != nil {
							t.Fatal(e)
						}
					}
					_, _, e := s.OpenAttachment(c, "r", "absent")
					if !errors.Is(e, ErrAttachmentNotFound) {
						t.Errorf("original tombstone=%v", e)
					}
					cancelled, cancel := context.WithCancel(c)
					cancel()
					_, _, e = s.OpenAttachment(cancelled, "r", "absent")
					if !errors.Is(e, context.Canceled) {
						t.Errorf("canceled=%v", e)
					}
					if p == 1 && retained {
						b, _ := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: f, Protocol: p, BirthID: "other-birth"})
						other, _ := WithRunDataBinding(context.Background(), b)
						other, _ = WithRunValueFormat(other, RunValueFormat{RunID: "r", Format: f})
						_, _, e = s.OpenAttachment(other, "r", "absent")
						if !errors.Is(e, ErrRunDataIdentityChanged) {
							t.Errorf("foreign birth tombstone=%v", e)
						}
					}
				})
			}
		}
	}
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = s.OpenAttachment(context.Background(), "absent", "absent")
	if !errors.Is(e, ErrAttachmentNotFound) {
		t.Fatal("legacy absence", e)
	}
}
