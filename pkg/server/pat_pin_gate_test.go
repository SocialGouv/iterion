package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// #2486: a personal access token pinned to one team used to reach every team
// of its owner — the pin only picked the ACTIVE team, and every
// team-addressed gate then authorized on the owner's membership or org
// standing elsewhere. The pin is now a credential fact every gate asks about
// (auth.Identity.PinRefusesTeam / PinRefusesOrg), so this file measures the
// CLASS, not one route: each gate, pinned versus unpinned, on a team the
// owner genuinely holds.
//
// The unpinned arm is what makes the pinned arm mean anything: it is the same
// user, the same gate, the same team, and it must be ADMITTED. Without it a
// refusal proves only that the fixture forgot a membership.

// pinGateFixture is a server where u1 holds everything twice: admin of t1
// (org o1) and of t3 (org o3), and an org admin of both orgs — so every arm
// of every gate (direct grant, org lineage) would admit t3, and only the pin
// can refuse it.
type pinGateFixture struct {
	s        *Server
	ctx      context.Context
	pinned   auth.Identity
	unpinned auth.Identity
	super    auth.Identity
}

func newPinGateFixture(t *testing.T) pinGateFixture {
	t.Helper()
	s := newOrgTestServer(t)
	ctx := context.Background()
	seedOrg(t, s, "o1", "org-one")
	seedOrg(t, s, "o3", "org-three")
	for _, team := range []struct{ id, slug, org string }{
		{"t1", "team-one", "o1"},
		{"t3", "team-three", "o3"},
	} {
		if _, err := s.authStore().CreateTeam(ctx, identity.Team{
			ID: team.id, Name: team.id, Slug: team.slug, OrgID: team.org, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed team %s: %v", team.id, err)
		}
	}
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "u1", Email: "u1@x", Status: identity.UserStatusActive, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatalf("create the token owner: %v", err)
	}
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "root", Email: "root@x", Status: identity.UserStatusActive, IsSuperAdmin: true, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatalf("create the super-admin: %v", err)
	}
	for _, m := range []identity.Membership{
		{UserID: "u1", TeamID: "t1", Role: identity.RoleAdmin},
		{UserID: "u1", TeamID: "t3", Role: identity.RoleAdmin},
		{UserID: "root", TeamID: "t1", Role: identity.RoleAdmin},
		{UserID: "root", TeamID: "t3", Role: identity.RoleAdmin},
	} {
		if err := s.authStore().UpsertMembership(ctx, m); err != nil {
			t.Fatalf("seed membership %s/%s: %v", m.UserID, m.TeamID, err)
		}
	}
	for _, om := range []identity.OrgMembership{
		{UserID: "u1", OrgID: "o1", Role: identity.OrgRoleAdmin},
		{UserID: "u1", OrgID: "o3", Role: identity.OrgRoleAdmin},
		{UserID: "root", OrgID: "o1", Role: identity.OrgRoleAdmin},
		{UserID: "root", OrgID: "o3", Role: identity.OrgRoleAdmin},
	} {
		if err := s.authStore().UpsertOrgMembership(ctx, om); err != nil {
			t.Fatalf("seed org membership %s/%s: %v", om.UserID, om.OrgID, err)
		}
	}

	// Guard the premise: with no standing in t3 every refusal below would be
	// the membership check answering, and the test would pass on the bug.
	if mb, err := s.authStore().GetMembership(ctx, "u1", "t3"); err != nil || !mb.Role.AtLeast(identity.RoleAdmin) {
		t.Fatalf("the fixture must give the owner a real standing in t3 (role=%v err=%v)", mb.Role, err)
	}

	base := auth.Identity{
		UserID: "u1", Email: "u1@x", TeamID: "t1", OrgID: "o1",
		Role: identity.RoleAdmin, Credential: auth.CredentialPAT, CredentialID: "pat-1",
	}
	pinned := base
	pinned.TeamPin, pinned.OrgPin = "t1", "o1"
	super := base
	super.UserID, super.Email, super.IsSuperAdmin = "root", "root@x", true
	super.TeamPin, super.OrgPin = "t1", "o1"
	return pinGateFixture{s: s, ctx: ctx, pinned: pinned, unpinned: base, super: super}
}

