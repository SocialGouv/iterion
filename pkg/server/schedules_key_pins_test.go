package server

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The scheduled lane names its repository and carries key pins (#3241). A
// schedule launched with neither: a key restricted to the very repository it
// clones never funded it, the per-repository quota had nothing to bind, the
// spend meter charged no repository, and with two gateway keys of one team
// serving the same bot the walk picked the oldest for every schedule — the
// split an operator asks for ("these schedules spend key B") had no place to
// be written.

const (
	pinWorldIntegration = "ri-veille"
	pinWorldWebhook     = "wh-veille"
	// The integration's repository as the forge writes it; the schedule's
	// clone URL spells it in lower case.
	pinWorldRepo    = "Group/Veille"
	pinWorldRepoURL = "https://gitlab.com/group/veille.git"
)

// scheduledPinWorld is a cloud-shaped server for the tick: team t1 with the
// probe bot and a recording run service (newGatedSpineServer), the forge
// layer, an api key store, and one integration of t1 on gitlab.com whose
// webhook config holds the integration's key pins.
type scheduledPinWorld struct {
	s    *Server
	pub  *spinePublisher
	keys secrets.ApiKeyStore
}

func newScheduledPinWorld(t *testing.T, integrationPins map[string]string) *scheduledPinWorld {
	t.Helper()
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
	keys := secrets.NewMemoryApiKeyStore()
	s.apiKeys = keys
	wireForgeTestStores(t, s)
	ctx := context.Background()
	sealed, err := connection.SealForgePAT(s.sealer, "c1", "glpat-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(ctx, forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, SealedPayload: sealed}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(ctx, forge.RepoIntegration{
		ID: pinWorldIntegration, TenantID: "t1", ConnectionID: "c1",
		Provider: forge.ProviderGitLab, RepoFullName: pinWorldRepo, WebhookID: pinWorldWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	seedSweepWebhook(t, s.webhookConfigs, webhooks.Config{ID: pinWorldWebhook, TenantID: "t1", KeyOverrides: integrationPins})
	for _, k := range []struct {
		id   string
		prov secrets.Provider
	}{
		{"key-tech", secrets.ProviderOpenAICompatible},
		{"key-design", secrets.ProviderOpenAICompatible},
		{"key-anthropic", secrets.ProviderAnthropic},
	} {
		seedSweepKey(t, keys, "t1", k.id, k.prov)
	}
	return &scheduledPinWorld{s: s, pub: pub, keys: keys}
}

// boundSchedule is a schedule of the probe bot bound to the world's
// integration, cloning its repository.
func boundSchedule(own map[string]string) cloudsched.ScheduledBot {
	sb := scheduledBot("t1")
	sb.RepoIntegrationID = pinWorldIntegration
	sb.RepoURL = pinWorldRepoURL
	sb.KeyOverrides = own
	return sb
}

// setIntegrationPins rewrites the integration's pins on its webhook config,
// as the webhook PATCH or the delete sweep would.
func (w *scheduledPinWorld) setIntegrationPins(t *testing.T, pins map[string]string) {
	t.Helper()
	cfg, err := w.s.webhookConfigs.Get(context.Background(), pinWorldWebhook)
	if err != nil {
		t.Fatal(err)
	}
	cfg.KeyOverrides = pins
	if err := w.s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

// tick fires sb through the production launch and returns the spec the run
// service was handed.
func (w *scheduledPinWorld) tick(t *testing.T, sb cloudsched.ScheduledBot) tickSpec {
	t.Helper()
	before := w.pub.count()
	if err := w.s.launchScheduledBot(spineCtx(t), sb); err != nil {
		t.Fatalf("launchScheduledBot = %v, want a launch", err)
	}
	if w.pub.count() != before+1 {
		t.Fatalf("the run service was asked %d time(s), want one more launch", w.pub.count()-before)
	}
	spec := w.pub.lastSpec()
	return tickSpec{ProjectPath: spec.ProjectPath, KeyOverrides: spec.KeyOverrides}
}

// refused fires sb and requires the tick to be refused before the run
// service is asked, with an error naming every fragment in want.
func (w *scheduledPinWorld) refused(t *testing.T, sb cloudsched.ScheduledBot, want ...string) {
	t.Helper()
	before := w.pub.count()
	err := w.s.launchScheduledBot(spineCtx(t), sb)
	if err == nil {
		t.Fatalf("launchScheduledBot = nil, want a refusal naming %q", want)
	}
	for _, frag := range want {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("refusal %q does not name %q", err, frag)
		}
	}
	if got := w.pub.count() - before; got != 0 {
		t.Errorf("the run service was asked %d time(s) by a refused tick, want 0", got)
	}
}

// tickSpec is the slice of the published LaunchSpec these tests read.
type tickSpec struct {
	ProjectPath  string
	KeyOverrides map[string]string
}

// TestScheduledLaunchNamesItsRepository: the published launch names the
// repository the schedule clones, so a key restricted to that repository
// funds it (secrets.WithLaunchRepo reads spec.ProjectPath at the walk).
func TestScheduledLaunchNamesItsRepository(t *testing.T) {
	t.Run("a schedule bound to no integration names its clone URL's repository", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
		sb := scheduledBot("t1")
		sb.RepoURL = "https://github.com/acme/api.git"
		if err := s.launchScheduledBot(spineCtx(t), sb); err != nil {
			t.Fatalf("launchScheduledBot = %v", err)
		}
		got := pub.lastSpec().ProjectPath
		if got != "acme/api" {
			t.Fatalf("ProjectPath = %q, want acme/api — the repository the schedule clones", got)
		}
		if !(secrets.ApiKey{Repos: []string{"acme/api"}}).ServesRepoForLaunch(got) {
			t.Error("a key restricted to the cloned repository would still be withheld from the schedule")
		}
	})
	t.Run("a schedule bound to an integration names the integration's repository", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		got := w.tick(t, boundSchedule(nil)).ProjectPath
		if got != pinWorldRepo {
			t.Fatalf("ProjectPath = %q, want %q — the integration's repository as the forge writes it", got, pinWorldRepo)
		}
		if !(secrets.ApiKey{Repos: []string{"group/veille"}}).ServesRepoForLaunch(got) {
			t.Error("a key restricted to the integration's repository would still be withheld from its schedule")
		}
	})
	t.Run("a schedule that clones nothing names no repository", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
		if err := s.launchScheduledBot(spineCtx(t), scheduledBot("t1")); err != nil {
			t.Fatalf("launchScheduledBot = %v", err)
		}
		if got := pub.lastSpec().ProjectPath; got != "" {
			t.Fatalf("ProjectPath = %q for a schedule with no repository, want none", got)
		}
	})
}

