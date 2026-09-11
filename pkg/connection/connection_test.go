package connection_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func sealer(t *testing.T) secrets.Sealer {
	t.Helper()
	s, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// conn builds a valid connection for tenant t1 unless a case changes it.
func conn(id, tenant, alias string) connection.Connection {
	return connection.Connection{
		ID: id, TenantID: tenant, Connector: "probe", Alias: alias,
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: []connection.Capability{connection.CapAction},
		// A real connection always carries its origin — `connections add`
		// resolves and pins it at creation, so nothing can later redirect the
		// credential. The fixture carries one for the same reason a stub must
		// bear every term of the real producer: without it these tests would
		// exercise a shape no store can hold.
		BaseURL: "https://probe.example",
		// And its PLACEMENT, for the same reason and from the same package:
		// `connections add` pins what the scheme said, so a later package
		// cannot move the credential while keeping the scheme's name. A
		// fixture without one is a shape no store will hold.
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization",
		},
	}
}

// TestAConnectionIsUNREACHABLEFromAnotherTenant is the reason this package
// exists rather than a widened `pkg/forge`.
//
// `forge.ConnectionStore.Get(ctx, id)` filters on `_id` alone, so any caller
// holding an id — an API path parameter, a webhook, a run — reads any tenant's
// connection, and nothing at the call site looks wrong. Here the tenant is a
// positional argument, so the hole cannot come back by forgetting a filter:
// a caller must produce a tenant, and producing the wrong one is visible.
//
// The other tenant's record must read as NOT FOUND, never as a distinguishable
// refusal: an error that says "exists but not yours" enumerates ids for
// whoever is probing.
func TestAConnectionIsUNREACHABLEFromAnotherTenant(t *testing.T) {
	ctx := context.Background()
	store := connection.NewMemoryStore()
	if err := store.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := store.Get(ctx, "tenant-a", "c1"); err != nil {
		t.Fatalf("the owning tenant must read its own connection: %v", err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := store.Get(ctx, "tenant-b", "c1"); return err }},
		{"ByAlias", func() error { _, err := store.ByAlias(ctx, "tenant-b", "probe", "main"); return err }},
		{"Delete", func() error { return store.Delete(ctx, "tenant-b", "c1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, connection.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound — another tenant's record must be indistinguishable from an absent one", err)
			}
		})
	}
	// List must not leak it either.
	got, err := store.List(ctx, "tenant-b", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d connections for a tenant that owns none", len(got))
	}

	// And an update may not MOVE a connection between tenants: that would
	// grant one tenant's credential to another, written as an ordinary edit.
	moved := conn("c1", "tenant-b", "main")
	if err := store.Update(ctx, moved); !errors.Is(err, connection.ErrNotFound) {
		t.Errorf("update across tenants = %v, want ErrNotFound", err)
	}
	still, err := store.Get(ctx, "tenant-a", "c1")
	if err != nil || still.TenantID != "tenant-a" {
		t.Errorf("the connection must still belong to tenant-a, got %+v (%v)", still.TenantID, err)
	}
}

