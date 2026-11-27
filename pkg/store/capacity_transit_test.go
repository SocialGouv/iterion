package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The holder predicate, arm by arm: alive statuses hold; a failed_resumable
// run holds only in its redelivery transit, strictly before its instant; no
// other continuation, no paused or ended status holds.
func TestHoldsCapacity(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { u := now.Add(d); return &u }
	transit := func(c ContinuationState, until *time.Time) *Run {
		return &Run{Status: RunStatusFailedResumable, ContinuationState: c, ContinuationUntil: until}
	}
	cases := []struct {
		name  string
		run   *Run
		holds bool
	}{
		{"queued", &Run{Status: RunStatusQueued}, true},
		{"running", &Run{Status: RunStatusRunning}, true},
		{"transit ahead of now", transit(ContinuationRedeliveryPending, at(time.Minute)), true},
		{"transit a millisecond ahead", transit(ContinuationRedeliveryPending, at(time.Millisecond)), true},
		{"transit at its instant", transit(ContinuationRedeliveryPending, at(0)), false},
		{"transit past its instant", transit(ContinuationRedeliveryPending, at(-time.Minute)), false},
		{"redelivery pending with no instant", transit(ContinuationRedeliveryPending, nil), false},
		{"retry armed with an instant ahead", transit(ContinuationRetryArmed, at(time.Minute)), false},
		{"final with an instant ahead", transit(ContinuationFinal, at(time.Minute)), false},
		{"unknown continuation with an instant ahead", transit("", at(time.Minute)), false},
		{"paused operator with a transit", &Run{Status: RunStatusPausedOperator, ContinuationState: ContinuationRedeliveryPending, ContinuationUntil: at(time.Minute)}, false},
		{"paused waiting human", &Run{Status: RunStatusPausedWaitingHuman}, false},
		{"finished", &Run{Status: RunStatusFinished}, false},
		{"cancelled with a transit", &Run{Status: RunStatusCancelled, ContinuationState: ContinuationRedeliveryPending, ContinuationUntil: at(time.Minute)}, false},
		{"nil run", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.run.HoldsCapacity(now); got != c.holds {
				t.Fatalf("HoldsCapacity = %v, want %v", got, c.holds)
			}
			stamped := c.run
			if stamped != nil {
				cp := *stamped
				cp.CredFingerprints = []string{"fp"}
				stamped = &cp
			}
			if got := stamped.SpendsCredential("fp", now); got != c.holds {
				t.Fatalf("SpendsCredential = %v, want %v (a holder stamped with the key)", got, c.holds)
			}
		})
	}
}

// The transit's length, read on runners only: a Go duration, strictly
// positive, an error for anything else, and a warning — not a refusal — below
// the floor the runner's own re-offers need. Set but empty (a manifest that
// renders an optional value as "") is unset: the default, not a boot stop.
func TestParseRedeliveryHoldTTL(t *testing.T) {
	cases := []struct {
		raw     string
		set     bool
		want    time.Duration
		warns   bool
		refused bool
	}{
		{"", false, DefaultRedeliveryHoldTTL, false, false},
		{"10m", true, 10 * time.Minute, false, false},
		{" 15m ", true, 15 * time.Minute, false, false},
		{"7m", true, 7 * time.Minute, false, false},
		{"3m", true, 3 * time.Minute, true, false},
		{"90s", true, 90 * time.Second, true, false},
		{"0", true, 0, false, true},
		{"0s", true, 0, false, true},
		{"-1m", true, 0, false, true},
		{"x", true, 0, false, true},
		{"600", true, 0, false, true},
		{"", true, DefaultRedeliveryHoldTTL, false, false},
		{"   ", true, DefaultRedeliveryHoldTTL, false, false},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, warn, err := ParseRedeliveryHoldTTL(c.raw, c.set)
			if c.refused {
				if err == nil {
					t.Fatalf("%q accepted as %s, want an error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("%q = %s, want %s", c.raw, got, c.want)
			}
			if (warn != "") != c.warns {
				t.Fatalf("%q: warning %q, want warns=%v", c.raw, warn, c.warns)
			}
		})
	}
}

// Class guard: the status-only holder rule is read through HoldsCapacity and
// the Mongo holder branches alone. A reader that calls HoldsCredentialSlot
// directly answers "not a holder" for a run in transit, and the ledger's
// readings stop agreeing.
func TestHoldsCredentialSlotIsReadOnlyThroughTheHolderPredicate(t *testing.T) {
	allowed := map[string]bool{"HoldsCapacity": true, "holdsCapacityBranches": true}
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"pkg", "cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "vendor" || name == "testdata" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), "HoldsCredentialSlot()") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "HoldsCredentialSlot" && !allowed[fn.Name.Name] {
						offenders = append(offenders, fset.Position(call.Pos()).String()+" in "+fn.Name.Name)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("HoldsCredentialSlot read outside HoldsCapacity and holdsCapacityBranches — a run in transit reads as no holder there:\n%s", strings.Join(offenders, "\n"))
	}
}