// teamGates is every team-addressed gate, as a caller reaches it.
func (f pinGateFixture) teamGates(t *testing.T) map[string]func(auth.Identity, string) bool {
	t.Helper()
	return map[string]func(auth.Identity, string) bool{
		"canViewTeam": func(id auth.Identity, team string) bool { return f.s.canViewTeam(f.ctx, id, team) },
		"canViewTeamOrFail": func(id auth.Identity, team string) bool {
			ok, err := f.s.canViewTeamOrFail(f.ctx, id, team)
			if err != nil {
				t.Fatalf("canViewTeamOrFail(%s): %v", team, err)
			}
			return ok
		},
		"canManageTeam":       func(id auth.Identity, team string) bool { return f.s.canManageTeam(f.ctx, id, team) },
		"canEditConfigShares": func(id auth.Identity, team string) bool { return f.s.canEditConfigShares(f.ctx, id, team) },
		"canEditBots":         func(id auth.Identity, team string) bool { return f.s.canEditBots(f.ctx, id, team) },
		"orgAdminOfTeam":      func(id auth.Identity, team string) bool { return f.s.orgAdminOfTeam(f.ctx, id, team) },
		"orgAdminOfTeamOrFail": func(id auth.Identity, team string) bool {
			ok, err := f.s.orgAdminOfTeamOrFail(f.ctx, id, team)
			if err != nil {
				t.Fatalf("orgAdminOfTeamOrFail(%s): %v", team, err)
			}
			return ok
		},
		// The run- and issue-by-id re-scope (ADR-103/141): the second way a
		// request addresses a team, and the one a per-route guard misses.
		// allowOrgShared is true on purpose: it is the widest form of this
		// gate (the ADR-144 org-shared arm included), so the census measures
		// the pin against every grant the re-scope can hand out.
		"identityInTeam": func(id auth.Identity, team string) bool {
			_, ok, err := f.s.identityInTeam(f.ctx, id, team, true)
			if err != nil {
				t.Fatalf("identityInTeam(%s): %v", team, err)
			}
			return ok
		},
	}
}

// orgGates is every org-addressed gate.
func (f pinGateFixture) orgGates(t *testing.T) map[string]func(auth.Identity, string) bool {
	t.Helper()
	return map[string]func(auth.Identity, string) bool{
		"canViewOrg":   func(id auth.Identity, org string) bool { return f.s.canViewOrg(f.ctx, id, org) },
		"canManageOrg": func(id auth.Identity, org string) bool { return f.s.canManageOrg(f.ctx, id, org) },
		"canManageOrgOrFail": func(id auth.Identity, org string) bool {
			ok, err := f.s.canManageOrgOrFail(f.ctx, id, org)
			if err != nil {
				t.Fatalf("canManageOrgOrFail(%s): %v", org, err)
			}
			return ok
		},
	}
}

func TestPATPin_BoundsEveryTeamAddressedGate(t *testing.T) {
	f := newPinGateFixture(t)
	for name, gate := range f.teamGates(t) {
		t.Run(name, func(t *testing.T) {
			if !gate(f.pinned, "t1") {
				t.Fatalf("%s refused the pinned token on its OWN team — the pin bounds, it does not revoke", name)
			}
			if !gate(f.unpinned, "t3") {
				t.Fatalf("%s refused an UNPINNED token on another team of its owner: the fixture's standing in t3 is not reaching this gate, so its refusal of the pinned token would prove nothing", name)
			}
			if gate(f.pinned, "t3") {
				t.Fatalf("%s admitted a token pinned to t1 on t3 — a leaked CI token reaches every team of its owner (#2486)", name)
			}
		})
	}
}

func TestPATPin_BoundsEveryOrgAddressedGate(t *testing.T) {
	f := newPinGateFixture(t)
	for name, gate := range f.orgGates(t) {
		t.Run(name, func(t *testing.T) {
			if !gate(f.pinned, "o1") {
				t.Fatalf("%s refused the pinned token on its own team's org", name)
			}
			if !gate(f.unpinned, "o3") {
				t.Fatalf("%s refused an UNPINNED token on another org of its owner: the fixture's org standing is not reaching this gate", name)
			}
			if gate(f.pinned, "o3") {
				t.Fatalf("%s admitted a token pinned to a team of o1 on o3 (#2486)", name)
			}
		})
	}
}

// A pin on a team with NO parent org reaches no organization: OrgPin is empty
// and every named org differs from it. The fail-closed reading, asserted so a
// later "empty means unbounded" shortcut reddens here.
func TestPATPin_OnATeamWithoutAnOrgReachesNoOrg(t *testing.T) {
	f := newPinGateFixture(t)
	orphan := f.pinned
	orphan.TeamPin, orphan.OrgPin = "t-orphan", ""
	if !orphan.PinRefusesOrg("o1") {
		t.Fatal("a pin on an org-less team reaches o1 — an empty OrgPin must bound, not open")
	}
}

// D19: the platform authority sees everything by default, so a super-admin's
// pinned token keeps its reach. Asserted rather than assumed — it is the one
// named place a future team-level opt-in shield inserts, and a change of mind
// here must be deliberate.
func TestPATPin_LeavesTheSuperAdminAboveTheGate(t *testing.T) {
	f := newPinGateFixture(t)
	for name, gate := range f.teamGates(t) {
		if !gate(f.super, "t3") {
			t.Errorf("%s refused a super-admin's pinned token on t3 — D19 keeps the platform authority above the pin", name)
		}
	}
	for name, gate := range f.orgGates(t) {
		if !gate(f.super, "o3") {
			t.Errorf("%s refused a super-admin's pinned token on o3 — D19 keeps the platform authority above the pin", name)
		}
	}
}

