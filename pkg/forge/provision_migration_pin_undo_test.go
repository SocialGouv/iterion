package forge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the migration carry's re-check beside the provision's undo. A
// retried migration updates the candidate row an earlier attempt stored, so
// a step failing after the re-check restores that row's previous settings —
// through restoreConfig, which writes only while the row stands at the
// version this attempt wrote. The version the re-check reports is that
// judgment's input: one this attempt reached and did not report makes the
// undo leave its own row behind, one another writer reached and the attempt
// claims makes the undo write a stale snapshot over that writer.

var (
	errPinWriteDown = errors.New("config store down")
	errAdminDown    = errors.New("gitlab: installation token: HTTP 503")
)

// namesRecord reports whether any credential pin of the row names id.
func namesRecord(c webhooks.Config, id string) bool {
	for _, pins := range []map[string]string{c.KeyOverrides, c.SecretOverrides} {
		for _, v := range pins {
			if v == id {
				return true
			}
		}
	}
	return false
}

// pinRaceStore drives the writes on one config row around the carry's
// re-check, whatever write shape clears a pin. kill runs once, right before
// the provision's own write of the row: the records the carried pins name
// die there, and no sweep visits the row. A write that would drop a pin
// naming refuse fails like a store outage, leaving the row as it was. race
// runs once, right after the first write that drops a dead pin lands.
type pinRaceStore struct {
	webhooks.ConfigStore
	rowID   string
	dead    []string
	kill    func()
	refuse  string
	race    func()
	killed  bool
	dropped bool
}

func (s *pinRaceStore) dropsDead(before, after webhooks.Config) bool {
	return slices.ContainsFunc(s.dead, func(id string) bool {
		return namesRecord(before, id) && !namesRecord(after, id)
	})
}

func (s *pinRaceStore) landedDrop() {
	if s.dropped {
		return
	}
	s.dropped = true
	if s.race != nil {
		s.race()
	}
}

func (s *pinRaceStore) Update(ctx context.Context, c webhooks.Config) error {
	if c.ID != s.rowID {
		return s.ConfigStore.Update(ctx, c)
	}
	if s.kill != nil && !s.killed {
		s.killed = true
		s.kill()
	}
	before, err := s.Get(ctx, c.ID)
	if err != nil {
		return err
	}
	if s.refuse != "" && namesRecord(before, s.refuse) && !namesRecord(c, s.refuse) {
		return errPinWriteDown
	}
	if err := s.ConfigStore.Update(ctx, c); err != nil {
		return err
	}
	if s.dropsDead(before, c) {
		s.landedDrop()
	}
	return nil
}

func (s *pinRaceStore) ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error) {
	if id == s.rowID && recordID == s.refuse {
		return false, errPinWriteDown
	}
	cleared, err := s.ConfigStore.ClearCredentialPins(ctx, id, recordID)
	if err == nil && cleared && id == s.rowID {
		s.landedDrop()
	}
	return cleared, err
}

// raceOnKeyRead runs race once, at the re-check's first read of a pinned
// key: after the provision's write of the row, before any clear of it.
type raceOnKeyRead struct {
	secrets.ApiKeyStore
	race  func()
	raced bool
}

func (r *raceOnKeyRead) Get(ctx context.Context, id string) (secrets.ApiKey, error) {
	if !r.raced {
		r.raced = true
		r.race()
	}
	return r.ApiKeyStore.Get(ctx, id)
}

// retriedMigration leaves a migration half-done, as a failed attempt does:
// the new hook identity's row stored with the carried pins, the integration
// still on the old identity. The next provision of the repo updates that
// candidate row — a write with a previous version the undo may restore — and
// re-checks the pins it carries. raceStore wraps the config store around the
// candidate row, its kill deleting both carried records.
func retriedMigration(t *testing.T) (*Orchestrator, webhooks.Config, *pinRaceStore) {
	t.Helper()
	ctx := context.Background()
	o, keys, _, _ := migrationWithOperatorPins(t)
	adminFor := o.AdminFor
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return nil, errAdminDown }
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	}); !errors.Is(err, errAdminDown) {
		t.Fatalf("the first migration attempt = %v, want its admin-client failure", err)
	}
	o.AdminFor = adminFor
	ri, err := o.Integrations.GetProvisionByConnRepo(ctx, "t1", "conn-1", "group/api")
	if err != nil || ri.CandidateWebhookID == "" || ri.CandidateWebhookID == ri.WebhookID {
		t.Fatalf("no half-done migration: candidate %q, active %q, err=%v", ri.CandidateWebhookID, ri.WebhookID, err)
	}
	candidate, err := o.Webhooks.Get(ctx, ri.CandidateWebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !namesRecord(candidate, "key-op") || !namesRecord(candidate, "sec-op") {
		t.Fatalf("the candidate row carries keys=%v secrets=%v, want both operator pins", candidate.KeyOverrides, candidate.SecretOverrides)
	}
	tctx := store.WithTenant(ctx, "t1")
	rs := &pinRaceStore{ConfigStore: o.Webhooks, rowID: candidate.ID, dead: []string{"key-op", "sec-op"}, kill: func() {
		if err := keys.Delete(tctx, "key-op"); err != nil {
			t.Error(err)
		}
		if err := o.Secrets.Delete(tctx, "sec-op"); err != nil {
			t.Error(err)
		}
	}}
	o.Webhooks = rs
	return o, candidate, rs
}

