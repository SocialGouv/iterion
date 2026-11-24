package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAuthorizeSpaceWrite_GatesOnlyTheGlobalSpace pins the gate's shape: the
// instance-wide space is fail-closed, every team-scoped space is untouched,
// and the grant is what flips it (not the ref, which carries no principal).
func TestAuthorizeSpaceWrite_GatesOnlyTheGlobalSpace(t *testing.T) {
	granted := WithPlatformWriteAuthority(context.Background())
	bare := context.Background()

	for _, vis := range []Visibility{
		VisibilityPrivate, VisibilityBot, VisibilityProject,
		VisibilityCrossProject, VisibilityUser, VisibilityOrg,
	} {
		ref := SpaceRef{Visibility: vis, TenantID: "t1", ProjectID: "p", BotID: "b", UserID: "u", Name: "n"}
		if err := AuthorizeSpaceWrite(bare, ref); err != nil {
			t.Errorf("visibility %q reaches only its own org and must need no grant, got %v", vis, err)
		}
	}

	globalRef := SpaceRef{Visibility: VisibilityGlobal, Name: "catalogue"}
	err := AuthorizeSpaceWrite(bare, globalRef)
	if !errors.Is(err, ErrGlobalWriteForbidden) {
		t.Errorf("an ungranted context mutating the global space: err=%v, want ErrGlobalWriteForbidden", err)
	}
	if err != nil && !strings.Contains(err.Error(), "catalogue") {
		t.Errorf("the refusal must name the space it refuses: %q", err.Error())
	}
	if err := AuthorizeSpaceWrite(granted, globalRef); err != nil {
		t.Errorf("a platform-authorized context must keep the capability, got %v", err)
	}
}

// TestPlatformWriteAuthorized_NilContextIsRefused: the predicate is the gate,
// so it must answer "no" to the degenerate input rather than panic — a nil
// ctx reaching a store is a bug, not a grant.
func TestPlatformWriteAuthorized_NilContextIsRefused(t *testing.T) {
	if PlatformWriteAuthorized(nil) {
		t.Fatal("a nil context must not be read as platform authority")
	}
	//nolint:staticcheck // deliberately the degenerate input the gate must survive
	if err := AuthorizeSpaceWrite(nil, SpaceRef{Visibility: VisibilityGlobal, Name: "c"}); !errors.Is(err, ErrGlobalWriteForbidden) {
		t.Fatalf("nil ctx on a global ref: err=%v, want ErrGlobalWriteForbidden", err)
	}
}

// TestSpaceIDDelimiterIsRefusedInQualifiers: ID() joins the ref's axes with
// ':', so a qualifier carrying one makes two DIFFERENT refs spell the SAME
// id. The pair below is that collision, built from the caller-controlled
// query params of the REST surface (?project=, ?bot=, ?name=) — it must not
// be constructible.
func TestSpaceIDDelimiterIsRefusedInQualifiers(t *testing.T) {
	alias := SpaceRef{Visibility: VisibilityOrg, TenantID: "t1", ProjectID: "x:y", Name: "n"}
	other := SpaceRef{Visibility: VisibilityOrg, TenantID: "t1", ProjectID: "x", BotID: "y:", Name: "n"}
	if alias.ID() != other.ID() {
		t.Fatalf("premise lost: the two refs no longer alias (%q vs %q) — rebuild the collision before trusting the refusal", alias.ID(), other.ID())
	}
	for _, ref := range []SpaceRef{alias, other} {
		if err := ref.Validate(); err == nil {
			t.Errorf("ref %+v spells another space's id and was accepted", ref)
		}
	}
	// The name is caller-controlled too, and lands in the last component.
	if err := ValidateName("a:b"); err == nil {
		t.Error(`ValidateName("a:b") accepted the id delimiter in a space name`)
	}
	// A plain name and plain qualifiers stay legal.
	if err := (SpaceRef{Visibility: VisibilityOrg, TenantID: "t1", ProjectID: "x-y", Name: "n"}).Validate(); err != nil {
		t.Errorf("an ordinary ref must stay valid: %v", err)
	}
}

// TestQuotaError_NamesTheRefusingAggregate: a global write meets the
// tenant-less INSTANCE ceiling, and reporting that as "org quota exceeded"
// sends an operator to raise the wrong ceiling (a team's, which was never the
// bound). An empty Counter keeps the original wording, so every pre-existing
// refusal reads exactly as before.
func TestQuotaError_NamesTheRefusingAggregate(t *testing.T) {
	instance := (&QuotaError{Aggregate: true, Used: 1, Delta: 2, Quota: 3, Counter: InstanceUsageID}).Error()
	if !strings.Contains(instance, "instance quota exceeded") {
		t.Errorf("instance ceiling refusal = %q, want it to name the instance", instance)
	}
	org := (&QuotaError{Aggregate: true, Used: 1, Delta: 2, Quota: 3, Counter: "acme"}).Error()
	if !strings.Contains(org, "org acme quota exceeded") {
		t.Errorf("org ceiling refusal = %q, want it to name the org", org)
	}
	legacy := (&QuotaError{Aggregate: true, Used: 1, Delta: 2, Quota: 3}).Error()
	if legacy != "knowledge: org quota exceeded (used=1 delta=2 quota=3)" {
		t.Errorf("an unnamed aggregate refusal changed wording: %q", legacy)
	}
	space := (&QuotaError{Used: 1, Delta: 2, Quota: 3, Counter: "ignored"}).Error()
	if !strings.Contains(space, "space quota exceeded") {
		t.Errorf("a per-space refusal must stay a space refusal: %q", space)
	}
}
