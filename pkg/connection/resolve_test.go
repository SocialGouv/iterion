package connection_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// probePackage is a two-operation connector: one anonymous-ish read and one
// operation with a real scope requirement, which is what the scope branches
// below need to differ on.
func probePackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL: spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{
				{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"},
				{ID: "other", Kind: spec.AuthAPIKey, In: "header", Name: "X-Other"},
			},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{
				{
					ID: "probe.issue.get", Resource: "issue", Verb: "get",
					HTTP:   spec.HTTPBinding{Method: "GET", Path: "/issues/{id}"},
					Effect: spec.EffectRead, Deterministic: true,
					Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
					Results: []spec.ResultCase{{Status: 200}},
					Security: []spec.SecurityRequirement{
						{Terms: []spec.SecurityTerm{{SchemeID: "token", Scopes: []string{"read:issue"}}}},
					},
				},
				{
					ID: "probe.issue.purge", Resource: "issue", Verb: "purge",
					HTTP:   spec.HTTPBinding{Method: "DELETE", Path: "/issues/{id}"},
					Effect: spec.EffectDelete, Deterministic: true,
					Params:  []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
					Results: []spec.ResultCase{{Status: 204}},
					Security: []spec.SecurityRequirement{
						{Terms: []spec.SecurityTerm{{SchemeID: "other", Scopes: []string{"admin"}}}},
					},
				},
			},
		}},
	}
}

func resolver(t *testing.T, store connection.Store) *connection.Resolver {
	t.Helper()
	return &connection.Resolver{
		Catalog:  connection.NewMemoryCatalog(probePackage()),
		Store:    store,
		Sealer:   sealer(t),
		TenantID: "tenant-a",
	}
}

// seed creates a connection carrying a real sealed token.
func seed(t *testing.T, r *connection.Resolver, store connection.Store, c connection.Connection) {
	t.Helper()
	sealed, err := connection.SealToken(r.Sealer, c.ID, "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	c.SealedPayload = sealed
	if err := store.Create(context.Background(), c); err != nil {
		t.Fatalf("create: %v", err)
	}
}

// TestResolveHandsTheExecutorWhatItNeeds is the happy path, and the assertion
// that matters is the BASE URL: it comes from the connection, not the package,
// because the same package serves a SaaS host and every self-hosted instance —
// and only the connection knows which one this tenant means.
func TestResolveHandsTheExecutorWhatItNeeds(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "main")
	c.BaseURL = "https://probe.internal"
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	pkg, op, cred, baseURL, err := r.ResolveAction(context.Background(), "probe.issue.get", "main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if pkg.Connector.ID != "probe" || op.ID != "probe.issue.get" {
		t.Errorf("resolved %s / %s", pkg.Connector.ID, op.ID)
	}
	if cred.Value != "the-token" || cred.SchemeID != "token" {
		t.Errorf("credential = %+v, want the sealed token under its scheme", cred)
	}
	if baseURL != "https://probe.internal" {
		t.Errorf("baseURL = %q, want the CONNECTION's instance, not the package default", baseURL)
	}

}

// TestAnOriginlessRecordIsRefusedRatherThanCompleted. Falling back to the
// package meant "whatever it says when the call happens", so replacing the
// package — or shadowing it with a project-tier one — sent an existing
// credential to a different host with nothing in the run saying so.
//
// The record is handed over by a STUB rather than created, and that is the
// point: `Connection.Validate` now refuses an originless record, so no store
// will make one. What can still produce one is a file written by hand or by an
// older build — `FileStore.reload` does not validate what it loads — and this
// refusal is the last thing standing between such a record and a credential on
// the wire. Seeding through `Create` would test the guard one layer up and
// leave this one unexercised.
func TestAnOriginlessRecordIsRefusedRatherThanCompleted(t *testing.T) {
	c := conn("c2", "tenant-a", "saas")
	c.BaseURL = ""
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true

	r := resolver(t, originlessStore{conn: c})
	sealed, err := connection.SealToken(r.Sealer, c.ID, "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	c.SealedPayload = sealed
	r.Store = originlessStore{conn: c}

	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "saas"); err == nil {
		t.Error("a record that names no instance must be refused — nothing says where its credential may be sent")
	}
}

