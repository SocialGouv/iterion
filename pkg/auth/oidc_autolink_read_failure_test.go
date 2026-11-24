package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// canAutoLink used to answer `false` for every failed read, and the caller
// turned that into ErrLinkRequiresConsent — a 409 whose remedy ("sign in with
// your password, then link") is a DEAD END for the account an SSO login
// matched, since that account may have no password. So a store blip presented
// as a policy decision, permanently, with the cause logged nowhere. The three
// reads, three isolates; the policy refusals keep their 409 in their own
// tests (TestLoginForOrg_NoAutoLinkUnverifiedDomain,
// TestLoginForOrg_NoAutoLinkSuperAdmin).

// failingDomainStore answers errStoreUnreachable from the auto-link gate's
// lookup and delegates the rest.
type failingDomainStore struct {
	orgsso.DomainStore
	err error
}

func (f failingDomainStore) IsVerifiedForTenant(context.Context, string, string) (bool, error) {
	return false, f.err
}

// seedAutoLinkTarget wires opt-in auto-link with a VERIFIED domain and an
// existing account the SSO identity matches by email: everything the policy
// needs to say yes, so a refusal can only come from a read.
func seedAutoLinkTarget(t *testing.T, svc *Service, teamID, email string) {
	t.Helper()
	ctx := context.Background()
	domains := enableAutoLink(t, svc)
	now := time.Now()
	if err := domains.Create(ctx, orgsso.VerifiedDomain{
		ID: "d1", TenantID: teamID, Domain: "acme.example", VerifiedAt: &now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateUserAndPersonalTeam(ctx, email, "Bob", "correcthorse", false, identity.UserStatusActive); err != nil {
		t.Fatal(err)
	}
}

func TestAutoLink_aFailedDomainReadIsNotAPermanentConsentRefusal(t *testing.T) {
	ctx := context.Background()
	svc, teamID := orgServiceFixture(t, identity.RoleMember)
	seedAutoLinkTarget(t, svc, teamID, "bob@acme.example")
	svc.domains = failingDomainStore{DomainStore: svc.domains, err: errStoreUnreachable}

	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example"}
	_, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
	if errors.Is(err, ErrLinkRequiresConsent) {
		t.Fatalf("a failed domain read answered the policy's 409: %v", err)
	}
	if !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("err = %v, want the store failure of the domain read", err)
	}
	if !errors.Is(err, errStoreUnreachable) || !containsAll(err.Error(), "acme.example", teamID) {
		t.Errorf("the failure does not name what could not be read: %v", err)
	}
	// Nothing was linked: the refusal stands, it just says why.
	if _, gerr := svc.store.GetOIDCLink(ctx, "oidc-org-prov1", "kc-bob"); !errors.Is(gerr, identity.ErrNotFound) {
		t.Errorf("a failed read linked the identity anyway (%v)", gerr)
	}
}

func TestAutoLink_aFailedGrantReadIsNotAPermanentConsentRefusal(t *testing.T) {
	ctx := context.Background()
	svc, teamID := orgServiceFixture(t, identity.RoleMember)
	seedAutoLinkTarget(t, svc, teamID, "bob@acme.example")
	// The grants of the account the auto-link would take over: the read that
	// decides whether it is privileged elsewhere.
	gate := newNthCallGate(svc.store, errStoreUnreachable).fail("ListMembershipsByUser", 1)
	svc.SetStoreForTest(gate)

	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example"}
	_, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
	if errors.Is(err, ErrLinkRequiresConsent) {
		t.Fatalf("a failed grant read answered the policy's 409: %v", err)
	}
	if !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("err = %v, want the store failure of the grant read", err)
	}
	if _, gerr := svc.store.GetOIDCLink(ctx, "oidc-org-prov1", "kc-bob"); !errors.Is(gerr, identity.ErrNotFound) {
		t.Errorf("a failed read linked the identity anyway (%v)", gerr)
	}
}

func TestAutoLink_aFailedTeamReadIsNotAPermanentConsentRefusal(t *testing.T) {
	ctx := context.Background()
	svc, teamID := orgServiceFixture(t, identity.RoleMember)
	seedAutoLinkTarget(t, svc, teamID, "bob@acme.example")
	// The account owns its personal team, so the privileged-grant loop reads
	// a team to decide whether that ownership confers cross-tenant power.
	gate := newNthCallGate(svc.store, errStoreUnreachable).fail("GetTeam", 1)
	svc.SetStoreForTest(gate)

	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example"}
	_, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
	if errors.Is(err, ErrLinkRequiresConsent) {
		t.Fatalf("a failed team read answered the policy's 409: %v", err)
	}
	if !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("err = %v, want the store failure of the team read", err)
	}
	if _, gerr := svc.store.GetOIDCLink(ctx, "oidc-org-prov1", "kc-bob"); !errors.Is(gerr, identity.ErrNotFound) {
		t.Errorf("a failed read linked the identity anyway (%v)", gerr)
	}
}

// TestAutoLink_theVerifiedDomainStillLinks is the mirror in this file's own
// isolate: with every read answering, the opt-in plus the verified domain
// still auto-links — the three failures above must not have turned the gate
// into a refusal for everyone.
func TestAutoLink_theVerifiedDomainStillLinks(t *testing.T) {
	ctx := context.Background()
	svc, teamID := orgServiceFixture(t, identity.RoleMember)
	seedAutoLinkTarget(t, svc, teamID, "bob@acme.example")

	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example"}
	if _, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip"); err != nil {
		t.Fatalf("the auto-link that every read allows failed: %v", err)
	}
	if _, err := svc.store.GetOIDCLink(ctx, "oidc-org-prov1", "kc-bob"); err != nil {
		t.Errorf("the auto-link wrote no link: %v", err)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		found := false
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
