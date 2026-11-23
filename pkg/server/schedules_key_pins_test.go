package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestScheduledProjectPath walks each reading of scheduledProjectPath; a
// want of "" with refused set is a tick refused by name.
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
		refused bool
	}{
		{"no integration: the clone URL's slug", "https://github.com/acme/api.git", nil, "acme/api", false},
		{"no integration, no clone: no repository", "", nil, "", false},
		{"bound, cloning the integration's repository: the forge's spelling", "https://gitlab.com/group/api.git", gitlab(""), "Group/Api", false},
		{"bound, cloning nothing: the integration's repository", "", gitlab(""), "Group/Api", false},
		// The connection's sub-path base is stripped whole: read without it,
		// the base's own path would become a subgroup ("gitlab/group/api")
		// and the launch would name a repository nobody restricted a key to.
		{"bound to a sub-path forge", "https://host.example/gitlab/group/api.git", gitlab("https://host.example/gitlab"), "Group/Api", false},
		// A repo_url edited away from the integration names what the run
		// actually clones, never the integration's repository.
		{"bound, cloning another repository", "https://gitlab.com/group/other.git", gitlab(""), "group/other", false},
		{"bound, cloning another repository of a sub-path forge", "HTTP://host.example/gitlab/group/other.git", gitlab("https://host.example/gitlab"), "group/other", false},
		{"bound, an ssh URL of the integration's forge", "git@gitlab.com:Group/Api.git", gitlab(""), "Group/Api", false},
		// Off the integration's forge: refused, whatever the slug says.
		{"bound, another host", "https://attacker.example/Group/Api.git", gitlab(""), "", true},
		{"bound, a host prefixed by the forge's", "https://gitlab.com.attacker.example/group/api.git", gitlab(""), "", true},
		{"bound, a userinfo naming the forge", "https://gitlab.com@attacker.example/group/api.git", gitlab(""), "", true},
		{"bound, a port the forge does not carry", "https://gitlab.com:8443/group/api.git", gitlab(""), "", true},
		{"bound, outside the sub-path base", "https://host.example/other/group/api.git", gitlab("https://host.example/gitlab"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb := cloudsched.ScheduledBot{ID: "s1", TenantID: "t1", BotID: "probe", RepoURL: c.repoURL}
			got, err := scheduledProjectPath(sb, c.b)
			if (err != nil) != c.refused {
				t.Fatalf("scheduledProjectPath(%q) error = %v, want refused=%v", c.repoURL, err, c.refused)
			}
			if got != c.want {
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

	// git (libcurl) collapses "." and ".." path segments before it asks the
	// forge, so each spelling below clones acme/widgets and is held to its
	// quota.
	for _, repoURL := range []string{
		sb.RepoURL,
		"https://github.com/x/../acme/widgets.git",
		"https://github.com/./acme/widgets.git",
		"https://github.com/acme/./widgets.git",
		"https://github.com/acme//widgets.git",
	} {
		s, pub := newSrv(t, 12)
		over := sb
		over.RepoURL = repoURL
		err := s.launchScheduledBot(spineCtx(t), over)
		var deny *launchDeniedError
		if !errors.As(err, &deny) || deny.Reason != denyRepoQuota {
			t.Fatalf("launchScheduledBot(%q) = %v, want the %s denial — the repository is over its quota", repoURL, err, denyRepoQuota)
		}
		if got := pub.count(); got != 0 {
			t.Errorf("the run service was asked %d time(s) past the repository's quota for %q, want 0", got, repoURL)
		}
	}

	s, pub := newSrv(t, 4)
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

	t.Run("a pin naming another team's key refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		seedSweepKey(t, w.keys, "t2", "key-of-t2", secrets.ProviderOpenAICompatible)
		w.refused(t, boundSchedule(map[string]string{"openai_compatible": "key-of-t2"}), "key_overrides[openai_compatible]", "key-of-t2")
	})

	t.Run("a pin the server has no key store to honour refuses the tick", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		w.s.apiKeys = nil
		w.refused(t, boundSchedule(map[string]string{"openai_compatible": "key-design"}), "no api key store")
	})
}

// narrowKeyAudience re-scopes a key through the real key PATCH route — the
// only re-scope the API offers: `bots` and `repos` (the route refuses a
// scope, team or provider field as unknown).
func (w *scheduledPinWorld) narrowKeyAudience(t *testing.T, keyID, body string) {
	t.Helper()
	r := httptest.NewRequest("PATCH", "/api/teams/t1/api-keys/"+keyID, strings.NewReader(body))
	r = r.WithContext(superAdminCtx())
	r.SetPathValue("id", "t1")
	r.SetPathValue("key_id", keyID)
	rw := httptest.NewRecorder()
	w.s.handleUpdateApiKey(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("key PATCH %s = %d %s", body, rw.Code, rw.Body.String())
	}
}

// TestLaunchScheduledBot_APinWhoseAudienceExcludesTheScheduleRefusesTheTick:
// a key's `bots` and `repos` audiences are what the key PATCH route can
// still move after a schedule pinned it. The walk withholds a key whose
// audience excludes the launch and, for a vendor provider, spends the
// team's next key in silence — so the tick judges both audiences against
// the bot it launches and the repository it names, and refuses by name.
func TestLaunchScheduledBot_APinWhoseAudienceExcludesTheScheduleRefusesTheTick(t *testing.T) {
	t.Run("a bots audience narrowed away from the schedule's bot", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		sb := boundSchedule(map[string]string{"anthropic": "key-anthropic"})
		w.tick(t, sb)
		w.narrowKeyAudience(t, "key-anthropic", `{"bots":["some-other-bot"]}`)
		w.refused(t, sb, "key_overrides[anthropic]", "key-anthropic", "bot")
	})
	t.Run("a repos audience narrowed away from the repository the schedule names", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		sb := boundSchedule(map[string]string{"anthropic": "key-anthropic"})
		w.narrowKeyAudience(t, "key-anthropic", `{"repos":["group/other"]}`)
		w.refused(t, sb, "key_overrides[anthropic]", "key-anthropic", "repository")
	})
	t.Run("the bot is read as the walk reads it", func(t *testing.T) {
		// The key route stores a bots entry normalised ("feed_watch" →
		// "feed-watch") and the walk compares it with the normalised bot id,
		// so the tick reads the schedule's bot the same way.
		w := newScheduledPinWorld(t, nil)
		if err := os.WriteFile(filepath.Join(w.s.cfg.Bots.Paths[0], "feed_watch.bot"), []byte(spineProbeBot), 0o600); err != nil {
			t.Fatal(err)
		}
		sb := boundSchedule(map[string]string{"anthropic": "key-anthropic"})
		sb.BotID = "feed_watch"
		w.narrowKeyAudience(t, "key-anthropic", `{"bots":["feed_watch"]}`)
		if got := w.tick(t, sb).KeyOverrides; got["anthropic"] != "key-anthropic" {
			t.Fatalf("KeyOverrides = %v, want the pin kept", got)
		}
	})
	t.Run("audiences naming the schedule's bot and repository launch", func(t *testing.T) {
		w := newScheduledPinWorld(t, nil)
		sb := boundSchedule(map[string]string{"anthropic": "key-anthropic"})
		// The audience reads the bot as the walk does (botregistry's
		// normalisation) and the repository case-folded.
		w.narrowKeyAudience(t, "key-anthropic", `{"bots":["Probe"],"repos":["group/veille"]}`)
		if got := w.tick(t, sb).KeyOverrides; got["anthropic"] != "key-anthropic" {
			t.Fatalf("KeyOverrides = %v, want the pin kept", got)
		}
	})
}

