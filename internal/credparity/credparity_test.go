package credparity

import (
	"slices"
	"strings"
	"testing"
)

// The table is well-formed: every row is named once, says where it comes
// from, walks a chain its Want can index, and sits where a run of its trust
// can — a fork-lane run is always sandboxed (the PR2b floor). A chain the
// run's allowlist emptied walks no leg, and its one answer is the allowlist's
// refusal, which the dispatch meets.
func TestEveryRowIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range append(Rows(), RecoveryRows()...) {
		if r.Name == "" || r.Origin == "" {
			t.Errorf("row %q: a row names itself and its origin", r.Name)
		}
		if seen[r.Name] {
			t.Errorf("row %q appears twice", r.Name)
		}
		seen[r.Name] = true
		if len(r.Node.Legs) == 0 && (len(r.Run.Allowlist) == 0 || r.Want.Refusal != OutsideAllowlist) {
			t.Errorf("row %q: the node walks no leg, and no allowlist refusal stands for its emptied chain", r.Name)
		}
		if !r.Want.Refused() && (r.Want.Leg < 0 || r.Want.Leg >= len(r.Node.Legs)) {
			t.Errorf("row %q: Want spends leg %d of a %d-leg chain", r.Name, r.Want.Leg, len(r.Node.Legs))
		}
		if r.Trust == Fork && !r.Sandboxed {
			t.Errorf("row %q: a fork-lane run is never unsandboxed", r.Name)
		}
		if r.Trust == "" || r.Role == "" {
			t.Errorf("row %q: trust %q role %q — both are stated", r.Name, r.Trust, r.Role)
		}
	}
}

// Every row's dispatch is driven by one harness: a row on a backend no
// harness drives would be judged by the readers above the dispatch alone.
func TestEveryRowHasADispatchHarness(t *testing.T) {
	for _, r := range Rows() {
		if DispatchHarness(r.Node.Backend) == "" {
			t.Errorf("row %q: no dispatch harness drives backend %q", r.Name, r.Node.Backend)
		}
		if r.Role == SupervisorRole && r.Node.Backend != "claw" {
			t.Errorf("row %q: a supervisor evaluates on claw, in process — not %q", r.Name, r.Node.Backend)
		}
	}
}

// U0–U2 land as one tip (ADR-161 decision 9): every divergence they pinned is
// closed, so no row carries a Pending entry — every reader is judged against
// the row's Want. A later unit that pins a reader again relaxes this test.
func TestNoRowIsPending(t *testing.T) {
	for _, r := range Rows() {
		for _, p := range r.Pending {
			t.Errorf("row %q: %s pending on %q (%s) — U0–U2 land as one tip, and no red row lands with them", r.Name, p.Reader, p.Unit, p.Why)
		}
	}
}

// The rows ADR-161 (decision 9, U0) says are seen red on main are in the
// table and judge their reader against Want: U1 greened the admission's, U2
// the meter's, the ledger's and the plan's.
func TestTheSeenRedRowsAreJudged(t *testing.T) {
	for _, want := range []struct {
		origin string
		reader Reader
	}{
		{"t50 r2 #1", Meter},
		{"t50 r2 #1", Admission},
		{"t50 r2 #2", Admission},
		{"plan ≠ dispatch", Plan},
		{"t53 r2 #2", Meter},
		{"t53 r2 #2", Admission},
		{"t53 r2 #4", Admission},
		{"t53 r2 #3", Admission},
		{"t53 r3 #2", Admission},
	} {
		found := false
		for _, r := range Rows() {
			if !strings.Contains(r.Origin, want.origin) {
				continue
			}
			if _, p := r.Expect(want.reader); p != nil {
				t.Errorf("row %q from %q still pins %s on %s", r.Name, want.origin, want.reader, p.Unit)
				continue
			}
			if want.reader != Admission || r.Judged() {
				found = true
			}
		}
		if !found {
			t.Errorf("no row from %q judges %s against its Want", want.origin, want.reader)
		}
	}
}

// The rows U2 brings: a supervisor on the run's own key under the stamp (the
// one behaviour change), each own-config backend stamped and not, and the
// guard's two witnesses — a stamped pi and a stamped codex row refused
// arm-pending.
func TestTheUnitTwoRowsAreInTheTable(t *testing.T) {
	has := func(pred func(Row) bool) bool { return slices.ContainsFunc(Rows(), pred) }
	if !has(func(r Row) bool {
		return r.Role == SupervisorRole && r.Run.Stamped && !r.Want.Refused() && r.Want.Channel == RunKey
	}) {
		t.Error("no row has a stamped run's own key serve its supervisor")
	}
	for _, backend := range []string{"kimi", "grok", "opencode"} {
		if !has(func(r Row) bool { return r.Node.Backend == backend && r.Run.Stamped && r.Want.Refusal == AmbientStamp }) {
			t.Errorf("no row refuses a stamped %s node ambient-stamp", backend)
		}
		if !has(func(r Row) bool { return r.Node.Backend == backend && !r.Run.Stamped && r.Want.Channel == OwnConfig }) {
			t.Errorf("no row has an unstamped %s node read its own config", backend)
		}
	}
	for _, backend := range []string{"pi", "codex"} {
		if !has(func(r Row) bool { return r.Node.Backend == backend && r.Run.Stamped && r.Want.Refusal == ArmPending }) {
			t.Errorf("no row refuses a stamped %s node arm-pending (the guard's witness)", backend)
		}
	}
}

// The structural round's rows (#2022, U0–U2's second review): an LLM router, a
// human node's LLM half and a Verified Action's self-repair rung — the sites
// the admission skipped — each refused under the stamp where only the pod
// funds it, on both placements.
func TestTheSiteKindRowsAreInTheTable(t *testing.T) {
	for _, kind := range []SiteKind{LLMRouter, HumanLLM, SelfRepair} {
		for _, sandboxed := range []bool{false, true} {
			if !slices.ContainsFunc(Rows(), func(r Row) bool {
				return r.Node.Kind == kind && r.Sandboxed == sandboxed && r.Run.Stamped && r.Want.Refusal == AmbientStamp
			}) {
				t.Errorf("no row refuses a stamped %s site ambient-stamp (sandboxed %v)", kind, sandboxed)
			}
		}
	}
}

// The recovery table (RecoveryRows) holds the shapes its readers can disagree
// on: a recovery a run slot funds on a planned run (the ledger books the slot,
// the fine table its fingerprint), one the pod funds beside a plan (nobody,
// no miss), and one the stamp refuses (nothing sent, nothing booked).
func TestTheRecoveryRowsAreInTheTable(t *testing.T) {
	has := func(pred func(Row) bool) bool { return slices.ContainsFunc(RecoveryRows(), pred) }
	if !has(func(r Row) bool { return r.Run.Plan && r.Want.Slot != "" }) {
		t.Error("no recovery row spends a run slot on a planned run")
	}
	if !has(func(r Row) bool { return r.Run.Plan && !r.Want.Refused() && r.Want.Slot == "" }) {
		t.Error("no recovery row spends the pod's money beside a plan")
	}
	if !has(func(r Row) bool { return r.Run.Stamped && r.Want.Refusal == AmbientStamp }) {
		t.Error("no recovery row is refused under the stamp")
	}
	for _, r := range RecoveryRows() {
		if r.Role != AuxiliaryRole || r.Sandboxed {
			t.Errorf("row %q: the recovery is an auxiliary call in process on the host (role %q, sandboxed %v)", r.Name, r.Role, r.Sandboxed)
		}
	}
}