// TestScheduledProjectPath walks each reading of scheduledProjectPath.
func TestScheduledProjectPath(t *testing.T) {
	gitlab := func(base string) *scheduledBinding {
		return &scheduledBinding{
			integration: forge.RepoIntegration{RepoFullName: "Group/Api"},
			connection:  forge.Connection{Provider: forge.ProviderGitLab, ForgeBaseURL: base},
		}
	}
	cases := []struct {
		name    string
		repoURL string
		b       *scheduledBinding
		want    string
	}{
		{"no integration: the clone URL's slug", "https://github.com/acme/api.git", nil, "acme/api"},
		{"no integration, no clone: no repository", "", nil, ""},
		{"bound, cloning the integration's repository: the forge's spelling", "https://gitlab.com/group/api.git", gitlab(""), "Group/Api"},
		{"bound, cloning nothing: the integration's repository", "", gitlab(""), "Group/Api"},
		// The connection's sub-path base is stripped whole: read without it,
		// the base's own path would become a subgroup ("gitlab/group/api")
		// and the launch would name a repository nobody restricted a key to.
		{"bound to a sub-path forge", "https://host.example/gitlab/group/api.git", gitlab("https://host.example/gitlab"), "Group/Api"},
		// A repo_url edited away from the integration names what the run
		// actually clones, never the integration's repository.
		{"bound, cloning another repository", "https://gitlab.com/group/other.git", gitlab(""), "group/other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb := cloudsched.ScheduledBot{ID: "s1", TenantID: "t1", BotID: "probe", RepoURL: c.repoURL}
			if got := scheduledProjectPath(sb, c.b); got != c.want {
				t.Errorf("scheduledProjectPath(%q) = %q, want %q", c.repoURL, got, c.want)
			}
		})
	}
}

// TestLaunchScheduledBot_RepoQuotaBindsTheSchedule: the gate subject now
// carries the schedule's repository, so a repository over its monthly quota
// stops the tick before the run service is asked — the ceiling no cron
// cadence went around.
func TestLaunchScheduledBot_RepoQuotaBindsTheSchedule(t *testing.T) {
	newSrv := func(t *testing.T, spent float64) (*Server, *spinePublisher) {
		t.Helper()
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
		s.credUsage = credusage.NewMemoryCounter()
		withFloor(t, s, budgetfloor.Policy{RepoQuotas: []budgetfloor.RepoQuota{{Repo: "acme/widgets", MonthlyUSD: 10}}})
		if spent > 0 {
			seedRepoUsage(t, s.credUsage, "acme/widgets", spent, 3)
		}
		return s, pub
	}
	sb := scheduledBot("t1")
	sb.RepoURL = "https://github.com/acme/widgets.git/"

	s, pub := newSrv(t, 12)
	err := s.launchScheduledBot(spineCtx(t), sb)
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyRepoQuota {
		t.Fatalf("launchScheduledBot = %v, want the %s denial — the repository is over its quota", err, denyRepoQuota)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("the run service was asked %d time(s) past the repository's quota, want 0", got)
	}

	s, pub = newSrv(t, 4)
	if err := s.launchScheduledBot(spineCtx(t), sb); err != nil {
		t.Fatalf("launchScheduledBot under the quota = %v, want a launch", err)
	}
	if got := pub.count(); got != 1 {
		t.Errorf("the run service was asked %d time(s) under the quota, want 1", got)
	}
}