// TestLaunchScheduledBot_ABoundScheduleCloningOffItsForgeIsRefused: the
// integration's forge token is aimed at the host it belongs to only — the
// REST lane's rule. A schedule bound to an integration whose repo_url names
// another host is refused by name before the token is minted and before its
// pins are judged, instead of handing that host the token and funding its
// clone with the integration's keys.
func TestLaunchScheduledBot_ABoundScheduleCloningOffItsForgeIsRefused(t *testing.T) {
	for _, repoURL := range []string{
		"https://attacker.example/Group/Veille.git",
		"https://gitlab.com.attacker.example/group/veille.git",
		"https://gitlab.com@attacker.example/group/veille.git",
		"https://gitlab.com:8443/group/veille.git",
	} {
		for _, own := range []map[string]string{
			nil,
			// The refusal names the host before the pins are judged: this
			// pin names a key that does not exist.
			{"openai_compatible": "key-gone"},
		} {
			t.Run(fmt.Sprintf("%s pinning %v", repoURL, own), func(t *testing.T) {
				w := newScheduledPinWorld(t, map[string]string{"openai_compatible": "key-tech"})
				sb := boundSchedule(own)
				sb.RepoURL = repoURL
				err := w.s.launchScheduledBot(spineCtx(t), sb)
				if err == nil || !strings.Contains(err.Error(), pinWorldIntegration) || !strings.Contains(err.Error(), "not on") {
					t.Errorf("launchScheduledBot = %v, want a refusal naming the integration and the host", err)
				}
				if got := w.pub.count(); got != 0 {
					t.Errorf("the run service was asked %d time(s) for a clone off the integration's forge, want 0", got)
				}
				conn, err := w.s.forgeConnections.Get(context.Background(), "t1", "c1")
				if err != nil {
					t.Fatal(err)
				}
				if conn.ManagedSecretID != "" {
					t.Errorf("the refused tick minted the integration's forge token (%s)", conn.ManagedSecretID)
				}
			})
		}
	}
}

