package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

// trustProbePackage is a two-operation connector whose operations differ ONLY
// in their EFFECT: same scheme, same scope requirement, so ONE connection
// serves both.
//
// probePackage's two differ in scheme AND scope (that is what its scope
// branches need), and a connection that can perform one of those cannot
// perform the other — which would make "the read still resolves" unprovable
// against the same record. The question here turns on the effect and on
// nothing else, so the fixture differs in nothing else.
func trustProbePackage() *spec.Package {
	op := func(id, verb string, effect spec.Effect, status int) spec.Operation {
		return spec.Operation{
			ID: id, Resource: "issue", Verb: verb,
			HTTP:   spec.HTTPBinding{Method: "POST", Path: "/issues/{id}"},
			Effect: effect, Deterministic: true,
			Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
			Results: []spec.ResultCase{{Status: status}},
			Security: []spec.SecurityRequirement{
				{Terms: []spec.SecurityTerm{{SchemeID: "token", Scopes: []string{"issue"}}}},
			},
		}
	}
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL: spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{
				{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"},
			},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{
				op("probe.issue.get", "get", spec.EffectRead, 200),
				op("probe.issue.comment", "comment", spec.EffectCreate, 201),
				// A hand-written package, or one from a schema this build does
				// not know yet. spec.Effect.Mutating() reads it as mutating,
				// and the refusal has to carry that rather than assert what
				// the operation does.
				op("probe.issue.reed", "reed", spec.Effect("reed"), 200),
			},
		}},
	}
}

// trustResolver is the SAME fixture for every arm of the table — same package,
// same store, same sealed credential, same connection — differing in the one
// thing under test.
func trustResolver(t *testing.T, trust connection.ActionTrust) *connection.Resolver {
	t.Helper()
	st := connection.NewMemoryStore()
	r := &connection.Resolver{
		Catalog:  connection.NewMemoryCatalog(trustProbePackage()),
		Store:    st,
		Sealer:   sealer(t),
		TenantID: "tenant-a",
		Trust:    trust,
	}
	c := conn("c1", "tenant-a", "main")
	// Both capabilities, so ONE connection serves both entries of the
	// chokepoint: the tool node the workflow's graph declared (action) and
	// the facade the MODEL chose (agent). The capability grant has its own
	// table in agent_test.go; here it must not refuse before the trust can.
	c.Capabilities = []connection.Capability{connection.CapAction, connection.CapAgent}
	c.GrantedScopes, c.ScopesKnown = []string{"issue"}, true
	seed(t, r, st, c)
	return r
}