// TestLaunchScheduledBot_KeyPins: the tick carries the integration's pins,
// read live, under the schedule's own, which win provider by provider and
// are judged at the tick; every way the pins cannot be read or honoured
// refuses the tick by name before the run service is asked.
func TestLaunchScheduledBot_KeyPins(t *testing.T) {
	integrationPins := map[string]string{"openai_compatible": "key-tech", "anthropic": "key-anthropic"}

	t.Run("nothing pinned anywhere: the launch carries no pins", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		if got := w.tick(t, boundSchedule(nil)).KeyOverrides; got != nil {
			t.Fatalf("KeyOverrides = %v, want none", got)
		}
	})

	t.Run("the integration's pins are read live at the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		if got := w.tick(t, boundSchedule(nil)).KeyOverrides; !maps.Equal(got, integrationPins) {
			t.Fatalf("KeyOverrides = %v, want the integration's %v", got, integrationPins)
		}
		repinned := map[string]string{"openai_compatible": "key-design"}
		w.setIntegrationPins(t, repinned)
		if got := w.tick(t, boundSchedule(nil)).KeyOverrides; !maps.Equal(got, repinned) {
			t.Fatalf("after the integration repinned: KeyOverrides = %v, want %v — a copy frozen earlier would still spend the old key", got, repinned)
		}
	})

	t.Run("the schedule's own pin wins provider by provider", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		got := w.tick(t, boundSchedule(map[string]string{"openai_compatible": "key-design"})).KeyOverrides
		want := map[string]string{"openai_compatible": "key-design", "anthropic": "key-anthropic"}
		if !maps.Equal(got, want) {
			t.Fatalf("KeyOverrides = %v, want %v — the schedule's gateway pin over the integration's, the integration's other pin kept", got, want)
		}
	})

	t.Run("a schedule bound to no integration carries its own pins", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		sb := scheduledBot("t1")
		sb.KeyOverrides = map[string]string{"openai_compatible": "key-design"}
		if got := w.tick(t, sb).KeyOverrides; !maps.Equal(got, sb.KeyOverrides) {
			t.Fatalf("KeyOverrides = %v, want %v", got, sb.KeyOverrides)
		}
	})

	t.Run("an integration whose webhook config is another team's refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		cfg, err := w.s.webhookConfigs.Get(context.Background(), pinWorldWebhook)
		if err != nil {
			t.Fatal(err)
		}
		cfg.TenantID = "t2"
		cfg.KeyOverrides = map[string]string{"openai_compatible": "key-of-t2"}
		if err := w.s.webhookConfigs.Update(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		w.refused(t, boundSchedule(nil), pinWorldWebhook, "another tenant")
	})

	t.Run("an integration naming a webhook config that does not exist refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		integ, err := w.s.forgeIntegrations.Get(context.Background(), pinWorldIntegration)
		if err != nil {
			t.Fatal(err)
		}
		integ.WebhookID = "wh-gone"
		if err := w.s.forgeIntegrations.Update(context.Background(), integ); err != nil {
			t.Fatal(err)
		}
		w.refused(t, boundSchedule(nil), "wh-gone", "key pins")
	})

	t.Run("a server with no webhook config store refuses a bound tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		w.s.webhookConfigs = nil
		w.refused(t, boundSchedule(nil), pinWorldWebhook, "cannot read")
	})

	t.Run("a pin whose key was deleted refuses the tick by name", func(t *testing.T) {
		w := newScheduledPinWorld(t, integrationPins)
		if err := w.keys.Delete(store.WithTenant(context.Background(), "t1"), "key-design"); err != nil {
			t.Fatal(err)
		}
		w.refused(t, boundSchedule(map[string]string{"openai_compatible": "key-design"}),
			"key_overrides[openai_compatible]", "key-design", "no longer exists")
	})

	t.Run("a pin whose key left the lane's sight refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		personal := secrets.ApiKey{
			ID: "key-alice", ScopeTeamID: "t1", ScopeUserID: "alice", Provider: secrets.ProviderOpenAICompatible,
			Name: "alice", Fingerprint: "fp-alice", CreatedAt: time.Now().UTC(),
		}
		if err := w.keys.Create(store.WithTenant(context.Background(), "t1"), personal); err != nil {
			t.Fatal(err)
		}
		w.refused(t, boundSchedule(map[string]string{"openai_compatible": "key-alice"}), "key-alice", "not spendable")
	})

	t.Run("a pin the server has no key store to honour refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		w.s.apiKeys = nil
		w.refused(t, boundSchedule(map[string]string{"openai_compatible": "key-design"}), "no api key store")
	})
}
