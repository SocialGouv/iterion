package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A `tool … action:` node resolves through a connector catalog the LAUNCH
// SURFACE hands it. Only `iterion run` and `iterion resume` did: the studio,
// the launch API, a subbot and the dispatcher built their ExecutorSpec without
// it, so the same `.bot`, on the same machine, with the same catalog and the
// same sealer, ran from the CLI and failed at its first action node
// everywhere else.
//
// These pin the answer that is now shared by all of them.

func actionWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Action: "forgejo.issue.comment", Connection: "main"},
	}}
}

func plainWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: "go test ./..."},
	}}
}

func TestLocalConnectorsAreWiredForAWorkflowThatCallsOne(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "connectors", "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The project tier is a deliberate grant, so a fixture that seeds it has
	// to make it — otherwise this passes on the tier that reports why it did
	// not answer, and pins nothing about the wiring it exists to pin.
	t.Setenv(connection.ProjectCatalogEnv, "1")
	sealer := secrets.NewLazyLocalSealer(t.TempDir(), nil)

	r, client, err := LocalConnectors(actionWorkflow(), workspace, t.TempDir(), sealer, connection.TrustOfRun(store.RunTrustDefault))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("a workflow declaring `action:` must reach the local catalog from every launch surface, not only the CLI")
	}
	if client == nil {
		t.Error("the resolver must come with its GUARDED client — without one the executor would invent the unguarded default")
	}
}

// TestAnUngrantedProjectCatalogSaysSoAtTheRUN.
//
// The project tier is off by default because the workspace is the repository
// a run acts on. A repository that ships `connectors/` and hears "no connector
// catalog wired" has been told about a directory it does not have, while the
// one it does have goes unmentioned — the silently-inert-capability shape this
// repo calls a defect. The refusal has to name the grant at the surface where
// the run meets it, not only where an operator types a command.
func TestAnUngrantedProjectCatalogSaysSoAtTheRun(t *testing.T) {
	t.Setenv(connection.ProjectCatalogEnv, "")
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "connectors", "forgejo"), 0o755); err != nil {
		t.Fatal(err)
	}

	r, _, err := LocalConnectors(actionWorkflow(), workspace, t.TempDir(), secrets.NewLazyLocalSealer(t.TempDir(), nil), connection.TrustOfRun(store.RunTrustDefault))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("a present-but-ungranted tier must not vanish — it is what has the answer an operator needs")
	}
	_, _, _, _, err = r.ResolveAction(context.Background(), "forgejo.issue.comment", "main")
	if err == nil {
		t.Fatal("the tier is not granted, so nothing may serve its package")
	}
	if !strings.Contains(err.Error(), connection.ProjectCatalogEnv) {
		t.Errorf("error = %v, want it to name %s", err, connection.ProjectCatalogEnv)
	}
}

func TestLocalConnectorsAreNotBuiltWhenNothingWouldUseThem(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "connectors", "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		wf     *ir.Workflow
		sealer secrets.Sealer
	}{
		// Reading the catalog and the connection store for a run that cannot
		// call one is work nobody asked for — the same gate the local secret
		// store follows.
		{"no action node", plainWorkflow(), secrets.NewLazyLocalSealer(t.TempDir(), nil)},
		// No sealer is a CLOUD surface: its connections come from the
		// tenant's store, and a local catalog there would be the pod's.
		{"no sealer", actionWorkflow(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, client, err := LocalConnectors(tc.wf, workspace, t.TempDir(), tc.sealer, connection.TrustOfRun(store.RunTrustDefault))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if r != nil || client != nil {
				t.Errorf("resolver = %v, client = %v, want neither built", r, client)
			}
		})
	}
}

// TestNoCatalogYieldsANilINTERFACE.
//
// The executor asks `e.connectors == nil` to answer "this process has no
// connector catalog wired", which names the fix. A nil *connection.Resolver
// assigned into that interface field is NOT nil, so the run would instead get
// the resolver's own "not wired (catalog or store missing)" — a message that
// points at the connector rather than at the install.
func TestNoCatalogYieldsANilINTERFACE(t *testing.T) {
	// A workspace and a home with no `connectors/` at all, and no store file.
	r, _, err := LocalConnectors(actionWorkflow(), t.TempDir(), t.TempDir(), secrets.NewLazyLocalSealer(t.TempDir(), nil), connection.TrustOfRun(store.RunTrustDefault))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r != nil {
		t.Errorf("resolver = %#v, want an untyped nil so the executor's own diagnostic speaks", r)
	}
}