// The step order of EnterCapacityTransit: the transit write, then the seam,
// then the re-assertions in message order; a lost hold releases the ones
// re-asserted before it by their new instants and stops.
func TestEnterCapacityTransitOrder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := t.Context()
	credSlotRun(t, s, "enter", RunStatusPausedOperator, "fp-1", "fp-2")
	first, err := s.ReserveCredSlot(ctx, "fp-1", "enter", 2)
	if err != nil || !first.Granted {
		t.Fatalf("admission fp-1: %+v %v", first, err)
	}
	second, err := s.ReserveCredSlot(ctx, "fp-2", "enter", 2)
	if err != nil || !second.Granted {
		t.Fatalf("admission fp-2: %+v %v", second, err)
	}
	r, err := s.LoadRun(ctx, "enter")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Status = RunStatusFailedResumable
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	until := TransitInstant(time.Now().Add(5 * time.Minute))
	holds := []CapacityHold{{Subject: "fp-1", Token: first.At}, {Subject: "fp-2", Token: second.At}}

	var seenInTransit bool
	seamCtx := WithCapacityTransitStepSeam(ctx, func() {
		got, err := s.LoadRun(ctx, "enter")
		if err != nil {
			t.Errorf("LoadRun in the seam: %v", err)
			return
		}
		seenInTransit = got.InCapacityTransit(time.Now())
		// Between the steps nothing is re-asserted yet: the entry still
		// carries the admission's own instant.
		s.credSlotMu.Lock()
		defer s.credSlotMu.Unlock()
		if h := s.credSlots["fp-1"][0]; !h.at.Equal(first.At) {
			t.Errorf("fp-1 entry at %s in the seam, want the admission's %s — the re-assertion ran before the transit write", h.at, first.At)
		}
	})
	entry, err := EnterCapacityTransit(seamCtx, s, s, "enter", holds, CapacityTransit{Until: until})
	if err != nil {
		t.Fatalf("EnterCapacityTransit: %v", err)
	}
	if !seenInTransit {
		t.Fatal("the seam ran before the transit write")
	}
	if !entry.Started || entry.Lost != "" || len(entry.Held) != 2 {
		t.Fatalf("entry %+v, want started with both holds held", entry)
	}
	for i, h := range entry.Held {
		if h.Subject != holds[i].Subject || !h.At.After(holds[i].Token) {
			t.Fatalf("held[%d] = %+v, want %s moved past its token %s", i, h, holds[i].Subject, holds[i].Token)
		}
	}

	// fp-2's entry is gone: fp-1, re-asserted first, is given back by its new
	// instant, and fp-2 is named lost.
	if err := s.ReleaseCredSlot(ctx, "fp-2", "enter", entry.Held[1].At); err != nil {
		t.Fatalf("release fp-2: %v", err)
	}
	again, err := EnterCapacityTransit(ctx, s, s, "enter", holds, CapacityTransit{Until: until.Add(time.Minute), ExpectUntil: until})
	if err != nil {
		t.Fatalf("EnterCapacityTransit: %v", err)
	}
	if !again.Started || again.Lost != "fp-2" || len(again.Held) != 0 {
		t.Fatalf("entry %+v, want started with fp-2 lost and nothing held", again)
	}
	s.credSlotMu.Lock()
	left := len(s.credSlots["fp-1"])
	s.credSlotMu.Unlock()
	if left != 0 {
		t.Fatalf("fp-1 keeps %d entries after its hold was released by its new instant, want 0", left)
	}
}

// An adoption writes no admission identity on the entry it adds; the
// admission's own entry carries the instant it returned.
func TestAdoptedEntryCarriesNoAdmissionIdentity(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := t.Context()
	credSlotRun(t, s, "adopted", RunStatusRunning, "fp-id")
	credSlotRun(t, s, "asker", RunStatusPausedOperator, "fp-id")
	adm, err := s.ReserveCredSlot(ctx, "fp-id", "asker", 3)
	if err != nil || !adm.Granted {
		t.Fatalf("admission: %+v %v", adm, err)
	}
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	for _, h := range s.credSlots["fp-id"] {
		switch h.runID {
		case "adopted":
			if !h.admittedAt.IsZero() {
				t.Fatalf("adopted entry carries admitted_at %s, want none", h.admittedAt)
			}
		case "asker":
			if !h.admittedAt.Equal(adm.At) {
				t.Fatalf("admitted entry admitted_at %s, want its admission's %s", h.admittedAt, adm.At)
			}
		}
	}
}
