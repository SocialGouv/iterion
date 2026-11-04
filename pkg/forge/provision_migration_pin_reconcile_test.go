package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the migration carry. The carry copies prev's pins by value into
// the new hook identity's row, and that write is a CAS on the candidate
// row, never on prev: a record deleted between the read of prev and the
// write — its sweep listing the tenant before the new row existed — leaves
// the dead pin on a row the sweep never revisits. The write re-checks the
// records its carried pins name once the row is stored, as the routes do.

// beforeCreateStore runs a hook right before the config store writes a NEW
// row — the instant between the carry's read of prev and the landing of
// its copy, where a record's delete fits.
type beforeCreateStore struct {
	webhooks.ConfigStore
	before func()
}

func (s *beforeCreateStore) Create(ctx context.Context, c webhooks.Config) error {
	if s.before != nil {
		s.before()
	}
	return s.ConfigStore.Create(ctx, c)
}

// gettingSecrets fails the re-read of one secret the way a store outage
// would, leaving every other read to the real store.
type gettingSecrets struct {
	secrets.GenericSecretStore
	failID string
}

func (g gettingSecrets) Get(ctx context.Context, id string) (secrets.GenericSecret, error) {
	if id == g.failID {
		return secrets.GenericSecret{}, errors.New("secret store down")
	}
	return g.GenericSecretStore.Get(ctx, id)
}

// migrationWithOperatorPins provisions a repo, pins an operator's BYOK key
// and second forge identity on its config (real team-scoped records of
// t1), and retires the hook identity so the next provision migrates to a
// new one, carrying those pins.
func migrationWithOperatorPins(t *testing.T) (*Orchestrator, *secrets.MemoryApiKeyStore, ProvisionResult, Connection) {
	t.Helper()
	ctx := context.Background()
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	keys := secrets.NewMemoryApiKeyStore()
	o.ApiKeys = keys
	first := provisionBots(t, o, "review-pr")
	// The managed secret is minted by that first provision: read the
	// connection after it, so its id is the live pin the carry keeps.
	conn, err := o.Connections.Get(ctx, "t1", "conn-1")
	if err != nil || conn.ManagedSecretID == "" {
		t.Fatalf("connection after the first provision: managed secret %q err=%v", conn.ManagedSecretID, err)
	}
	tctx := store.WithTenant(ctx, "t1")
	if err := keys.Create(tctx, secrets.ApiKey{
		ID: "key-op", TenantID: "t1", ScopeTeamID: "t1", Provider: secrets.ProviderAnthropic,
		Name: "operator-key", Fingerprint: "fp-op", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.Secrets.Create(tctx, secrets.GenericSecret{
		ID: "sec-op", TenantID: "t1", ScopeTeamID: "t1", Name: "other_token", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := o.Webhooks.Get(ctx, first.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretOverrides["forge_token"] != conn.ManagedSecretID {
		t.Fatalf("the provisioned config pins %v, want forge_token=%s", cfg.SecretOverrides, conn.ManagedSecretID)
	}
	cfg.KeyOverrides = map[string]string{"anthropic": "key-op"}
	cfg.SecretOverrides["other_token"] = "sec-op"
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ri, err := o.Integrations.Get(ctx, first.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.WebhookSecretSealed = nil
	if err := o.Integrations.Update(ctx, ri); err != nil {
		t.Fatal(err)
	}
	return o, keys, first, conn
}

func TestProvisionMigration_ReChecksTheCarriedPinsAndClearsTheDeadOnes(t *testing.T) {
	ctx := context.Background()
	o, keys, first, conn := migrationWithOperatorPins(t)
	tctx := store.WithTenant(ctx, "t1")
	// Both records die after the carry read prev and before the new row
	// lands: the sweeps behind their deletes list the tenant's rows now,
	// and the row about to be written is not among them.
	deleted := false
	o.Webhooks = &beforeCreateStore{ConfigStore: o.Webhooks, before: func() {
		deleted = true
		if err := keys.Delete(tctx, "key-op"); err != nil {
			t.Error(err)
		}
		if err := o.Secrets.Delete(tctx, "sec-op"); err != nil {
			t.Error(err)
		}
	}}
	second := provisionBots(t, o, "revi-converse")
	if !deleted || second.WebhookID == first.WebhookID {
		t.Fatalf("migration fixture ineffective: deleted=%v same identity=%v", deleted, second.WebhookID == first.WebhookID)
	}
	got, err := o.Webhooks.Get(ctx, second.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if _, stands := got.KeyOverrides["anthropic"]; stands {
		t.Fatalf("the new hook identity carries a pin naming the deleted key: %v — no sweep will ever revisit this row and the lane seals the team default in silence", got.KeyOverrides)
	}
	if _, stands := got.SecretOverrides["other_token"]; stands {
		t.Fatalf("the new hook identity carries a pin naming the deleted secret: %v", got.SecretOverrides)
	}
	if got.SecretOverrides["forge_token"] != conn.ManagedSecretID {
		t.Fatalf("the live managed pin moved: %v, want forge_token=%s", got.SecretOverrides, conn.ManagedSecretID)
	}
	// The row's version is the one the clears left: a fresh read-modify-write
	// lands, so later writers CAS on what the store holds.
	got.LabelAllowlist = []string{"approved-only"}
	if err := o.Webhooks.Update(ctx, got); err != nil {
		t.Fatalf("an edit of the reconciled row = %v, want nil", err)
	}
}

// A carried pin naming a record that still lives is carried untouched: the
// re-check judges, it does not strip.
func TestProvisionMigration_CarriesALivePinUntouched(t *testing.T) {
	ctx := context.Background()
	o, _, first, _ := migrationWithOperatorPins(t)
	second := provisionBots(t, o, "revi-converse")
	if second.WebhookID == first.WebhookID {
		t.Fatal("migration fixture ineffective")
	}
	got, err := o.Webhooks.Get(ctx, second.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyOverrides["anthropic"] != "key-op" || got.SecretOverrides["other_token"] != "sec-op" {
		t.Fatalf("live pins did not survive the migration: keys=%v secrets=%v", got.KeyOverrides, got.SecretOverrides)
	}
}

// A record store that cannot answer the re-check fails the provision
// explicitly — the retry re-provisions and re-checks — instead of landing a
// pin nobody vouched for behind a success.
func TestProvisionMigration_ARecordReCheckTheStoreCannotAnswerFailsTheProvision(t *testing.T) {
	ctx := context.Background()
	o, _, _, _ := migrationWithOperatorPins(t)
	o.Secrets = gettingSecrets{GenericSecretStore: o.Secrets, failID: "sec-op"}
	_, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	})
	if err == nil || !strings.Contains(err.Error(), "re-check stored secret sec-op") {
		t.Fatalf("provision = %v, want the re-check's explicit refusal", err)
	}
}
