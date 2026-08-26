package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// seedWatchOnlyRuntimeConn is a watch-only connection on the orchestrator's
// store, shaped like the ones Provision is normally handed.
func seedWatchOnlyRuntimeConn(t *testing.T, o *Orchestrator, sealer secrets.Sealer) Connection {
	t.Helper()
	c := seedConn(t, o, sealer)
	c.Purpose = PurposeSecurityRead
	if err := o.Connections.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Provision writes the bot bindings BEFORE its first forge call, the binding
// key is (tenant, bot, secret_name) and therefore TEAM-GLOBAL, and nothing
// rolls it back. So a watch-only connection accepted here does not merely fail
// — it repoints every repo of that bot at a token that cannot push, and the
// operator only sees "provision failed". Refusing at ensureManagedSecret is
// what makes that impossible: it runs before any binding is written.
func TestProvision_RefusesWatchOnlyBeforeWritingAnyBinding(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	conn := seedWatchOnlyRuntimeConn(t, o, sealer)

	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: conn.ID,
		RepoFullName: "grp/app",
		BotIDs:       []string{"review-pr"},
		ActorID:      "u1",
	})
	if err == nil {
		t.Fatal("Provision accepted a watch-only connection")
	}
	// The message has to name the cause: the operator picked this connection.
	if got := err.Error(); !strings.Contains(got, "watch-only") {
		t.Fatalf("error = %q, want it to name the watch-only role", got)
	}
	// Nothing was written — no managed secret, no binding, no integration.
	fresh, gerr := o.Connections.Get(context.Background(), conn.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if fresh.ManagedSecretID != "" {
		t.Fatalf("a managed secret was created for a watch-only connection: %s", fresh.ManagedSecretID)
	}
	if b, berr := o.Bindings.ListByTenant(context.Background(), "t1"); berr == nil && len(b) > 0 {
		t.Fatalf("bindings were written before the refusal: %+v", b)
	}
}

// EnsureManagedSecret is the same chokepoint reached by the repo-targeted
// launch path, which asks for a run's forge token.
func TestEnsureManagedSecret_RefusesWatchOnly(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	conn := seedWatchOnlyRuntimeConn(t, o, sealer)
	if _, err := o.EnsureManagedSecret(context.Background(), &conn, "launch"); err == nil {
		t.Fatal("EnsureManagedSecret handed a runtime token to a watch-only connection")
	}
}

// A watch-only App must never be the IMPLICIT answer to "which app for this
// host". This listing is oldest-first, so a team that registered its watch-only
// App first — the natural order for a security-first onboarding — would have
// every default install and every OAuth connect resolve to an App with no
// rights, producing a connection labelled runtime that can do nothing.
func TestGetByInstance_SkipsWatchOnlyApps(t *testing.T) {
	st := NewMemoryOAuthAppStore()
	ctx := context.Background()
	base := CanonicalBaseURL(ProviderGitHub, "")
	mk := func(id, owner string, watch bool, at time.Time) ForgeOAuthApp {
		return ForgeOAuthApp{
			ID: id, TenantID: "t1", Provider: ProviderGitHub, ForgeBaseURL: base,
			ClientID: id, OwnerLogin: owner, SecurityReadOnly: watch, CreatedAt: at,
		}
	}
	// Watch-only registered FIRST (it is the oldest).
	if err := st.Create(ctx, mk("watch", "OrgA", true, time.Unix(1, 0))); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, mk("runtime", "OrgB", false, time.Unix(2, 0))); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetByInstance(ctx, "t1", ProviderGitHub, base)
	if err != nil {
		t.Fatalf("GetByInstance: %v", err)
	}
	if got.ID != "runtime" {
		t.Fatalf("default app = %q, want the runtime one", got.ID)
	}

	// With ONLY a watch-only App, the answer is "none" — a clean error beats
	// silently authorizing against an App that holds nothing.
	only := NewMemoryOAuthAppStore()
	if err := only.Create(ctx, mk("watch", "OrgA", true, time.Unix(1, 0))); err != nil {
		t.Fatal(err)
	}
	if _, err := only.GetByInstance(ctx, "t1", ProviderGitHub, base); !errors.Is(err, ErrOAuthAppNotFound) {
		t.Fatalf("GetByInstance err = %v, want ErrOAuthAppNotFound", err)
	}
}

// One org legitimately hosts a runtime App AND a watch-only App. Without the
// role in the uniqueness key the second collides — and the collision is only
// discovered at the manifest callback, after GitHub created the App and handed
// back the private key it never reissues, which iterion then drops on the floor.
func TestOAuthAppUniqueness_AllowsRuntimeAndWatchOnlyOnTheSameOrg(t *testing.T) {
	st := NewMemoryOAuthAppStore()
	ctx := context.Background()
	base := CanonicalBaseURL(ProviderGitHub, "")
	mk := func(id string, watch bool) ForgeOAuthApp {
		return ForgeOAuthApp{
			ID: id, TenantID: "t1", Provider: ProviderGitHub, ForgeBaseURL: base,
			ClientID: id, OwnerLogin: "SocialGouv", SecurityReadOnly: watch,
		}
	}
	if err := st.Create(ctx, mk("runtime", false)); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, mk("watch", true)); err != nil {
		t.Fatalf("the watch-only App collided with the runtime one: %v", err)
	}
	// Two apps of the SAME role on one org still collide.
	if err := st.Create(ctx, mk("runtime-2", false)); !errors.Is(err, ErrOAuthAppExists) {
		t.Fatalf("second runtime app err = %v, want ErrOAuthAppExists", err)
	}
	if err := st.Create(ctx, mk("watch-2", true)); !errors.Is(err, ErrOAuthAppExists) {
		t.Fatalf("second watch-only app err = %v, want ErrOAuthAppExists", err)
	}
}
