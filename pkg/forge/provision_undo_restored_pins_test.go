package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the undo's restore beside a credential record's delete. The restore
// writes back a snapshot of the row read before the provision's own write,
// at the version that write reached — so no sweep that visited the row in
// between conflicts with it. A sweep removes only what the row names when it
// visits: one that passed while the provision's write did not name the
// record (it never carried the pin, or its re-check already cleared it)
// bumped nothing, and the restore would land the dead pin on a row nothing
// revisits.

// assertNoDeadPin reads the row back and requires that no credential pin of
// it names a deleted record.
func assertNoDeadPin(t *testing.T, o *Orchestrator, rowID string, dead []string, what string) {
	t.Helper()
	got, err := o.Webhooks.Get(context.Background(), rowID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range dead {
		if namesRecord(got, id) {
			t.Fatalf("%s: the row pins deleted record %s again (keys=%v secrets=%v) — the undo wrote back a pin the delete's sweep had already passed", what, id, got.KeyOverrides, got.SecretOverrides)
		}
	}
}

// sweepRow is the delete's sweep reaching one row: the value-scoped clear of
// every pin naming each record, on the row as the store holds it. It
// reports whether the row named any of them.
func sweepRow(t *testing.T, s webhooks.ConfigStore, rowID string, records ...string) bool {
	t.Helper()
	found := false
	for _, id := range records {
		cleared, err := s.ClearCredentialPins(context.Background(), rowID, id)
		if err != nil {
			t.Errorf("the sweep's clear of %s on %s = %v", id, rowID, err)
		}
		found = found || cleared
	}
	return found
}

// The finding's interleaving on a retried migration: the carried records die
// right before the attempt's write, the re-check clears their pins, and
// their delete's sweep reaches the candidate row after that clear — it finds
// nothing, so it leaves the version where the attempt's clear put it. The
// admin client then fails, and the undo restores the candidate row's
// previous settings: those settings come back, the dead pins do not.
func TestProvisionMigration_TheUndoRestoresNoPinTheSweepPassed(t *testing.T) {
	o, candidate, rs := retriedMigration(t)
	swept, sweepFound := false, false
	o.AdminFor = func(context.Context, Connection) (Admin, error) {
		swept = true
		sweepFound = sweepRow(t, rs.ConfigStore, candidate.ID, rs.dead...)
		return nil, errAdminDown
	}
	if err := retryMigration(o); !errors.Is(err, errAdminDown) {
		t.Fatalf("retry = %v, want the admin-client failure", err)
	}
	if !rs.killed || !rs.dropped || !swept || sweepFound {
		t.Fatalf("fixture ineffective: records killed=%v, re-check cleared a dead pin=%v, sweep ran=%v, sweep found a pin=%v", rs.killed, rs.dropped, swept, sweepFound)
	}
	assertRestored(t, o, candidate, "after the sweep passed and the admin client failed")
	assertNoDeadPin(t, o, candidate.ID, rs.dead, "after the sweep passed and the admin client failed")
}

// beforeRepinStore runs before once, right before a write of rowID that
// would land a pin naming recordID on a row that does not name it — the
// undo's restore of a snapshot the provision's write had moved off the
// record.
type beforeRepinStore struct {
	webhooks.ConfigStore
	rowID    string
	recordID string
	before   func()
	ran      bool
}

func (s *beforeRepinStore) Update(ctx context.Context, c webhooks.Config) error {
	if c.ID == s.rowID && !s.ran && namesRecord(c, s.recordID) {
		cur, err := s.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		if !namesRecord(cur, s.recordID) {
			s.ran = true
			s.before()
		}
	}
	return s.ConfigStore.Update(ctx, c)
}

// The same restore on a same-identity re-provision, which carries no pin
// re-check of its own: the operator pinned a stored secret under
// forge_token, the name the provision stamps with the connection's managed
// token, so the attempt's write drops that pin and the undo's snapshot holds
// it. The secret dies, its sweep passing the row while it does not name
// it — before the undo begins, or as the restore's write arrives, after any
// judgment the restore could make before writing. Either way the restored
// row must not pin the dead secret.
func TestReprovision_TheUndoRestoresNoPinTheSweepPassed(t *testing.T) {
	for _, window := range []string{"before the undo", "as the restore lands"} {
		t.Run(window, func(t *testing.T) {
			ctx := context.Background()
			o, _, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			first := provisionBots(t, o, "review-pr")
			tctx := store.WithTenant(ctx, "t1")
			if err := o.Secrets.Create(tctx, secrets.GenericSecret{
				ID: "sec-op", TenantID: "t1", ScopeTeamID: "t1", Name: "operator_forge_token", CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			prev, err := o.Webhooks.Get(ctx, first.WebhookID)
			if err != nil {
				t.Fatal(err)
			}
			prev.SecretOverrides["forge_token"] = "sec-op"
			if err := o.Webhooks.Update(ctx, prev); err != nil {
				t.Fatal(err)
			}
			prev.Version++

			inner := o.Webhooks
			swept, sweepFound := false, false
			kill := func() {
				swept = true
				if err := o.Secrets.Delete(tctx, "sec-op"); err != nil {
					t.Error(err)
				}
				sweepFound = sweepRow(t, inner, first.WebhookID, "sec-op")
			}
			if window == "before the undo" {
				o.AdminFor = func(context.Context, Connection) (Admin, error) {
					kill()
					return nil, errAdminDown
				}
			} else {
				o.Webhooks = &beforeRepinStore{ConfigStore: inner, rowID: first.WebhookID, recordID: "sec-op", before: kill}
				o.AdminFor = func(context.Context, Connection) (Admin, error) { return nil, errAdminDown }
			}
			_, err = o.Provision(ctx, ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"revi-converse"}, ActorID: "u1",
			})
			if !errors.Is(err, errAdminDown) {
				t.Fatalf("re-provision = %v, want the admin-client failure", err)
			}
			if !swept || sweepFound {
				t.Fatalf("fixture ineffective: sweep ran=%v, sweep found the pin=%v", swept, sweepFound)
			}
			assertRestored(t, o, prev, window)
			assertNoDeadPin(t, o, first.WebhookID, []string{"sec-op"}, window)
		})
	}
}

// refusingDropStore, once armed, fails like a store outage every write of
// rowID that would drop a pin naming recordID.
type refusingDropStore struct {
	webhooks.ConfigStore
	rowID    string
	recordID string
	armed    bool
}

func (s *refusingDropStore) Update(ctx context.Context, c webhooks.Config) error {
	if s.armed && c.ID == s.rowID && !namesRecord(c, s.recordID) {
		cur, err := s.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		if namesRecord(cur, s.recordID) {
			return errPinWriteDown
		}
	}
	return s.ConfigStore.Update(ctx, c)
}

func (s *refusingDropStore) ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error) {
	if s.armed && id == s.rowID && recordID == s.recordID {
		return false, errPinWriteDown
	}
	return s.ConfigStore.ClearCredentialPins(ctx, id, recordID)
}