// retryMigration re-runs the migration with one more bot, so a restored
// candidate row is told from this attempt's write by its bot set.
func retryMigration(o *Orchestrator) error {
	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	})
	return err
}

// assertRestored reads the candidate row back and requires the settings it
// held before the retry.
func assertRestored(t *testing.T, o *Orchestrator, candidate webhooks.Config, what string) {
	t.Helper()
	got, err := o.Webhooks.Get(context.Background(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(got.BotIDs, candidate.BotIDs) {
		t.Fatalf("%s: the candidate row keeps the failed attempt's bots %v, want its previous %v restored — the undo judged a row this attempt still owns as moved by another writer", what, got.BotIDs, candidate.BotIDs)
	}
}

// The control: the re-check clears the dead pins, a later step fails, and
// the undo restores — the version the re-check reports is the one its own
// clear reached.
func TestProvisionMigration_UndoRestoresTheRowItsPinClearLeft(t *testing.T) {
	o, candidate, rs := retriedMigration(t)
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return nil, errAdminDown }
	if err := retryMigration(o); !errors.Is(err, errAdminDown) {
		t.Fatalf("retry = %v, want the admin-client failure", err)
	}
	if !rs.killed || !rs.dropped {
		t.Fatalf("fixture ineffective: records killed=%v, a dead pin cleared=%v", rs.killed, rs.dropped)
	}
	assertRestored(t, o, candidate, "after the admin-client failure")
}

// The store refuses the clear of the second dead record — after the first
// record's clear, when the clears are one write per record. Whatever of the
// clearing landed, the version reported is the one the row reached under
// this attempt's own writes, so the undo still restores the row it owns.
func TestProvisionMigration_AFailedPinClearLeavesTheUndoOwningTheRow(t *testing.T) {
	o, candidate, rs := retriedMigration(t)
	rs.refuse = "sec-op"
	if err := retryMigration(o); !errors.Is(err, errPinWriteDown) {
		t.Fatalf("retry = %v, want the refused pin clear", err)
	}
	if !rs.killed {
		t.Fatal("fixture ineffective: the carried records never died")
	}
	assertRestored(t, o, candidate, "after the refused pin clear")
}

// Another writer moves the row while the re-check runs — before the clear,
// or between the clear's writes. A bump the attempt did not make is not its
// own to claim: the undo leaves the row to that writer, and the dead pins
// are cleared all the same.
func TestProvisionMigration_AConcurrentBumpIsNotTheUndosToClaim(t *testing.T) {
	for _, window := range []string{"during the re-check", "after the first clearing write"} {
		t.Run(window, func(t *testing.T) {
			ctx := context.Background()
			o, candidate, rs := retriedMigration(t)
			raced := false
			edit := func() {
				raced = true
				row, err := rs.Get(ctx, candidate.ID)
				if err != nil {
					t.Error(err)
					return
				}
				row.LabelAllowlist = []string{"concurrent-edit"}
				if err := rs.ConfigStore.Update(ctx, row); err != nil {
					t.Errorf("the concurrent edit = %v, want it to land", err)
				}
			}
			if window == "during the re-check" {
				o.ApiKeys = &raceOnKeyRead{ApiKeyStore: o.ApiKeys, race: edit}
			} else {
				rs.race = edit
			}
			o.AdminFor = func(context.Context, Connection) (Admin, error) { return nil, errAdminDown }
			if err := retryMigration(o); !errors.Is(err, errAdminDown) {
				t.Fatalf("retry = %v, want the admin-client failure", err)
			}
			if !rs.killed || !raced {
				t.Fatalf("fixture ineffective: records killed=%v, concurrent edit ran=%v", rs.killed, raced)
			}
			got, err := o.Webhooks.Get(ctx, candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.LabelAllowlist, []string{"concurrent-edit"}) {
				t.Fatalf("label allowlist = %v, want the concurrent writer's [concurrent-edit] — the undo claimed another writer's bump and restored a stale snapshot over its edit", got.LabelAllowlist)
			}
			if namesRecord(got, "key-op") || namesRecord(got, "sec-op") {
				t.Fatalf("the row still pins a deleted record: keys=%v secrets=%v", got.KeyOverrides, got.SecretOverrides)
			}
		})
	}
}