// addSubPathIntegration gives the world a second integration of t1, on a
// GitLab served under a sub-path, for the repository Group/Veille.
func (w *scheduledPinWorld) addSubPathIntegration(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	sealed, err := connection.SealForgePAT(w.s.sealer, "c-sub", "glpat-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.forgeConnections.Create(ctx, forge.Connection{ID: "c-sub", TenantID: "t1", Provider: forge.ProviderGitLab, ForgeBaseURL: "https://host.example/gitlab", SealedPayload: sealed}); err != nil {
		t.Fatal(err)
	}
	if err := w.s.forgeIntegrations.Create(ctx, forge.RepoIntegration{
		ID: "ri-sub", TenantID: "t1", ConnectionID: "c-sub", Provider: forge.ProviderGitLab, RepoFullName: pinWorldRepo,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestScheduledLaunch_EverySpellingTheBindingAcceptsNamesTheIntegrationsRepository:
// the create route binds a repo_url to an integration case-, scheme- and
// scp-insensitively (normalizeRepoURL); the tick must read the same URL the
// same way. On a sub-path forge a case- or scheme-sensitive reading takes
// the base's own path for a subgroup and names "gitlab/group/veille" — a
// repository no key is restricted to and no quota binds.
func TestScheduledLaunch_EverySpellingTheBindingAcceptsNamesTheIntegrationsRepository(t *testing.T) {
	for _, repoURL := range []string{
		"https://host.example/gitlab/group/veille.git",
		"http://host.example/gitlab/group/veille.git",
		"https://HOST.example/gitlab/group/veille.git",
		"ssh://git@host.example/gitlab/group/veille.git",
		"HTTPS://host.example/gitlab/group/veille",
	} {
		t.Run(repoURL, func(t *testing.T) {
			w := newScheduledPinWorld(t, nil)
			w.addSubPathIntegration(t)
			bound := w.s.resolveRepoIntegrationID(store.WithTenant(context.Background(), "t1"), "t1", repoURL)
			if bound != "ri-sub" {
				t.Fatalf("the create route binds %q to %q, want ri-sub — the precondition of this test", repoURL, bound)
			}
			sb := scheduledBot("t1")
			sb.RepoIntegrationID, sb.RepoURL = bound, repoURL
			if got := w.tick(t, sb).ProjectPath; got != pinWorldRepo {
				t.Fatalf("ProjectPath = %q, want %q — the repository the schedule is bound to", got, pinWorldRepo)
			}
		})
	}
}

// TestScheduleBindingImpliesItsForgeHost: the create route's binding
// (repoURLNamesRepo) and the tick's host reading (repoOnForgeBase) read a
// repo_url through one normalisation, so no spelling is bound to an
// integration by the one and refused as off its forge by the other — over
// every combination of scheme, host, separator, base path, repository and
// suffix spellings below, against each base.
func TestScheduleBindingImpliesItsForgeHost(t *testing.T) {
	bases := []string{"https://gitlab.com", "https://host.example/gitlab", "https://host.example/gitlab/", "https://forge.example:8443"}
	schemes := []string{"https://", "http://", "HTTPS://", "ssh://", "ssh://git@", "git@", ""}
	hosts := []string{"gitlab.com", "GitLab.com", "gitlab.com:8443", "gitlab.com@evil.example", "host.example", "HOST.example", "forge.example:8443", "forge.example"}
	seps := []string{"/", ":"}
	paths := []string{"", "gitlab/", "GitLab/", "other/", "8443/"}
	repos := []string{"Group/Veille", "group/veille", "Group/Other", "x/../Group/Veille"}
	suffixes := []string{"", ".git", ".GIT", "/", ".git/"}
	bound, refused := 0, 0
	for _, base := range bases {
		for _, sc := range schemes {
			for _, h := range hosts {
				for _, sep := range seps {
					for _, p := range paths {
						for _, r := range repos {
							for _, sfx := range suffixes {
								u := sc + h + sep + p + r + sfx
								_, onForge := repoOnForgeBase(u, base)
								if !onForge {
									refused++
								}
								if !repoURLNamesRepo(u, base, "Group/Veille") {
									continue
								}
								bound++
								if !onForge {
									t.Errorf("repo_url %q is bound to Group/Veille on %s, yet read as off that forge", u, base)
								}
							}
						}
					}
				}
			}
		}
	}
	if bound == 0 || refused == 0 {
		t.Fatalf("bound=%d refused=%d: the spellings exercise only one side", bound, refused)
	}
	t.Logf("%d bound spellings, every one on its forge (%d spellings read as off their forge)", bound, refused)
}