// TestTheLaunchSurfacesTrustReachesTheResolution is the other half of #1802.
//
// The refusal itself lives at pkg/connection's resolution chokepoint, and a
// test there proves the predicate. What it cannot prove is that the trust a
// LAUNCH SURFACE holds — a launch spec, a resumed run's document, a subbot's
// parent — ever arrives: hardcoding "trusted" inside this builder would leave
// that test green and the guard dead, which is the "a guard whose condition can
// never be true" shape the ticket exists to close.
//
// So the fixture is the real builder, twice over the SAME workspace, and the
// question is WHICH refusal comes back: no connection is seeded, so nothing
// resolves either way — the trusted arm is refused by the tenant's connection
// store (it got that far), the fork arm before it, and the fork arm's READ gets
// the store's refusal again.
func TestTheLaunchSurfacesTrustReachesTheResolution(t *testing.T) {
	workspace := t.TempDir()
	// The OPERATOR's own tier holds the package, which is both what a real
	// install has and the only tier that can answer the question this test
	// asks: since #2769 the workspace tier is not consulted at all for a run
	// whose trust is not the tenant's own, so a project-tier fixture would
	// make the fork arms fail on the TIER and prove nothing about whether the
	// trust arrived. The tier rule has its own witnesses in pkg/connection and
	// in TestAForkLaunchIsNotServedTheWorkspacesOwnPackage below.
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv(connection.ProjectCatalogEnv, "")
	writeTrustProbePackage(t, filepath.Join(home, "connectors"))
	sealer := secrets.NewLazyLocalSealer(t.TempDir(), nil)

	for _, tc := range []struct {
		name        string
		trust       store.RunTrust
		action      string
		wantRefused bool
	}{
		{"a trusted launch resolves as far as the tenant's connections", store.RunTrustDefault, "probe.issue.comment", false},
		{"a fork launch's mutating action is refused before them", store.RunTrustFork, "probe.issue.comment", true},
		{"a fork launch's read is not", store.RunTrustFork, "probe.issue.get", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, err := LocalConnectors(actionWorkflow(), workspace, t.TempDir(), sealer, connection.TrustOfRun(tc.trust))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if r == nil {
				t.Fatal("the operator's own tier holds the package, so a resolver must be built")
			}
			_, _, _, _, err = r.ResolveAction(context.Background(), tc.action, "main")
			if err == nil {
				t.Fatalf("resolve %s: no connection is seeded, so nothing may resolve", tc.action)
			}
			if got := errors.Is(err, connection.ErrUntrustedMutation); got != tc.wantRefused {
				t.Fatalf("resolve %s on trust %q: untrusted-mutation refusal = %v, want %v (error: %v)", tc.action, string(tc.trust), got, tc.wantRefused, err)
			}
			if !tc.wantRefused && !strings.Contains(err.Error(), "connection named") {
				t.Errorf("error = %v, want the tenant's connection store to have been consulted", err)
			}
		})
	}
}