// TestAnActionIsRefusedOnAnUntrustedWorkspace is #1802, tightened by ADR-162
// decision 6.
//
// A `tool … action:` node authenticates with the TENANT's binding. Nothing on
// that path asked who wrote the workflow, so a run over a workspace authored
// outside the tenant — the fork review lane's store.RunTrustFork — would have
// spent the tenant's credential on whatever the fork's own graph declared.
// #1802 let such a run read; decision 6 withdraws the reads too, because what
// the run reads can leave through the run's one route out — so every action,
// read or mutating, through the tool node's seam AND the facade's, refuses.
//
// The TRUSTED arm is what makes the untrusted arms mean something: the same
// connection, the same operation, resolving all the way to the credential. The
// refusal is the forbidden alternative, not a fixture that cannot resolve at
// all.
func TestAnActionIsRefusedOnAnUntrustedWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		via     string // "tool node" (ResolveAction) or "facade" (ResolveAgent)
		trust   connection.ActionTrust
		action  string
		wantErr error // nil: must resolve all the way to the credential
	}{
		{"a trusted run resolves the mutating operation", "tool node", connection.TrustOfRun(store.RunTrustDefault), "probe.issue.comment", nil},
		{"a fork-trust run does not", "tool node", connection.TrustOfRun(store.RunTrustFork), "probe.issue.comment", connection.ErrUntrustedAction},
		// The predicate is Trusted(), not IsFork(): a value this binary does
		// not recognise — a document written by a newer replica mid-rollout, a
		// hand-edited row — must lose the credential too. Mutating the check
		// to t.trust.IsFork() leaves this arm, and only this arm, red.
		{"a trust value this build does not know does not either", "tool node", connection.TrustOfRun(store.RunTrust("vendored")), "probe.issue.comment", connection.ErrUntrustedAction},
		// The zero value is what a builder that FORGOT the field produces, and
		// store.RunTrust's own zero value is the trusted one — so this arm is
		// the whole reason trust is not a bare store.RunTrust here.
		{"a resolver nobody told does not either (the zero value)", "tool node", connection.ActionTrust{}, "probe.issue.comment", connection.ErrTrustUnstated},
		{"nor one that says it cannot tell", "tool node", connection.TrustUnknown(), "probe.issue.comment", connection.ErrTrustUnstated},
		// An effect this build does not know fails SAFE, at this site too:
		// Mutating() admits only `read`, so a typo cannot buy a mutation.
		{"an effect this build does not know is refused as mutating", "tool node", connection.TrustOfRun(store.RunTrustFork), "probe.issue.reed", connection.ErrUntrustedAction},
		// The tightened half (ADR-162 decision 6): a READ refuses too, on both
		// entries of the one chokepoint — the tool node the workflow's own
		// graph declared, and the facade the MODEL chose.
		{"a fork-trust run resolves no read either", "tool node", connection.TrustOfRun(store.RunTrustFork), "probe.issue.get", connection.ErrUntrustedAction},
		{"an unstated resolver resolves no read either", "tool node", connection.TrustUnknown(), "probe.issue.get", connection.ErrTrustUnstated},
		{"a read the MODEL chose is refused through the facade too", "facade", connection.TrustOfRun(store.RunTrustFork), "probe.issue.get", connection.ErrUntrustedAction},
		{"a mutation the MODEL chose is refused through the facade too", "facade", connection.TrustOfRun(store.RunTrustFork), "probe.issue.comment", connection.ErrUntrustedAction},
		{"the facade resolves the read for the tenant's own run", "facade", connection.TrustOfRun(store.RunTrustDefault), "probe.issue.get", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := trustResolver(t, tc.trust)
			var err error
			var op spec.Operation
			var cred exec.Credential
			var baseURL string
			if tc.via == "facade" {
				_, op, cred, baseURL, err = r.ResolveAgent(context.Background(), tc.action, "main")
			} else {
				_, op, cred, baseURL, err = r.ResolveAction(context.Background(), tc.action, "main")
			}

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("resolve %s = %v, want the credential — the arm that proves the fixture resolves at all", tc.action, err)
				}
				if cred.Value != "the-token" || baseURL == "" {
					t.Fatalf("credential = %+v, baseURL = %q, want the sealed token and the connection's origin", cred, baseURL)
				}
				if op.ID != tc.action {
					t.Fatalf("op = %q, want %q", op.ID, tc.action)
				}
				return
			}

			if err == nil {
				t.Fatalf("resolve %s through the %s handed back a credential (%+v) on a workspace the tenant does not control", tc.action, tc.via, cred)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("resolve %s = %v, want it to wrap %v — a caller branches on the sentinel, not on a substring", tc.action, err, tc.wantErr)
			}
			var typed *connection.TrustError
			if !errors.As(err, &typed) {
				t.Fatalf("error = %T, want a typed *connection.TrustError: a grant an operator widens and a property of the RUN are fixed in different places", err)
			}
			if cred.Value != "" || baseURL != "" {
				t.Fatalf("credential = %+v, baseURL = %q, want nothing handed back with the refusal", cred, baseURL)
			}
			// What an operator reads has to name what was refused and which
			// binding was not spent.
			for _, want := range []string{tc.action, `"main"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to name %s", err, want)
				}
			}
		})
	}
}

// TestTheTwoRefusalsSayDifferentThingsToDifferentReaders.
//
// "This run resolves no connector at all" is read by an OPERATOR whose fork
// run was refused, and nothing is wrong. "Nobody stated the trust" is read by
// a DEVELOPER whose builder forgot to say, and something is. A single message
// for both would send each of them to the other's fix.
func TestTheTwoRefusalsSayDifferentThingsToDifferentReaders(t *testing.T) {
	forkErr := refusal(t, connection.TrustOfRun(store.RunTrustFork))
	if !strings.Contains(forkErr, "no connector action at all") || !strings.Contains(forkErr, `trust="fork"`) {
		t.Errorf("fork refusal = %q, want the lane's contract and the trust it was refused for", forkErr)
	}
	r := trustResolver(t, connection.TrustOfRun(store.RunTrustFork))
	_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.reed", "main")
	if err == nil {
		t.Fatal("an unknown effect must be refused on an untrusted workspace")
	}
	if !strings.Contains(err.Error(), "does not know") {
		t.Errorf("unknown-effect refusal = %v, want it to say the effect is unrecognised rather than assert what the operation does", err)
	}
	// A refused READ must not be described as one: the sentence is what an
	// operator reads first, and a read that "changes remote state" is a lie
	// with a fix attached to it.
	_, _, _, _, readErr := r.ResolveAction(context.Background(), "probe.issue.get", "main")
	if readErr == nil || !strings.Contains(readErr.Error(), "only reads") {
		t.Errorf("read refusal = %v, want it to say what the operation does (only reads), not a state change it does not make", readErr)
	}

	unstated := refusal(t, connection.TrustUnknown())
	if !strings.Contains(unstated, "nobody stated") || !strings.Contains(unstated, "connection.TrustOfRun") {
		t.Errorf("unstated refusal = %q, want the wiring defect and the call that fixes it", unstated)
	}
	if strings.Contains(unstated, "no connector action at all") {
		t.Errorf("unstated refusal = %q, want it NOT to accuse a lane that is not involved", unstated)
	}
}

func refusal(t *testing.T, trust connection.ActionTrust) string {
	t.Helper()
	r := trustResolver(t, trust)
	_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.comment", "main")
	if err == nil {
		t.Fatal("want a refusal")
	}
	return err.Error()
}

// TestAnUntrustedResolutionNeverReadsTheTenantsConnectionStore pins the ORDER,
// which is part of the refusal and not a detail of it.
//
// A fork's graph can name any alias. If the verdict came after the lookup, the
// refusal would differ depending on whether the alias exists — "no probe
// connection named X" versus "refused" — and that difference is an oracle for
// the names a tenant's store holds, answered to code the tenant did not write.
// Nothing is unsealed either: openCredential is further down the same path.
// Since ADR-162 decision 6 the READ refuses on the same order, so an untrusted
// resolution reads the store zero times.
func TestAnUntrustedResolutionNeverReadsTheTenantsConnectionStore(t *testing.T) {
	st := &watchingStore{MemoryStore: connection.NewMemoryStore()}
	r := &connection.Resolver{
		Catalog:  connection.NewMemoryCatalog(trustProbePackage()),
		Store:    st,
		Sealer:   sealer(t),
		TenantID: "tenant-a",
		Trust:    connection.TrustOfRun(store.RunTrustFork),
	}
	c := conn("c1", "tenant-a", "main")
	c.GrantedScopes, c.ScopesKnown = []string{"issue"}, true
	seed(t, r, st, c)
	st.reads = 0

	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.comment", "guessed-alias"); err == nil {
		t.Fatal("a mutating action on an untrusted workspace must be refused")
	}
	if st.reads != 0 {
		t.Errorf("the resolution read the tenant's connections %d time(s) before refusing — the refusal must precede the lookup, or its wording tells a fork which aliases exist", st.reads)
	}

	// And the READ refuses on the same order: it never reaches the lookup
	// either, where #1802's contract let it resolve.
	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main"); err == nil {
		t.Fatal("a read on an untrusted workspace must be refused too (ADR-162 decision 6)")
	} else if !errors.Is(err, connection.ErrUntrustedAction) {
		t.Fatalf("read refusal = %v, want the trust refusal", err)
	}
	if st.reads != 0 {
		t.Errorf("reads = %d, want 0 — an untrusted resolution consults nobody's store", st.reads)
	}

	// The control that makes the counters mean anything: the tenant's own run
	// reads the store exactly once per resolution.
	r.Trust = connection.TrustOfRun(store.RunTrustDefault)
	st.reads = 0
	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main"); err != nil {
		t.Fatalf("read on the tenant's own run = %v, want it to resolve", err)
	}
	if st.reads != 1 {
		t.Errorf("reads = %d, want exactly 1 — the trusted read resolves through the store", st.reads)
	}
}

// watchingStore counts the tenant-scoped lookups a resolution performs.
type watchingStore struct {
	*connection.MemoryStore
	reads int
	lists int
}

func (s *watchingStore) ByAlias(ctx context.Context, tenantID, connector, alias string) (connection.Connection, error) {
	s.reads++
	return s.MemoryStore.ByAlias(ctx, tenantID, connector, alias)
}

func (s *watchingStore) List(ctx context.Context, tenantID, connector string) ([]connection.Connection, error) {
	s.lists++
	return s.MemoryStore.List(ctx, tenantID, connector)
}

// missResolver is a resolver whose store counts its lookups, seeded with
// exactly one connection — alias "main" — so a misspelt probe has something to
// leak.
func missResolver(t *testing.T, trust connection.ActionTrust) (*connection.Resolver, *watchingStore) {
	t.Helper()
	st := &watchingStore{MemoryStore: connection.NewMemoryStore()}
	r := &connection.Resolver{
		Catalog:  connection.NewMemoryCatalog(trustProbePackage()),
		Store:    st,
		Sealer:   sealer(t),
		TenantID: "tenant-a",
		Trust:    trust,
	}
	c := conn("c1", "tenant-a", "main")
	c.GrantedScopes, c.ScopesKnown = []string{"issue"}, true
	seed(t, r, st, c)
	return r, st
}

// TestAnUntrustedRunLearnsNoAliasesFromAReadMiss is the alias half of #1802,
// the residual #2810's catalog half left behind.
//
// A run over code the tenant does not control is refused before the store is
// read, so its misspelt alias probes nothing — neither the lookup nor
// suggestAlias, whose whole function is to end the error with the aliases the
// tenant DOES hold. The gate reads the run's trust rather than the operation's
// effect, because the fact withheld — which connections exist — does not depend
// on what was asked.
func TestAnUntrustedRunLearnsNoAliasesFromAReadMiss(t *testing.T) {
	for _, tc := range []struct {
		name        string
		trust       connection.ActionTrust
		wantSuggest bool // the tenant's own run: naming what exists is its repair
	}{
		{"a fork-trust run is told nothing", connection.TrustOfRun(store.RunTrustFork), false},
		// Fail-closed like every other trust read: a resolver nobody stated
		// resolves nothing, and for the same reason learns nothing about what
		// exists.
		{"a resolver nobody stated is told nothing either", connection.TrustUnknown(), false},
		// The arm that makes the refusals above a WITHHELD list rather than a
		// fixture that has nothing to list.
		{"the tenant's own run keeps the suggestion", connection.TrustOfRun(store.RunTrustDefault), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st := missResolver(t, tc.trust)
			_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "mian")
			if err == nil {
				t.Fatal("a misspelt alias must be refused")
			}
			if !strings.Contains(err.Error(), `"mian"`) {
				t.Errorf("refusal = %v, want it to name the alias that was asked for — the repair still starts from what missed", err)
			}
			if !tc.wantSuggest {
				if strings.Contains(err.Error(), "main") || strings.Contains(err.Error(), "it has") {
					t.Errorf("refusal = %v, want it to carry no alias the tenant holds — the list is the inventory the run may not read", err)
				}
				if st.lists != 0 {
					t.Errorf("the miss read the tenant's alias list %d time(s) — a suggestion that is never served must not be probed for either", st.lists)
				}
				if st.reads != 0 {
					t.Errorf("the miss read the tenant's connections %d time(s) — an untrusted resolution consults nobody's store", st.reads)
				}
				return
			}
			if !strings.Contains(err.Error(), "it has: main") {
				t.Errorf("refusal = %v, want the suggestion — the operator's run is who the list exists for", err)
			}
			if st.lists != 1 {
				t.Errorf("lists = %d, want exactly 1 — the suggestion comes from a real probe of the store, not from nowhere", st.lists)
			}
		})
	}
}
