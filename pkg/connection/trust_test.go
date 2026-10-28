package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
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
	c.GrantedScopes, c.ScopesKnown = []string{"issue"}, true
	seed(t, r, st, c)
	return r
}

// TestAMutatingActionIsRefusedOnAnUntrustedWorkspace is #1802.
//
// A `tool … action:` node authenticates with the TENANT's binding. Nothing on
// that path asked who wrote the workflow, so a run over a workspace authored
// outside the tenant — the fork review lane's store.RunTrustFork — would have
// spent the tenant's credential on whatever the fork's own graph declared. The
// lane's contract is a review: it reads.
//
// The TRUSTED arm is what makes the untrusted arms mean something: the same
// connection, the same operation, resolving all the way to the credential. The
// refusal is the forbidden alternative, not a fixture that cannot resolve at
// all.
func TestAMutatingActionIsRefusedOnAnUntrustedWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trust   connection.ActionTrust
		action  string
		wantErr error // nil: must resolve all the way to the credential
	}{
		{"a trusted run resolves the mutating operation", connection.TrustOfRun(store.RunTrustDefault), "probe.issue.comment", nil},
		{"a fork-trust run does not", connection.TrustOfRun(store.RunTrustFork), "probe.issue.comment", connection.ErrUntrustedMutation},
		// The predicate is Trusted(), not IsFork(): a value this binary does
		// not recognise — a document written by a newer replica mid-rollout, a
		// hand-edited row — must lose the credential too. Mutating the check
		// to t.trust.IsFork() leaves this arm, and only this arm, red.
		{"a trust value this build does not know does not either", connection.TrustOfRun(store.RunTrust("vendored")), "probe.issue.comment", connection.ErrUntrustedMutation},
		// The zero value is what a builder that FORGOT the field produces, and
		// store.RunTrust's own zero value is the trusted one — so this arm is
		// the whole reason trust is not a bare store.RunTrust here.
		{"a resolver nobody told does not either (the zero value)", connection.ActionTrust{}, "probe.issue.comment", connection.ErrTrustUnstated},
		{"nor one that says it cannot tell", connection.TrustUnknown(), "probe.issue.comment", connection.ErrTrustUnstated},
		// An effect this build does not know fails SAFE, at this site too:
		// Mutating() admits only `read`, so a typo cannot buy a mutation.
		{"an effect this build does not know is refused as mutating", connection.TrustOfRun(store.RunTrustFork), "probe.issue.reed", connection.ErrUntrustedMutation},
		// And a review lane READS: the same records, the same credential.
		{"a fork-trust run still resolves the read", connection.TrustOfRun(store.RunTrustFork), "probe.issue.get", nil},
		{"an unstated resolver still resolves the read", connection.TrustUnknown(), "probe.issue.get", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := trustResolver(t, tc.trust)
			_, op, cred, baseURL, err := r.ResolveAction(context.Background(), tc.action, "main")

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
				t.Fatalf("resolve %s handed back a credential (%+v) for a mutating effect on a workspace the tenant does not control", tc.action, cred)
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
// "This lane resolves reads only" is read by an OPERATOR whose fork run was
// refused, and nothing is wrong. "Nobody stated the trust" is read by a
// DEVELOPER whose builder forgot to say, and something is. A single message
// for both would send each of them to the other's fix.
func TestTheTwoRefusalsSayDifferentThingsToDifferentReaders(t *testing.T) {
	forkErr := refusal(t, connection.TrustOfRun(store.RunTrustFork))
	if !strings.Contains(forkErr, "read operations only") || !strings.Contains(forkErr, `trust="fork"`) {
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

	unstated := refusal(t, connection.TrustUnknown())
	if !strings.Contains(unstated, "nobody stated") || !strings.Contains(unstated, "connection.TrustOfRun") {
		t.Errorf("unstated refusal = %q, want the wiring defect and the call that fixes it", unstated)
	}
	if strings.Contains(unstated, "read operations only") {
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

// TestAnUntrustedMutationNeverReadsTheTenantsConnectionStore pins the ORDER,
// which is part of the refusal and not a detail of it.
//
// A fork's graph can name any alias. If the verdict came after the lookup, the
// refusal would differ depending on whether the alias exists — "no probe
// connection named X" versus "refused" — and that difference is an oracle for
// the names a tenant's store holds, answered to code the tenant did not write.
// Nothing is unsealed either: openCredential is further down the same path.
func TestAnUntrustedMutationNeverReadsTheTenantsConnectionStore(t *testing.T) {
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

	// And the READ it is allowed still goes through the lookup: the counter
	// proves the order, not a resolution that never happens.
	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main"); err != nil {
		t.Fatalf("read on a fork-trust run = %v, want it to resolve", err)
	}
	if st.reads != 1 {
		t.Errorf("reads = %d, want exactly 1 — the allowed read resolves through the same store", st.reads)
	}
}

// watchingStore counts the tenant-scoped lookups a resolution performs.
type watchingStore struct {
	*connection.MemoryStore
	reads int
}

func (s *watchingStore) ByAlias(ctx context.Context, tenantID, connector, alias string) (connection.Connection, error) {
	s.reads++
	return s.MemoryStore.ByAlias(ctx, tenantID, connector, alias)
}