// The restored row's re-check finds the dead secret, and the store refuses
// its clear. The settings stay restored, and the provision's error carries
// that failure beside its own — not as a restore that did not happen, which
// the route would report first and word as settings lost.
func TestReprovision_ARestoredPinTheStoreWillNotClearIsReported(t *testing.T) {
	ctx := context.Background()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	first := provisionBots(t, o, "review-pr")
	tctx := store.WithTenant(ctx, "t1")
	if err := o.Secrets.Create(tctx, secrets.GenericSecret{
		ID: "sec-op", TenantID: "t1", ScopeTeamID: "t1", Name: "operator_forge_token", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	prev, err := o.Webhooks.Get(ctx, first.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	prev.SecretOverrides["forge_token"] = "sec-op"
	if err := o.Webhooks.Update(ctx, prev); err != nil {
		t.Fatal(err)
	}
	prev.Version++

	rs := &refusingDropStore{ConfigStore: o.Webhooks, rowID: first.WebhookID, recordID: "sec-op"}
	o.Webhooks = rs
	sweepFound := false
	o.AdminFor = func(context.Context, Connection) (Admin, error) {
		if err := o.Secrets.Delete(tctx, "sec-op"); err != nil {
			t.Error(err)
		}
		sweepFound = sweepRow(t, rs.ConfigStore, first.WebhookID, "sec-op")
		rs.armed = true
		return nil, errAdminDown
	}
	_, err = o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	if !rs.armed || sweepFound {
		t.Fatalf("fixture ineffective: the secret died=%v, sweep found the pin=%v", rs.armed, sweepFound)
	}
	if !errors.Is(err, errAdminDown) {
		t.Fatalf("re-provision = %v, want the admin-client failure", err)
	}
	if !errors.Is(err, errPinWriteDown) {
		t.Fatalf("re-provision = %v, want the refused clear of the restored row's dead pin reported beside it", err)
	}
	var restoreFailed *ConfigRestoreError
	if errors.As(err, &restoreFailed) {
		t.Fatalf("re-provision = %v: a restore that landed is reported as a ConfigRestoreError, which tells the operator the previous settings were lost", err)
	}
	assertRestored(t, o, prev, "after the refused clear of the restored row")
}
