package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The defect this covers: a grant lived only in the node input of the
// re-invocation it was answered on, so the next pause — resumed by a
// fresh engine with a fresh runState — no longer had it, and the
// operator re-authorized the same tool for every mutation.
func TestPermissionGrants_AccumulatePersistAndReseed(t *testing.T) {
	ctx := context.Background()
	wf := loopWorkflow(2)
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "r-grants", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eng := New(wf, s, newStubExecutor())
	rs := eng.newRunState("r-grants", nil)
	rs.ctx = ctx

	eng.recordPermissionGrant(rs, "writer", "Write")
	eng.recordPermissionGrant(rs, "writer", "Bash(git add:*)")
	eng.recordPermissionGrant(rs, "writer", "Write") // same answer twice: no growth
	eng.recordPermissionGrant(rs, "writer", "")      // nothing to record
	eng.recordPermissionGrant(rs, "", "Write")       // no node, no record

	want := map[string][]string{"writer": {"Write", "Bash(git add:*)"}}
	if !reflect.DeepEqual(rs.permissionGrants, want) {
		t.Fatalf("in-memory grants = %#v, want %#v", rs.permissionGrants, want)
	}

	r, err := s.LoadRun(ctx, "r-grants")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if !reflect.DeepEqual(r.PermissionGrants, want) {
		t.Fatalf("persisted grants = %#v, want %#v", r.PermissionGrants, want)
	}

	// The resume: a brand-new runState, re-seeded from the run record.
	fresh := eng.newRunState("r-grants", nil)
	if len(fresh.permissionGrants) != 0 {
		t.Fatalf("a fresh runState already holds grants: %#v", fresh.permissionGrants)
	}
	eng.applySteeringState(fresh, r)
	if !reflect.DeepEqual(fresh.permissionGrants, want) {
		t.Errorf("re-seeded grants = %#v, want %#v", fresh.permissionGrants, want)
	}
	// Re-seeding must copy, not alias the store's slice.
	fresh.permissionGrants["writer"][0] = "mutated"
	if r.PermissionGrants["writer"][0] != "Write" {
		t.Error("applySteeringState aliased the run record's slice")
	}
}

// `allow` and `allow always` differ in LIFETIME, not only in scope. Only
// the always form may join the run set; recording the once form would
// turn a single approved argument-scoped rule into standing
// authorization for the rest of the run.
func TestPermissionGrants_OnlyAlwaysIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		recorded bool
	}{
		{"allow always", true},
		{"allow", false},
		{"once", false},
		{"deny", false},
		{"oui", false}, // unparseable: read as a refusal
	} {
		t.Run(tc.answer, func(t *testing.T) {
			allow, always := permission.ParseAnswer(tc.answer)
			rule, approved := permission.GrantFromAnswer(tc.answer, "Bash", map[string]any{"command": "rm -rf build/*"})
			if approved != allow {
				t.Fatalf("GrantFromAnswer approved=%v but ParseAnswer allow=%v", approved, allow)
			}
			if got := approved && always; got != tc.recorded {
				t.Errorf("answer %q would be recorded run-wide = %v, want %v (rule %q)", tc.answer, got, tc.recorded, rule)
			}
		})
	}
}

// Both node-input keys must be honoured by the executor contract, and
// they carry different things: the pause's own grant, and the run set.
func TestPermissionGrants_BothInputKeysDecode(t *testing.T) {
	if got := permission.GrantsFrom("Bash(rm -rf build/*)"); len(got) != 1 {
		t.Errorf("this pause's grant decoded to %#v, want one rule", got)
	}
	if got := permission.GrantsFrom([]string{"Write", "Bash(git add:*)"}); len(got) != 2 {
		t.Errorf("the run set decoded to %#v, want two rules", got)
	}
	if got := permission.GrantsFrom(nil); len(got) != 0 {
		t.Errorf("an absent key decoded to %#v, want nothing", got)
	}
}

// A grant belongs to the node that earned it. Evaluate ranks allow rules
// above the mode default, so lending one node's grant to another would
// beat a `permission: deny` that other node declared for itself — an
// approval given on a permissive implementer silently unlocking a strict
// reviewer.
func TestPermissionGrants_DoNotLeakToAnotherNode(t *testing.T) {
	ctx := context.Background()
	wf := loopWorkflow(2)
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "r-scope", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eng := New(wf, s, newStubExecutor())
	rs := eng.newRunState("r-scope", nil)
	rs.ctx = ctx
	eng.recordPermissionGrant(rs, "writer", "Bash")

	granted := eng.buildNodeInputRS("writer", rs.scope())
	if got := permission.GrantsFrom(granted[permission.RunGrantsInputKey]); len(got) != 1 {
		t.Errorf("the granting node carries %#v, want its own grant", got)
	}
	strict := eng.buildNodeInputRS("reviewer", rs.scope())
	if got := permission.GrantsFrom(strict[permission.RunGrantsInputKey]); len(got) != 0 {
		t.Errorf("a different node carries %#v, want nothing", got)
	}
}

// Run inputs are caller-supplied and land wholesale on the entry node, so
// a launch payload naming the reserved keys would otherwise seed policy
// allow rules straight into the gate meant to bound that caller.
func TestPermissionGrants_LaunchInputsCannotSeedRules(t *testing.T) {
	wf := loopWorkflow(2)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r-inject", map[string]any{
		permission.GrantInputKey:     "Bash",
		permission.RunGrantsInputKey: []string{"Bash", "Write"},
		"legit":                      "kept",
	})

	in := eng.buildNodeInputRS(wf.Entry, rs.scope())
	if _, present := in[permission.GrantInputKey]; present {
		t.Error("a launch input seeded this-pause grant onto the entry node")
	}
	if _, present := in[permission.RunGrantsInputKey]; present {
		t.Error("a launch input seeded run grants onto the entry node")
	}
	if in["legit"] != "kept" {
		t.Errorf("ordinary launch inputs were dropped: %#v", in)
	}
}
