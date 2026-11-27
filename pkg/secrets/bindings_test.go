package secrets

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestIntersectHosts(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"both empty -> unrestricted", nil, nil, nil},
		{"workflow empty, binding restricts", nil, []string{"gitlab.com"}, []string{"gitlab.com"}},
		{"binding empty, workflow restricts", []string{"gitlab.com"}, nil, []string{"gitlab.com"}},
		{"overlap narrows", []string{"gitlab.com", "evil.com"}, []string{"gitlab.com"}, []string{"gitlab.com"}},
		{"disjoint -> nothing", []string{"a.com"}, []string{"b.com"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IntersectHosts(c.a, c.b)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("IntersectHosts(%v,%v)=%v want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestMemoryBotSecretBindingStore(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryBotSecretBindingStore()
	b := BotSecretBinding{ID: "b1", TenantID: "t1", BotID: "review-pr", SecretID: "s1", SecretNameForWorkflow: "gitlab_token", CreatedAt: time.Now()}
	if err := st.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "b1")
	if err != nil || got.SecretNameForWorkflow != "gitlab_token" {
		t.Fatalf("get: %+v %v", got, err)
	}
	_ = st.Create(ctx, BotSecretBinding{ID: "b2", TenantID: "t1", BotID: "billy", SecretID: "s2", SecretNameForWorkflow: "npm_token", CreatedAt: time.Now()})
	if byBot, _ := st.ListByTenantBot(ctx, "t1", "review-pr"); len(byBot) != 1 || byBot[0].ID != "b1" {
		t.Fatalf("ListByTenantBot: %+v", byBot)
	}
	if all, _ := st.ListByTenant(ctx, "t1"); len(all) != 2 {
		t.Fatalf("ListByTenant: %d", len(all))
	}
	if other, _ := st.ListByTenant(ctx, "nope"); len(other) != 0 {
		t.Fatal("tenant isolation")
	}
}

// A secret's OWN egress lock (GenericSecret.AllowedHosts — the managed
// forge-token case) must be enforced through every resolution tier, and a
// binding may only narrow it further, never broaden it.
func TestResolveGenericWithBindings_SecretOwnEgressLock(t *testing.T) {
	ctx := context.Background()
	sealer := newSealer(t)
	secStore := NewMemoryGenericSecretStore()
	bindStore := NewMemoryBotSecretBindingStore()

	// A managed forge token pinned to github.com, bound via a Tier-0 override.
	id := NewGenericSecretID()
	sealed, err := SealGenericSecret(sealer, id, []byte("ghs_tok"))
	if err != nil {
		t.Fatal(err)
	}
	if err := secStore.Create(ctx, GenericSecret{
		ID: id, ScopeTeamID: "team", Name: "forge_token",
		SealedSecret: sealed, AllowedHosts: []string{"github.com"},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// (a) Tier-0 override carries no binding hosts, but the secret's own lock
	// must still apply so the token can't egress off github.com.
	got, err := ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "billy",
		[]string{"forge_token"}, map[string]string{"forge_token": id}, sealer, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := got["forge_token"]
	if r.SourceScope != "webhook-override" {
		t.Fatalf("want override tier, got %q", r.SourceScope)
	}
	if len(r.AllowedHosts) != 1 || r.AllowedHosts[0] != "github.com" {
		t.Fatalf("secret self-lock not enforced through override: %v", r.AllowedHosts)
	}

	// (b) A binding narrows further (disjoint host → deny-all, empty result).
	if err := bindStore.Create(ctx, BotSecretBinding{
		ID: "b1", TenantID: "team", BotID: "billy", SecretID: id,
		SecretNameForWorkflow: "forge_token", AllowedHosts: []string{"evil.com"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "billy",
		[]string{"forge_token"}, nil, sealer, nil)
	if hosts := got["forge_token"].AllowedHosts; len(hosts) != 0 {
		t.Fatalf("binding must narrow (github.com ∩ evil.com = ∅), got %v", hosts)
	}
}

func TestResolveGenericWithBindings_TierOrdering(t *testing.T) {
	ctx := context.Background()
	sealer := newSealer(t)
	secStore := NewMemoryGenericSecretStore()
	bindStore := NewMemoryBotSecretBindingStore()

	mkGenericSecret(t, secStore, sealer, "team", "", "gitlab_token", "team-val")
	orgCred := mkGenericSecret(t, secStore, sealer, "team", "", "org_gitlab_cred", "binding-val")
	if err := bindStore.Create(ctx, BotSecretBinding{
		ID: "b1", TenantID: "team", BotID: "review-pr",
		SecretID: orgCred.ID, SecretNameForWorkflow: "gitlab_token",
		AllowedHosts: []string{"gitlab.com"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// (a) unattended actor (no user secrets) → binding beats team-scoped.
	got, err := ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "review-pr", []string{"gitlab_token"}, nil, sealer, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := got["gitlab_token"]
	if string(r.Plaintext) != "binding-val" || r.SourceScope != "binding" {
		t.Fatalf("binding tier: %+v", r)
	}
	if len(r.AllowedHosts) != 1 || r.AllowedHosts[0] != "gitlab.com" {
		t.Fatalf("binding AllowedHosts not carried: %v", r.AllowedHosts)
	}

	// (b) no bot id → binding skipped, team-scoped wins.
	got, _ = ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "", []string{"gitlab_token"}, nil, sealer, nil)
	if string(got["gitlab_token"].Plaintext) != "team-val" || got["gitlab_token"].SourceScope != "team" {
		t.Fatalf("no-bot tier: %+v", got["gitlab_token"])
	}

	// (c) a personal user secret of the same name beats the binding.
	mkGenericSecret(t, secStore, sealer, "team", "alice", "gitlab_token", "user-val")
	got, _ = ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "alice", "review-pr", []string{"gitlab_token"}, nil, sealer, nil)
	if string(got["gitlab_token"].Plaintext) != "user-val" || got["gitlab_token"].SourceScope != "user" {
		t.Fatalf("user tier: %+v", got["gitlab_token"])
	}
}

func TestResolveGenericWithBindings_WebhookOverride(t *testing.T) {
	ctx := context.Background()
	sealer := newSealer(t)
	secStore := NewMemoryGenericSecretStore()
	bindStore := NewMemoryBotSecretBindingStore()

	// Org binding maps gitlab_token -> the default org credential.
	def := mkGenericSecret(t, secStore, sealer, "team", "", "org_default_token", "binding-val")
	if err := bindStore.Create(ctx, BotSecretBinding{
		ID: "b1", TenantID: "team", BotID: "review-pr",
		SecretID: def.ID, SecretNameForWorkflow: "gitlab_token", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A second org secret a webhook can pin instead.
	alt := mkGenericSecret(t, secStore, sealer, "team", "", "alt_token", "override-val")

	// (a) with an override, Tier 0 (webhook-override) wins over the binding.
	got, err := ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "review-pr",
		[]string{"gitlab_token"}, map[string]string{"gitlab_token": alt.ID}, sealer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["gitlab_token"]; string(r.Plaintext) != "override-val" || r.SourceScope != "webhook-override" {
		t.Fatalf("override should win over binding: %+v", r)
	}

	// (b) a dangling override id is ignored → falls back to the binding.
	got, _ = ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "review-pr",
		[]string{"gitlab_token"}, map[string]string{"gitlab_token": "nonexistent"}, sealer, nil)
	if string(got["gitlab_token"].Plaintext) != "binding-val" {
		t.Fatalf("dangling override should fall back to binding: %+v", got["gitlab_token"])
	}
}

func TestResolveGenericWithBindings_DanglingBindingSkipped(t *testing.T) {
	ctx := context.Background()
	sealer := newSealer(t)
	secStore := NewMemoryGenericSecretStore()
	bindStore := NewMemoryBotSecretBindingStore()
	mkGenericSecret(t, secStore, sealer, "team", "", "gitlab_token", "team-val")
	// binding points at a secret that doesn't exist → must fall through, not error.
	_ = bindStore.Create(ctx, BotSecretBinding{ID: "b1", TenantID: "team", BotID: "review-pr",
		SecretID: "ghost", SecretNameForWorkflow: "gitlab_token", CreatedAt: time.Now()})
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	got, err := ResolveGenericWithBindings(ctx, secStore, bindStore, "team", "", "review-pr", []string{"gitlab_token"}, nil, sealer, logger)
	if err != nil {
		t.Fatalf("dangling binding should not error: %v", err)
	}
	if string(got["gitlab_token"].Plaintext) != "team-val" || got["gitlab_token"].SourceScope != "team" {
		t.Fatalf("dangling binding should fall through to team: %+v", got["gitlab_token"])
	}
	// The dangling binding is non-fatal but must be observable: warn naming
	// the workflow-secret name so an operator can grep the drop.
	if out := buf.String(); !strings.Contains(out, "gitlab_token") || !strings.Contains(out, "dangles") {
		t.Fatalf("dangling binding must warn (naming the secret), got: %q", out)
	}
}

// getFailing is a secret store whose Get of the ids it names fails with err:
// a store that could not answer, not an absent secret.
type getFailing struct {
	GenericSecretStore
	ids map[string]bool
	err error
}

func (s *getFailing) Get(ctx context.Context, id string) (GenericSecret, error) {
	if s.ids[id] {
		return GenericSecret{}, s.err
	}
	return s.GenericSecretStore.Get(ctx, id)
}

// The resolver tells a candidate it could not read from one that is absent:
// a binding or an override whose Get fails other than with
// ErrGenericSecretNotFound, and a sealed value that does not unseal, are
// reported per name; a dangling binding (the secret deleted) is not; a lower
// tier that resolves still wins. Red when a read error is reported as
// nothing, or a deleted secret as unreadable.
func TestResolveGenericWithBindingsReport_UnreadableIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	sealer := newSealer(t)
	mem := NewMemoryGenericSecretStore()
	bindStore := NewMemoryBotSecretBindingStore()
	bound := mkGenericSecret(t, mem, sealer, "team", "", "org_forge", "bound-val")
	pinned := mkGenericSecret(t, mem, sealer, "team", "", "org_pin", "pinned-val")
	stranger := mkGenericSecret(t, mem, newSealer(t), "team", "", "org_other_ring", "other-ring-val")
	mkGenericSecret(t, mem, sealer, "team", "", "fallback", "team-val")
	for _, b := range []BotSecretBinding{
		{ID: "b1", TenantID: "team", BotID: "bot", SecretID: bound.ID, SecretNameForWorkflow: "forge_token"},
		{ID: "b2", TenantID: "team", BotID: "bot", SecretID: "ghost", SecretNameForWorkflow: "deleted_token"},
		{ID: "b3", TenantID: "team", BotID: "bot", SecretID: stranger.ID, SecretNameForWorkflow: "sealed_token"},
		{ID: "b4", TenantID: "team", BotID: "bot", SecretID: bound.ID, SecretNameForWorkflow: "fallback"},
	} {
		b.CreatedAt = time.Now()
		if err := bindStore.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	blip := errors.New("server selection error: context deadline exceeded")
	store := &getFailing{GenericSecretStore: mem, ids: map[string]bool{bound.ID: true, pinned.ID: true}, err: blip}
	names := []string{"forge_token", "deleted_token", "sealed_token", "pinned_token", "fallback"}
	got, unreadable, err := ResolveGenericWithBindingsReport(ctx, store, bindStore, "team", "", "bot", names,
		map[string]string{"pinned_token": pinned.ID}, sealer, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"forge_token", "pinned_token"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s resolved %+v, want nothing", name, got[name])
		}
		if !errors.Is(unreadable[name], blip) {
			t.Fatalf("%s: unreadable %v, want the store's error", name, unreadable[name])
		}
	}
	if _, ok := got["sealed_token"]; ok || unreadable["sealed_token"] == nil {
		t.Fatalf("sealed_token: resolved=%v unreadable=%v, want reported as not unsealing", ok, unreadable["sealed_token"])
	}
	if e, ok := unreadable["deleted_token"]; ok {
		t.Fatalf("deleted_token reported unreadable (%v), want absent: its secret is gone", e)
	}
	if string(got["fallback"].Plaintext) != "team-val" || got["fallback"].SourceScope != "team" {
		t.Fatalf("fallback: %+v, want the team tier past the unreadable binding", got["fallback"])
	}
}
