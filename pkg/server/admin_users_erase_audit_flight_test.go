package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// gatedAudit is the memory audit store whose Insert of one action parks
// until release is closed: an audit line still in flight when an erasure
// runs.
type gatedAudit struct {
	*audit.MemoryStore
	action  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedAudit) Insert(ctx context.Context, e audit.Event) error {
	if e.Action == g.action {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.MemoryStore.Insert(ctx, e)
}

// TestAdminEraseUser_AnAuditLineInFlightIsScrubbed: an invitation line
// naming the person by address, queued before the erasure and still in
// flight when it first scrubs — on this replica or another, which shares
// only the store — lands within the insert bound, and the second scrub,
// past that bound, takes its address. Without it the line would survive,
// keyed by an invitation id and an address the account no longer carries,
// where no later scrub finds it.
func TestAdminEraseUser_AnAuditLineInFlightIsScrubbed(t *testing.T) {
	f := newEraseFixture(t)
	admin := f.superAdmin(t)
	userID := f.user.User.ID
	f.s.auditInsertTimeout = 2 * time.Second
	gated := &gatedAudit{MemoryStore: f.audit, action: "org_invitation.created", entered: make(chan struct{}), release: make(chan struct{})}
	f.s.auditStore = gated

	f.s.auditInsert(audit.Event{Scope: audit.ScopeTenant, TenantID: "o1", ActorID: admin.User.ID, ActorKind: "user", Action: "org_invitation.created", Target: "invitation", TargetID: "inv-late", Meta: map[string]any{"role": "member", "email": fixtureEmail}})
	select {
	case <-gated.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued invitation line never reached the store")
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond) // inside the insert bound: after the first scrub, before the second
		close(gated.release)
	}()

	if code, _, body := f.erase(t, admin.AccessToken, userID); code != http.StatusOK {
		t.Fatalf("erase = %d %s, want 200", code, body)
	}
	<-released
	rows := f.tenantEvents(t, "o1", "org_invitation.created", 1)
	var late *audit.Event
	for i := range rows {
		if rows[i].TargetID == "inv-late" {
			late = &rows[i]
		}
	}
	if late == nil {
		t.Fatalf("the in-flight invitation line never landed: %+v", rows)
	}
	if late.Meta["email"] != nil || late.Meta["role"] != "member" {
		t.Fatalf("the invitation line in flight during the erasure: %+v, want its address scrubbed and the rest kept", late.Meta)
	}
}

// failingErasureLine is the memory audit store that refuses the first
// `user.erased` line.
type failingErasureLine struct {
	*audit.MemoryStore
	mu     sync.Mutex
	failed bool
}

func (f *failingErasureLine) Insert(ctx context.Context, e audit.Event) error {
	if e.Action == "user.erased" {
		f.mu.Lock()
		first := !f.failed
		f.failed = true
		f.mu.Unlock()
		if first {
			return errors.New("audit store unavailable")
		}
	}
	return f.MemoryStore.Insert(ctx, e)
}

// TestAdminEraseUser_ALostErasureLineIsReported: the `user.erased` line names
// the account to an erasure's replay after a restoration, so an erasure whose
// line the store refused answers an error, and repeating it (idempotent)
// writes the line.
func TestAdminEraseUser_ALostErasureLineIsReported(t *testing.T) {
	f := newEraseFixture(t)
	admin := f.superAdmin(t)
	userID := f.user.User.ID
	f.s.auditStore = &failingErasureLine{MemoryStore: f.audit}

	code, _, body := f.erase(t, admin.AccessToken, userID)
	if code != http.StatusInternalServerError {
		t.Fatalf("erase with the line refused = %d %s, want 500: the line the replay reads is lost", code, body)
	}
	if logged := f.logs.String(); !strings.Contains(logged, "the user.erased audit line was not written") {
		t.Fatalf("the server's log does not name the lost user.erased line:\n%s", logged)
	}
	code, out, body := f.erase(t, admin.AccessToken, userID)
	if code != http.StatusOK || !out.AlreadyErased {
		t.Fatalf("the repeated erasure = %d %s, want 200 with already_erased", code, body)
	}
	lines, err := f.audit.ListPlatform(context.Background(), audit.Page{Action: "user.erased"})
	if err != nil || len(lines) != 1 || lines[0].TargetID != userID {
		t.Fatalf("user.erased lines after the repeat = %+v (%v), want the one the repeat wrote", lines, err)
	}
}