// TestValidateRefusesAnIncoherentConnection. Each case states what the refusal
// prevents, because a validation nobody can explain gets deleted the first
// time it is inconvenient.
func TestValidateRefusesAnIncoherentConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*connection.Connection)
		wantMsg string
	}{
		{
			// Prevents: a connection reachable by everyone — the exact hole
			// the tenant-scoped reads exist to close.
			name:    "no tenant",
			mutate:  func(c *connection.Connection) { c.TenantID = "" },
			wantMsg: "missing tenant",
		},
		{
			// Prevents: a `.bot` unable to name it.
			name:    "no alias",
			mutate:  func(c *connection.Connection) { c.Alias = "" },
			wantMsg: "missing alias",
		},
		{
			// Prevents: a guessed scheme, whose wrong placement is a 401 that
			// reads like a bad credential.
			name:    "no scheme",
			mutate:  func(c *connection.Connection) { c.SchemeID = "" },
			wantMsg: "names no auth scheme",
		},
		{
			// Prevents: a record meaning "whatever the package says at call
			// time", which lets a replaced or shadowed package redirect an
			// existing credential to another host.
			name:    "no instance URL",
			mutate:  func(c *connection.Connection) { c.BaseURL = "" },
			wantMsg: "names no instance URL",
		},
		{
			// Prevents the value that slips through unaided: `url.Parse`
			// accepts it without error and returns an empty Host, so the
			// failure surfaces at the transport as `unsupported protocol
			// scheme ""` — naming neither the connection nor the field.
			name:    "instance URL with no scheme",
			mutate:  func(c *connection.Connection) { c.BaseURL = "git.example.com" },
			wantMsg: "needs an http or https scheme",
		},
		{
			name:    "instance URL with no host",
			mutate:  func(c *connection.Connection) { c.BaseURL = "https://" },
			wantMsg: "names no host",
		},
		{
			// Prevents: a connection usable for everything by saying nothing.
			name:    "no capability",
			mutate:  func(c *connection.Connection) { c.Capabilities = nil },
			wantMsg: "declares no capability",
		},
		{
			// Prevents an older replica silently NARROWING a connection an
			// operator widened with a newer one.
			name: "unknown capability",
			mutate: func(c *connection.Connection) {
				c.Capabilities = []connection.Capability{"root"}
			},
			wantMsg: "unknown capability",
		},
		{
			// Prevents: the unknown/none ambiguity coming back through the
			// record itself.
			name: "scopes listed but marked unknown",
			mutate: func(c *connection.Connection) {
				c.GrantedScopes = []string{"repo"}
				c.ScopesKnown = false
			},
			wantMsg: "says they are unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := conn("c1", "tenant-a", "main")
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("want a refusal mentioning %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestADuplicateAliasIsRefused: the alias is how a `.bot` names a connection,
// so two of them would make the workflow's choice depend on map iteration
// order — a run that reaches a different vendor account on different days.
func TestADuplicateAliasIsRefused(t *testing.T) {
	ctx := context.Background()
	store := connection.NewMemoryStore()
	if err := store.Create(ctx, conn("c1", "tenant-a", "main")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Create(ctx, conn("c2", "tenant-a", "MAIN")); !errors.Is(err, connection.ErrExists) {
		t.Errorf("err = %v, want ErrExists — aliases differing only in case name the same connection", err)
	}
	// But the same alias under ANOTHER tenant, or another connector, is a
	// different connection and must be allowed.
	other := conn("c3", "tenant-b", "main")
	if err := store.Create(ctx, other); err != nil {
		t.Errorf("another tenant's `main` is a different connection: %v", err)
	}
	sibling := conn("c4", "tenant-a", "main")
	sibling.Connector = "other"
	if err := store.Create(ctx, sibling); err != nil {
		t.Errorf("another connector's `main` is a different connection: %v", err)
	}
}

// TestASealedCredentialCannotBeTransplanted: the AAD binds the blob to its
// record id. Without it, anyone able to write a connection row could point a
// low-privilege connection at a high-privilege connection's sealed bytes and
// the seal would open happily.
func TestASealedCredentialCannotBeTransplanted(t *testing.T) {
	s := sealer(t)
	sealed, err := connection.SealToken(s, "c1", "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	ctx := context.Background()
	store := connection.NewMemoryStore()
	victim := conn("c2", "tenant-a", "main")
	victim.SealedPayload = sealed // c1's bytes on c2's record
	if err := store.Create(ctx, victim); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &connection.Resolver{
		Catalog: connection.NewMemoryCatalog(probePackage()),
		Store:   store, Sealer: s, TenantID: "tenant-a",
	}
	if _, _, _, _, err := r.ResolveAction(ctx, "probe.issue.get", "main"); err == nil {
		t.Fatal("a blob sealed for another connection must not open")
	}
}