// originlessStore hands back one record verbatim, without the validation every
// real store applies on the way in.
type originlessStore struct{ conn connection.Connection }

func (s originlessStore) Create(context.Context, connection.Connection) error { return nil }
func (s originlessStore) Get(_ context.Context, _, _ string) (connection.Connection, error) {
	return s.conn, nil
}
func (s originlessStore) ByAlias(_ context.Context, _, _, _ string) (connection.Connection, error) {
	return s.conn, nil
}
func (s originlessStore) List(_ context.Context, _, _ string) ([]connection.Connection, error) {
	return []connection.Connection{s.conn}, nil
}
func (s originlessStore) Update(context.Context, connection.Connection) error { return nil }
func (s originlessStore) Delete(_ context.Context, _, _ string) error         { return nil }

// TestUnknownScopesAreNeitherGrantedNorRefused is F7's distinction, made
// operational.
//
// Most providers never enumerate what a PAT carries. Reading the resulting
// empty list as "no scopes" refuses every such connection; reading it as "all
// scopes" authorises whatever is asked. The third state is the only honest
// one, and it has to survive all the way to the executor — collapsing it at
// the last step would be the same bug in a different place.
func TestUnknownScopesAreNeitherGrantedNorRefused(t *testing.T) {
	ctx := context.Background()

	t.Run("an unstated grant does not refuse a matching scheme", func(t *testing.T) {
		store := connection.NewMemoryStore()
		r := resolver(t, store)
		c := conn("c1", "tenant-a", "main") // ScopesKnown false, no scopes
		seed(t, r, store, c)

		_, _, cred, _, err := r.ResolveAction(ctx, "probe.issue.get", "main")
		if err != nil {
			t.Fatalf("a credential whose grant nobody stated must still be usable: %v", err)
		}
		// And the executor must keep seeing "unknown", not an empty grant.
		if cred.Scopes != nil {
			t.Errorf("cred.Scopes = %v, want nil so the executor still reads the grant as unknown", cred.Scopes)
		}
	})

	t.Run("an unstated grant does not excuse the wrong scheme", func(t *testing.T) {
		store := connection.NewMemoryStore()
		r := resolver(t, store)
		seed(t, r, store, conn("c1", "tenant-a", "main")) // holds "token"

		// probe.issue.purge requires the "other" scheme. Unknown scopes must
		// not turn that into a pass: the conjunction is still checked.
		_, _, _, _, err := r.ResolveAction(ctx, "probe.issue.purge", "main")
		if err == nil {
			t.Fatal("unknown scopes must not excuse a scheme the operation does not accept")
		}
		if !strings.Contains(err.Error(), "unknown") {
			t.Errorf("the refusal should say the grant was unknown so only the scheme was checked: %v", err)
		}
	})

	t.Run("a stated grant is really checked", func(t *testing.T) {
		store := connection.NewMemoryStore()
		r := resolver(t, store)
		c := conn("c1", "tenant-a", "main")
		c.GrantedScopes, c.ScopesKnown = []string{"read:other"}, true // not read:issue
		seed(t, r, store, c)

		_, _, _, _, err := r.ResolveAction(ctx, "probe.issue.get", "main")
		if err == nil {
			t.Fatal("a STATED grant missing the operation's scope must be refused")
		}
		if !strings.Contains(err.Error(), "read:issue") {
			t.Errorf("the refusal must name the missing scope: %v", err)
		}
	})
}

// TestACapabilityIsRequiredForTheUSE. A deterministic node and an agent facade
// are different uses of one credential, and an operator may legitimately allow
// one and not the other — the whole reason capabilities are explicit rather
// than implied by the connection existing.
func TestACapabilityIsRequiredForTheUSE(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "main")
	c.Capabilities = []connection.Capability{connection.CapAgent} // agent only
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	_, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main")
	if err == nil {
		t.Fatal("an agent-only connection must not serve a deterministic node")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("the refusal must name the capability that is missing: %v", err)
	}
}