// TestAdminEraseUser_ALiveTokenCannotRecreateAPushSubscription: an access
// token outlives the erasure by up to its TTL; with it, the erased account
// asks for a push subscription — endpoint, keys and user agent under the
// pseudonym. The route refuses it and nothing is stored.
func TestAdminEraseUser_ALiveTokenCannotRecreateAPushSubscription(t *testing.T) {
	f := newEraseFixtureWithConfig(t, configshare.NewMemoryStore(), func(c *Config) {
		c.WebPushVAPIDPublicKey, c.WebPushVAPIDPrivateKey = "test-public", "test-private"
	})
	admin := f.superAdmin(t)
	userID := f.user.User.ID

	if code, _, body := f.erase(t, admin.AccessToken, userID); code != http.StatusOK {
		t.Fatalf("erase = %d %s, want 200", code, body)
	}
	payload := `{"endpoint":"https://push.example/after-erasure","keys":{"p256dh":"k","auth":"a"}}`
	code, _, body := f.call(t, http.MethodPost, "/api/v1/notifications/push/subscriptions", f.user.AccessToken, payload)
	if code != http.StatusForbidden {
		t.Fatalf("a push subscription with the erased account's live token = %d %s, want 403", code, body)
	}
	subs, err := f.push.ListForUsers(context.Background(), []string{userID})
	if err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions under the pseudonym after the refusal = %+v (%v), want none", subs, err)
	}
}

// goneClientAudit is the memory audit store whose writes answer the
// context's error, the way the Mongo store does once a request is canceled.
// Its first ScrubActor is where the client goes away: it cancels the
// client's request and returns once the server's side of it is done.
type goneClientAudit struct {
	*audit.MemoryStore
	t            *testing.T
	cancelClient context.CancelFunc
	once         sync.Once
}

func (g *goneClientAudit) Insert(ctx context.Context, e audit.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.MemoryStore.Insert(ctx, e)
}

func (g *goneClientAudit) ScrubActor(ctx context.Context, userID, email string, digests []string) (int64, error) {
	g.once.Do(func() {
		g.cancelClient()
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			g.t.Error("the server never saw the client go away")
		}
	})
	return g.MemoryStore.ScrubActor(ctx, userID, email, digests)
}

// TestAdminEraseUser_AClientGoneMidErasureStillLeavesTheLine: the admin's
// client goes away once the account carries the erased status. The line that
// names the account to an erasure's replay after a restoration is written
// all the same: the request's cancellation takes nothing of it.
//
// The mutation this reddens: auditErasure inserting on the request's
// context — the canceled insert loses the line.
func TestAdminEraseUser_AClientGoneMidErasureStillLeavesTheLine(t *testing.T) {
	f := newEraseFixture(t)
	admin := f.superAdmin(t)
	userID := f.user.User.ID
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	f.s.auditStore = &goneClientAudit{MemoryStore: f.audit, t: t, cancelClient: cancelClient}

	req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, f.srv.URL+"/api/admin/users/"+userID+"/erase", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.AccessToken)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("the erasure answered %d to a client that went away", resp.StatusCode)
	}

	lines := f.platformEvents(t, "user.erased", 1)
	if len(lines) != 1 || lines[0].TargetID != userID {
		t.Fatalf("user.erased lines after the client went away = %+v, want the one naming %s", lines, userID)
	}
}

// markHookIdentity is the service's identity store with a hook run once,
// before the erased status lands on userID: what a concurrent request writes
// between the erasure's read of the memberships and its mark.
type markHookIdentity struct {
	identity.Store
	userID string
	before func()
}

func (m *markHookIdentity) MarkUserErased(ctx context.Context, id string) (identity.User, error) {
	if id == m.userID && m.before != nil {
		before := m.before
		m.before = nil
		before()
	}
	return m.Store.MarkUserErased(ctx, id)
}

// TestAdminEraseUser_ALateLastOwnerRefusalStillLeavesTheLine: the account
// creates an organization between the erasure's read of its memberships and
// its mark; the sweep's recount refuses the erasure (409 naming the org) once
// the account is already marked. The account stays erased, so the line a
// replay reads is written, incomplete.
//
// The mutation this reddens: the handler skipping the line on every
// last-owner refusal, the late one included.
func TestAdminEraseUser_ALateLastOwnerRefusalStillLeavesTheLine(t *testing.T) {
	f := newEraseFixture(t)
	admin := f.superAdmin(t)
	userID := f.user.User.ID
	st := f.s.authSvc.Store()
	now := time.Now().UTC()
	f.s.authSvc.SetStoreForTest(&markHookIdentity{Store: st, userID: userID, before: func() {
		ctx := context.Background()
		if _, err := st.CreateOrg(ctx, identity.Org{ID: "late", Name: "Late", Slug: "late", CreatedBy: userID, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Errorf("the racing org: %v", err)
		}
		if err := st.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: userID, OrgID: "late", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
			t.Errorf("the racing ownership: %v", err)
		}
	}})

	if code, _, body := f.erase(t, admin.AccessToken, userID); code != http.StatusConflict || !strings.Contains(body, "late") {
		t.Fatalf("erase with an ownership granted during it = %d %s, want 409 naming late", code, body)
	}
	lines := f.platformEvents(t, "user.erased", 1)
	if len(lines) != 1 || lines[0].TargetID != userID || lines[0].Meta["incomplete"] != true {
		t.Fatalf("user.erased lines after the late refusal = %+v, want one naming %s, incomplete", lines, userID)
	}
}
