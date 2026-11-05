package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// countingGrants is the memory identity store counting the membership
// writes that reach it.
type countingGrants struct {
	*identity.MemoryStore
	mu     sync.Mutex
	writes int
}

func (c *countingGrants) UpsertMembership(ctx context.Context, m identity.Membership) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.MemoryStore.UpsertMembership(ctx, m)
}

func (c *countingGrants) UpsertOrgMembership(ctx context.Context, m identity.OrgMembership) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.MemoryStore.UpsertOrgMembership(ctx, m)
}

func (c *countingGrants) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// TestGrantGuard_NoMembershipIsWrittenForAnErasedAccount: a grant to an
// account that already carries the erased status — the team and the org
// kind, through the store every grant of the service and the server goes
// through — is refused before its write reaches the store.
//
// The mutation this reddens: drop the read before the write (refuseErased)
// — the write lands, and only the read after it takes it back.
func TestGrantGuard_NoMembershipIsWrittenForAnErasedAccount(t *testing.T) {
	ctx := context.Background()
	st := &countingGrants{MemoryStore: identity.NewMemoryStore()}
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	ivy, err := svc.Register(ctx, "ivy@example.org", "correcthorse", "Ivy", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := st.MarkUserErased(ctx, ivy.User.ID); err != nil {
		t.Fatal(err)
	}
	before := st.count()
	now := time.Now().UTC()
	if err := svc.Store().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: ivy.User.ID, OrgID: "o1", Role: identity.OrgRoleOwner, JoinedAt: now}); !errors.Is(err, ErrGrantRefused) {
		t.Fatalf("an org grant to the erased account = %v, want ErrGrantRefused", err)
	}
	if err := svc.Store().UpsertMembership(ctx, identity.Membership{UserID: ivy.User.ID, TeamID: "t1", Role: identity.RoleOwner, JoinedAt: now}); !errors.Is(err, ErrGrantRefused) {
		t.Fatalf("a team grant to the erased account = %v, want ErrGrantRefused", err)
	}
	if n := st.count() - before; n != 0 {
		t.Fatalf("%d membership write(s) reached the store for an erased account, want none", n)
	}
}

// markAfterGrantStore is the memory identity store on which the erasure's
// mark lands right after the first team membership written for userID — in
// CreateOrgFor, the last grant it writes: a grant whose request read the
// account before the mark, every read before a write answered "active".
type markAfterGrantStore struct {
	*identity.MemoryStore
	userID string
	marked bool
}

func (m *markAfterGrantStore) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if err := m.MemoryStore.UpsertMembership(ctx, mb); err != nil {
		return err
	}
	if mb.UserID == m.userID && !m.marked {
		m.marked = true
		_, err := m.MarkUserErased(ctx, m.userID)
		return err
	}
	return nil
}

// TestGrantGuard_AnOrgCreatedAcrossTheMarkIsTakenBack: an account creates an
// organization while it is being erased — its request read the account
// active, and the erasure marks it between its last grant's write — the
// default team's owner membership — and the read after it. The grant is
// taken back and refused, and the creation compensates: no organization, no
// team and no membership is left with the pseudonym as its owner.
//
// The mutation this reddens: drop the read after the write
// (takeBackIfErased) — the organization is created, owned by an account
// under erasure.
func TestGrantGuard_AnOrgCreatedAcrossTheMarkIsTakenBack(t *testing.T) {
	ctx := context.Background()
	st := &markAfterGrantStore{MemoryStore: identity.NewMemoryStore()}
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	jay, err := svc.Register(ctx, "jay@example.org", "correcthorse", "Jay", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	st.userID = jay.User.ID

	if _, err := svc.CreateOrgFor(ctx, jay.User.ID, "Acme", "acme"); !errors.Is(err, ErrGrantRefused) {
		t.Fatalf("CreateOrgFor across the mark = %v, want ErrGrantRefused", err)
	}
	if !st.marked {
		t.Fatal("the mark never landed: the scenario did not run")
	}
	if o, err := st.GetOrgBySlug(ctx, "acme"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the org created across the mark is still there: %+v (%v)", o, err)
	}
	mems, err := st.ListMembershipsByUser(ctx, jay.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range mems {
		if tm, err := st.GetTeam(ctx, mb.TeamID); err == nil {
			if o, err := st.GetOrg(ctx, tm.OrgID); err == nil && !o.Personal {
				t.Fatalf("the erased account still holds %+v in team %+v", mb, tm)
			}
		}
	}
	oms, err := st.ListOrgMembershipsByUser(ctx, jay.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, om := range oms {
		if o, err := st.GetOrg(ctx, om.OrgID); err == nil && !o.Personal {
			t.Fatalf("the erased account still holds %+v in org %+v", om, o)
		}
	}
}