// TestARevokedConnectionRefuses, while a DEGRADED one still serves: degrading
// to unusable on the first hiccup is how a fleet loses a working credential.
func TestARevokedConnectionRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status  connection.Status
		wantErr bool
	}{
		{connection.StatusActive, false},
		{connection.StatusDegraded, false},
		{connection.StatusRevoked, true},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			store := connection.NewMemoryStore()
			r := resolver(t, store)
			c := conn("c1", "tenant-a", "main")
			c.Status = tc.status
			c.StatusReason = "the operator disconnected it"
			c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
			seed(t, r, store, c)

			_, _, _, _, err := r.ResolveAction(ctx, "probe.issue.get", "main")
			if tc.wantErr && err == nil {
				t.Error("a revoked connection must refuse")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("status %q must still serve: %v", tc.status, err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "disconnected") {
				t.Errorf("the refusal must carry the recorded reason: %v", err)
			}
		})
	}
}

// TestAnExpiredConnectionRefuses. iterion holds the expiry, so iterion names
// it: spending the call to have the vendor answer 401 reads like a bad token
// and sends an operator to rotate one that is merely out of date. The two
// serving cases are the falsifier — a guard that refused everything would pass
// the first assertion alone.
func TestAnExpiredConnectionRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		expiresAt time.Time
		wantErr   bool
	}{
		{"no expiry", time.Time{}, false},
		{"still valid", time.Now().Add(time.Hour), false},
		{"expired", time.Now().Add(-time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := connection.NewMemoryStore()
			r := resolver(t, store)
			c := conn("c1", "tenant-a", "main")
			c.ExpiresAt = tc.expiresAt
			c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
			seed(t, r, store, c)

			_, _, _, _, err := r.ResolveAction(ctx, "probe.issue.get", "main")
			if tc.wantErr {
				if err == nil {
					t.Fatal("an expired connection must refuse before the call is made")
				}
				// The instant, not just the word: an operator has to be able
				// to tell an expiry apart from a revocation.
				if !strings.Contains(err.Error(), "expired at") {
					t.Errorf("the refusal must name the expiry: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s must still serve: %v", tc.name, err)
			}
		})
	}
}

// TestAResolverWithoutATenantRefuses. An empty tenant matches the records that
// also have none, which on a shared deployment is whatever a migration left
// behind — so it is refused rather than defaulted.
func TestAResolverWithoutATenantRefuses(t *testing.T) {
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	r.TenantID = ""
	if _, _, _, _, err := r.ResolveAction(context.Background(), "probe.issue.get", "main"); err == nil {
		t.Fatal("a resolver with no tenant must refuse to read anything")
	}
}

// TestTheDiagnosticsNameTheFIX. Every one of these surfaces to an operator as
// "the node failed", so the message is the whole of what they get.
func TestTheDiagnosticsNameTheFIX(t *testing.T) {
	ctx := context.Background()
	store := connection.NewMemoryStore()
	r := resolver(t, store)
	c := conn("c1", "tenant-a", "prod")
	c.GrantedScopes, c.ScopesKnown = []string{"read:issue"}, true
	seed(t, r, store, c)

	for _, tc := range []struct {
		name, action, alias string
		want                []string
	}{
		{
			name: "a mistyped verb names its neighbours",
			// The connector and resource are right; only the verb is wrong.
			action: "probe.issue.gett", alias: "prod",
			want: []string{"probe.issue.get"},
		},
		{
			name:   "an unknown alias lists the ones that exist",
			action: "probe.issue.get", alias: "staging",
			want: []string{"prod"},
		},
		{
			name:   "an unknown connector says so plainly",
			action: "nowhere.issue.get", alias: "prod",
			want: []string{"nowhere"},
		},
		{
			name:   "an action that is not an operation id says what one looks like",
			action: "notanid", alias: "prod",
			want: []string{"connector.resource.verb"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := r.ResolveAction(ctx, tc.action, tc.alias)
			if err == nil {
				t.Fatal("want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestResolverSatisfiesTheExecutorSeam is a COMPILE-TIME assertion, and it is
// the one thing in this file that would otherwise be discovered at wiring
// time.
//
// `model.ConnectorResolver` is declared where it is consumed, so nothing forces
// this package to match it: the two compile independently and would keep
// compiling right up to the line that tries to pass one to the other. Asserting
// it here means a change to either side fails in the package that changed.
func TestResolverSatisfiesTheExecutorSeam(t *testing.T) {
	var _ model.ConnectorResolver = (*connection.Resolver)(nil)
}