// TestAForkLaunchIsNotServedTheWorkspacesOwnPackage is #2769 at the surface
// where the trust is REAL.
//
// pkg/connection proves the rule on the tiers it builds; what it cannot prove
// is that the composition closes at a launch surface — the grant open, the
// workspace holding a package of its own, and the lane's trust arriving from
// the run document. Under the defect this launch resolved `probe.issue.comment`
// from the reviewed checkout's own package, and #1802's refusal read that
// package's `effect:` to decide whether the credential was owed.
//
// The DEFAULT-trust arm over the same workspace is what keeps this honest: the
// tier is withheld by trust, not removed, so the grant still works for the
// operator it exists for.
func TestAForkLaunchIsNotServedTheWorkspacesOwnPackage(t *testing.T) {
	workspace := t.TempDir()
	// An empty home tier, so the workspace's package is the only answer
	// available and the arms cannot be confused by a second one.
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv(connection.ProjectCatalogEnv, "1")
	writeTrustProbePackage(t, filepath.Join(workspace, "connectors"))
	sealer := secrets.NewLazyLocalSealer(t.TempDir(), nil)

	resolve := func(t *testing.T, trust store.RunTrust) error {
		t.Helper()
		r, _, err := LocalConnectors(actionWorkflow(), workspace, t.TempDir(), sealer, connection.TrustOfRun(trust))
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if r == nil {
			t.Fatal("the tier is present, so a resolver is built — a withheld tier is not an absent catalog")
		}
		// A READ, deliberately: the mutating one would be refused by #1802
		// even if the tier had served it, so only the read can tell "the tier
		// did not answer" from "the trust refused the effect".
		_, _, _, _, err = r.ResolveAction(context.Background(), "probe.issue.get", "main")
		if err == nil {
			t.Fatal("no connection is seeded, so nothing may resolve")
		}
		return err
	}

	forkErr := resolve(t, store.RunTrustFork)
	if !strings.Contains(forkErr.Error(), "does not control") {
		t.Errorf("fork launch error = %v, want the workspace tier to report that it was not consulted for this run", forkErr)
	}
	if strings.Contains(forkErr.Error(), "connection named") {
		t.Errorf("fork launch error = %v, want the tier refusal rather than the connection store's: the outsider's package must not be resolved at all", forkErr)
	}
	// The falsifier: the operator's own launch over the SAME workspace gets
	// past the catalog and is refused by the tenant's connection store.
	if ownErr := resolve(t, store.RunTrustDefault); !strings.Contains(ownErr.Error(), "connection named") {
		t.Errorf("default-trust launch error = %v, want the granted workspace tier to have served the package", ownErr)
	}
}

// writeTrustProbePackage writes a connector package whose two operations
// differ only in their EFFECT, which is what the trust verdict turns on.
func writeTrustProbePackage(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	op := func(id, verb, method string, effect spec.Effect, status int) spec.Operation {
		return spec.Operation{
			ID: id, Resource: "issue", Verb: verb,
			HTTP:   spec.HTTPBinding{Method: method, Path: "/issues/{id}"},
			Effect: effect, Deterministic: true,
			Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
			Results: []spec.ResultCase{{Status: status}},
		}
	}
	pkg := &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			DisplayName: "Probe",
			BaseURL:     spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization",
			}},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{
				op("probe.issue.get", "get", "GET", spec.EffectRead, 200),
				op("probe.issue.comment", "comment", "POST", spec.EffectCreate, 201),
			},
		}},
	}
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write package: %v", err)
	}
}

// TestASubbotInheritsItsParentsTrust pins the ONE trust decision in this
// package that is a rule rather than a field read.
//
// A child subbot is a run of its own and resolves its own `action:` nodes, but
// its code came out of the parent's workspace. The trap is the nil arm: a
// parent that does not load and a parent that is the operator's own checkout
// are the SAME BYTES in store.RunTrust, so a child of an unreadable parent
// would have resolved mutating actions as though somebody had vouched for it.
func TestASubbotInheritsItsParentsTrust(t *testing.T) {
	// The whole reason a resolver's trust is not a bare store.RunTrust.
	if connection.TrustUnknown() == connection.TrustOfRun(store.RunTrustDefault) {
		t.Fatal("`nobody said` and `the tenant's own checkout` must be distinguishable values, or every forgotten wiring reads as trusted")
	}
	for _, tc := range []struct {
		name   string
		parent *store.Run
		want   connection.ActionTrust
	}{
		{"a child of a trusted parent is trusted", &store.Run{Trust: store.RunTrustDefault}, connection.TrustOfRun(store.RunTrustDefault)},
		{"a child of a fork parent is a fork", &store.Run{Trust: store.RunTrustFork}, connection.TrustOfRun(store.RunTrustFork)},
		// Carried through rather than normalised: the resolver's predicate is
		// Trusted(), which refuses what it does not recognise, and flattening
		// an unknown class here would decide it in the wrong place.
		{"a class this build does not know travels as itself", &store.Run{Trust: store.RunTrust("vendored")}, connection.TrustOfRun(store.RunTrust("vendored"))},
		{"a child whose parent does not load states nothing", nil, connection.TrustUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subbotConnectorTrust(tc.parent); got != tc.want {
				t.Fatalf("subbotConnectorTrust(%+v) = %#v, want %#v", tc.parent, got, tc.want)
			}
		})
	}
}