// An identity re-scoped to a run's team is still the credential that was
// presented: the pin travels with the credential facts (withCredentialOf), so
// a pinned token cannot launder itself through the re-scope.
func TestPATPin_SurvivesTheCredentialCarrier(t *testing.T) {
	f := newPinGateFixture(t)
	scoped := withCredentialOf(auth.Identity{UserID: "u1", TeamID: "t1", Role: identity.RoleAdmin}, f.pinned)
	if scoped.TeamPin != "t1" || scoped.OrgPin != "o1" {
		t.Fatalf("withCredentialOf dropped the pin: TeamPin=%q OrgPin=%q", scoped.TeamPin, scoped.OrgPin)
	}
	if !scoped.PinRefusesTeam("t3") {
		t.Fatal("a re-scoped identity no longer refuses the team its credential's pin excludes")
	}
}

// patPinGates enumerates every authorization gate of auth_authz.go — a method
// of *Server taking exactly (context.Context, auth.Identity, string) where
// the string is a teamID or an orgID — and names how it honours the
// credential's pin: either it asks the predicate itself, or it delegates to a
// gate in this same table.
//
// The pin is a property of the CREDENTIAL, so a gate that never asks about it
// is a gate a pinned token walks through, and the repo has paid four review
// rounds for exactly this class resurfacing site by site. Hence the
// enumeration rather than a body scan alone: a NEW gate of that shape with no
// entry here reddens this test and has to state its answer.
//
// Blind spot, documented: a gate of another shape — one taking an
// *http.Request, a struct, or living in another file — is outside this scan.
// The behavioural tests above cover the one such gate that exists today
// (identityInTeamOn, run_scope.go).
var patPinGates = map[string]string{
	"canViewTeam":          "canViewTeamOrFail",
	"canViewTeamOrFail":    "PinRefusesTeam",
	"canEditConfigShares":  "PinRefusesTeam",
	"canEditBots":          "canEditConfigShares",
	"canManageTeam":        "PinRefusesTeam",
	"orgAdminOfTeam":       "orgAdminOfTeamOrFail",
	"orgAdminOfTeamOrFail": "PinRefusesTeam",
	"canViewOrg":           "PinRefusesOrg",
	"canManageOrg":         "canManageOrgOrFail",
	"canManageOrgOrFail":   "PinRefusesOrg",
}

func TestEveryAuthzGateAnswersTheCredentialPin(t *testing.T) {
	const file = "auth_authz.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	found := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil || !isAuthzGateSignature(fn) {
			continue
		}
		name := fn.Name.Name
		found[name] = true
		answer, listed := patPinGates[name]
		if !listed {
			t.Errorf("%s is an authorization gate of %s with no entry in patPinGates: say how it honours the credential's pin (call auth.Identity.PinRefusesTeam/PinRefusesOrg, or delegate to a gate that does)", name, file)
			continue
		}
		if !bodyCalls(fn.Body, answer) {
			t.Errorf("%s claims to honour the pin through %q, which its body never calls", name, answer)
		}
		if _, delegated := patPinGates[answer]; !delegated && answer != "PinRefusesTeam" && answer != "PinRefusesOrg" {
			t.Errorf("%s delegates to %q, which is not itself a gate of this table", name, answer)
		}
	}

	if len(found) != len(patPinGates) {
		t.Fatalf("the scan saw %d gates in %s, the table names %d — a renamed or moved gate leaves the pin unchecked (seen: %v)", len(found), file, len(patPinGates), found)
	}
}

// isAuthzGateSignature reports whether fn is a gate of the shape every
// authz check in auth_authz.go takes: (context.Context, auth.Identity,
// teamID|orgID string).
func isAuthzGateSignature(fn *ast.FuncDecl) bool {
	var params []*ast.Field
	for _, p := range fn.Type.Params.List {
		// One field per name: `a, b string` is two parameters.
		for range max(len(p.Names), 1) {
			params = append(params, p)
		}
	}
	if len(params) != 3 {
		return false
	}
	if typeName(params[0].Type) != "context.Context" || typeName(params[1].Type) != "auth.Identity" {
		return false
	}
	if typeName(params[2].Type) != "string" || len(params[2].Names) != 1 {
		return false
	}
	switch params[2].Names[0].Name {
	case "teamID", "orgID":
		return true
	}
	return false
}

// typeName renders the identifier or qualified identifier of a type
// expression; anything else answers "".
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
	}
	return ""
}

// bodyCalls reports whether body calls the named function or method.
func bodyCalls(body *ast.BlockStmt, name string) bool {
	called := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			called = called || fn.Name == name
		case *ast.SelectorExpr:
			called = called || fn.Sel.Name == name
		}
		return true
	})
	return called
}
