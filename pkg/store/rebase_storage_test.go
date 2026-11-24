package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Prepared births use the common creator directly, bypassing CreateChildRun.
func TestPreparedBirthPreservesParentProvenance(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s := tmpStore(t)
			ctx := t.Context()
			parent, err := s.CreateRun(ctx, "parent", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			parent.Trust = RunTrustFork
			parent.RepoSHAExpected = "deadbeef"
			parent.ForkAuthorSubject = "author"
			if err := s.SaveRun(ctx, parent); err != nil {
				t.Fatal(err)
			}
			_, err = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "child", ParentRunID: "parent", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.LoadRun(ctx, "child")
			if err != nil {
				t.Fatal(err)
			}
			if child.Trust != parent.Trust || child.RepoSHAExpected != parent.RepoSHAExpected || child.ForkAuthorSubject != parent.ForkAuthorSubject {
				t.Fatalf("provenance lost: %+v", child)
			}
			_, err = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "orphan", ParentRunID: "missing", Format: format})
			if !errors.Is(err, ErrChildParentUnreadable) || !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("unreadable parent: %v", err)
			}
			if _, err := s.LoadRun(ctx, "orphan"); !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("refusal wrote child: %v", err)
			}
		})
	}
}

func TestSettleCredSlotAdmissionPreservesFormatBoundary(t *testing.T) {
	for _, scenario := range []string{"known", "future", "cancelled", "stale-flip"} {
		t.Run(scenario, func(t *testing.T) {
			s := tmpStore(t)
			ctx := t.Context()
			r, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: ExactValueFormat, Status: RunStatusQueued})
			if err != nil {
				t.Fatal(err)
			}
			r.CredAdmissionPending = true
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.runJSONPath("r"))
			if err != nil {
				t.Fatal(err)
			}
			at := *r.QueuedAt
			var want error
			switch scenario {
			case "future":
				raw = []byte(strings.Replace(string(raw), `"format_version": 2`, `"format_version": 99`, 1))
				if err := os.WriteFile(s.runJSONPath("r"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				want = ErrUnsupportedValueFormat
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "stale-flip":
				at = at.Add(1)
			}
			err = s.SettleCredSlotAdmission(ctx, "r", at)
			if !errors.Is(err, want) {
				t.Fatalf("settle: %v want %v", err, want)
			}
			after, err := os.ReadFile(s.runJSONPath("r"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "known" {
				if !bytes.Equal(raw, after) {
					t.Fatal("refusal or stale flip mutated metadata")
				}
			} else {
				got, err := s.LoadRun(t.Context(), "r")
				if err != nil {
					t.Fatal(err)
				}
				if got.CredAdmissionPending || got.FormatVersion != 2 {
					t.Fatalf("settled incorrectly: %+v", got)
				}
			}
		})
	}
}

func TestDeleteRunCancellationPreservesCredentialLedger(t *testing.T) {
	for _, cancelIt := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelIt), func(t *testing.T) {
			s := tmpStore(t)
			if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			slot, err := s.ReserveCredSlot(t.Context(), "key", "r", 1)
			if err != nil || !slot.Granted {
				t.Fatalf("reserve %v %v", slot, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelIt {
				cancel()
			}
			err = s.DeleteRun(ctx, "r")
			if cancelIt {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("delete canceled: %v", err)
				}
				if _, err = s.LoadRun(t.Context(), "r"); err != nil {
					t.Fatalf("cancel deleted run: %v", err)
				}
				// The unsuccessful deletion must not drop its reserved slot.
				if len(s.credSlots["key"]) != 1 {
					t.Fatal("cancel dropped ledger")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.LoadRun(t.Context(), "r"); !errors.Is(err, ErrRunDeleted) {
					t.Fatalf("delete: %v", err)
				}
				if len(s.credSlots["key"]) != 0 {
					t.Fatal("delete leaked ledger")
				}
			}
		})
	}
}

func TestSettleCredSlotAdmissionPreservesOriginalBinding(t *testing.T) {
	s := tmpStore(t)
	r, err := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: ExactValueFormat, Status: RunStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	r.CredAdmissionPending = true
	if err = s.SaveRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.runJSONPath("r"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	err = s.SettleCredSlotAdmission(ctx, "r", *r.QueuedAt)
	if !errors.Is(err, ErrValueFormatChanged) {
		t.Fatalf("original binding not enforced: %v", err)
	}
	after, err := os.ReadFile(s.runJSONPath("r"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("refusal mutated metadata")
	}
}
