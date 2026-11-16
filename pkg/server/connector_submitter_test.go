package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func submitChatPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: "chat"},
		Triggers: []spec.Trigger{{ID: "chat.message.posted", Event: "deploy",
			Filters: []spec.TriggerFilter{{ID: "room", From: "payload.room", Type: "string"}},
			Outputs: []spec.TriggerOutput{{Name: "prompt", From: "payload.text"}, {Name: "repo_url", From: "payload.repo"}},
		}},
	}
}

// planLauncher records the plans it is asked to launch.
type planLauncher struct {
	plans []trigger.LaunchPlan
	err   error
}

func (l *planLauncher) Launch(_ context.Context, p trigger.LaunchPlan) (string, error) {
	l.plans = append(l.plans, p)
	if l.err != nil {
		return "", l.err
	}
	return "run-1", nil
}

type submitWorld struct {
	sub    *connectorSubmitter
	subs   *trigger.MemorySubscriptionStore
	launch *planLauncher
	in     intake.Intake
	pair   intake.Pair
	bots   map[string][]string
	botErr error
}

func newSubmitWorld(t *testing.T, repoURL string) *submitWorld {
	t.Helper()
	subs := trigger.NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), trigger.Subscription{
		ID: "s1", TenantID: "t1", BotID: "chatbot", Enabled: true, Invocation: bundle.InvocationKindConnector,
		ConnectionID: "conn-chat", TriggerID: "chat.message.posted", Filters: map[string][]string{"room": {"ops"}},
		KeyOverrides: map[string]string{"anthropic": "key-1"},
	}); err != nil {
		t.Fatal(err)
	}
	w := &submitWorld{subs: subs, launch: &planLauncher{}, bots: map[string][]string{"chatbot": {"prompt"}}}
	cat := connection.NewMemoryCatalog(submitChatPackage())
	w.sub = &connectorSubmitter{
		subs:     subs,
		matcher:  &binding.Matcher{Subscriptions: subs, Catalog: func(string) connection.Catalog { return cat }},
		launcher: w.launch,
		botVars: func(_ context.Context, _, bot string) ([]string, bool, error) {
			if w.botErr != nil {
				return nil, false, w.botErr
			}
			names, ok := w.bots[bot]
			return names, ok, nil
		},
		repoTrusted: func(_ context.Context, _ intake.Intake, host string) error {
			if host == "chat.example" {
				return nil
			}
			return intake.Refuse("untrusted host "+host, nil)
		},
	}
	w.in = intake.Intake{ID: "in-1", TenantID: "t1", ConnectionID: "conn-chat", IngressID: "ing-1", TriggerID: "chat.message.posted",
		Event: []byte(`{"outputs":{"prompt":"ship it","repo_url":"` + repoURL + `"},"payload":{"room":"ops","text":"ship it"},"headers":{}}`)}
	w.pair = intake.Pair{ID: "pr-1", TenantID: "t1", IntakeID: "in-1", SubscriptionID: "s1", Epoch: 2}
	return w
}

func pairRefusalOf(err error) (string, bool) {
	var r interface{ RefusalReason() string }
	if errors.As(err, &r) {
		return r.RefusalReason(), true
	}
	return "", false
}

// A matched pair launches the subscription as it stands, under the pair's
// submission key, with the provenance the outcome spine reads back.
func TestTheConnectorSubmitterLaunchesThePairsSubmission(t *testing.T) {
	w := newSubmitWorld(t, "https://chat.example/acme/app.git")
	runID, err := w.sub.Submit(context.Background(), w.in, w.pair)
	if err != nil || runID != "run-1" || len(w.launch.plans) != 1 {
		t.Fatalf("submit = %q, %v (%d launches)", runID, err, len(w.launch.plans))
	}
	p := w.launch.plans[0]
	if p.SubmissionKey == "" || p.SubmissionKey != w.pair.SubmissionKey() {
		t.Errorf("submission key = %q, want the pair's %q", p.SubmissionKey, w.pair.SubmissionKey())
	}
	if p.BotID != "chatbot" || p.TenantID != "t1" || p.Mode != bundle.ExecutionDirect || p.Vars["prompt"] != "ship it" {
		t.Errorf("plan = %+v", p)
	}
	if p.RepoURL != "https://chat.example/acme/app.git" || p.KeyOverrides["anthropic"] != "key-1" {
		t.Errorf("plan repo/pins = %q %v", p.RepoURL, p.KeyOverrides)
	}
	want := store.RunSource{Kind: store.RunSourceKindConnector, ScheduleID: "s1", PairID: "pr-1", IntakeID: "in-1"}
	if p.SourceRef == nil || *p.SourceRef != want {
		t.Errorf("provenance = %+v, want %+v", p.SourceRef, want)
	}
}

// What changed since the match decides (ADR-168 Q2): each change refuses
// the pair, naming it, and launches nothing.
func TestTheConnectorSubmitterRefusesWhatChangedSinceTheMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*trigger.Subscription) (remove bool)
		want   string
	}{
		"deleted":     {func(*trigger.Subscription) bool { return true }, "deleted"},
		"disabled":    {func(s *trigger.Subscription) bool { s.Enabled = false; return false }, "disabled"},
		"re-bound":    {func(s *trigger.Subscription) bool { s.ConnectionID = "conn-other"; return false }, "another connection"},
		"board mode":  {func(s *trigger.Subscription) bool { s.Mode = bundle.ExecutionBoard; return false }, "unsupported"},
		"refiltered":  {func(s *trigger.Subscription) bool { s.Filters = map[string][]string{"room": {"dev"}}; return false }, "no longer matches"},
		"bot unknown": {func(s *trigger.Subscription) bool { s.BotID = "ghost"; return false }, "not in the tenant's catalog"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSubmitWorld(t, "")
			ctx := context.Background()
			cur, _ := w.subs.Get(ctx, "s1")
			if tc.change(&cur) {
				if err := w.subs.Delete(ctx, "s1"); err != nil {
					t.Fatal(err)
				}
			} else if err := w.subs.Update(ctx, cur); err != nil {
				t.Fatal(err)
			}
			_, err := w.sub.Submit(ctx, w.in, w.pair)
			reason, refused := pairRefusalOf(err)
			if !refused || !strings.Contains(reason, tc.want) {
				t.Fatalf("submit = %v, want a refusal containing %q", err, tc.want)
			}
			if len(w.launch.plans) != 0 {
				t.Fatalf("a refused pair launched %d run(s)", len(w.launch.plans))
			}
		})
	}
}

// A repo_url is believed only where the judge allows it; an outage of what
// the launch reads is retried, not refused; the launcher's own classes pass
// through as they are.
func TestTheConnectorSubmitterGuardsTheCloneTarget(t *testing.T) {
	for url, want := range map[string]string{
		"https://evil.example/acme/app.git": "untrusted host evil.example",
		"not a url":                         "is not a URL",
	} {
		w := newSubmitWorld(t, url)
		_, err := w.sub.Submit(context.Background(), w.in, w.pair)
		if reason, refused := pairRefusalOf(err); !refused || !strings.Contains(reason, want) || len(w.launch.plans) != 0 {
			t.Errorf("repo_url %q = %v (%d launches), want a refusal containing %q", url, err, len(w.launch.plans), want)
		}
	}

	w := newSubmitWorld(t, "")
	w.botErr = errors.New("bot store down")
	if _, err := w.sub.Submit(context.Background(), w.in, w.pair); err == nil {
		t.Fatal("a bot store outage launched")
	} else if _, refused := pairRefusalOf(err); refused {
		t.Fatalf("a bot store outage refused the pair: %v", err)
	}

	w = newSubmitWorld(t, "")
	w.launch.err = &launchDeniedError{Reason: denyConcurrencyCap, RetryAfter: 20 * time.Second}
	_, err := w.sub.Submit(context.Background(), w.in, w.pair)
	var postponed interface{ PostponeFor() (time.Duration, bool) }
	if !errors.As(err, &postponed) {
		t.Fatalf("a concurrency denial = %v, want it to reach the worker as a postponement", err)
	}
	if d, ok := postponed.PostponeFor(); !ok || d != 20*time.Second {
		t.Fatalf("postponement = %v, %v; want 20s", d, ok)
	}
}

// The gate's transient denials postpone; the others spend an attempt.
func TestOnlyTransientDenialsPostpone(t *testing.T) {
	for reason, want := range map[string]bool{
		denyConcurrencyCap: true, denyLaunchRateLimited: true,
		denyMonthlyRunQuota: false, denyMonthlyCostCap: false, denyOrgSuspended: false,
	} {
		d, ok := (&launchDeniedError{Reason: reason}).PostponeFor()
		if ok != want || (ok && d <= 0) {
			t.Errorf("%s: PostponeFor = %v, %v; want postponed=%v with a positive wait", reason, d, ok, want)
		}
	}
}

// The repo_url judge believes the connection's own host and the hosts its
// ingress allows, and nothing else.
func TestTheRepoURLJudgeReadsTheConnectionAndItsIngress(t *testing.T) {
	f := newIngressFixture(t, ingChatPackage())
	ctx := context.Background()
	cfg := webhooks.Config{ID: "ing-1", TenantID: "t1", Provider: webhooks.ProviderConnector, RepoHosts: []string{"git.acme.example"}}
	if err := f.s.webhookConfigs.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	in := intake.Intake{TenantID: "t1", ConnectionID: "conn-chat", IngressID: "ing-1"}
	for host, trusted := range map[string]bool{"chat.example": true, "git.acme.example": true, "evil.example": false} {
		err := f.s.connectorRepoTrusted(ctx, in, host)
		if _, refused := pairRefusalOf(err); refused == trusted || (trusted && err != nil) {
			t.Errorf("host %s: %v, want trusted=%v", host, err, trusted)
		}
	}
	// Another tenant's intake, through its own connection on its own host,
	// names this ingress: it does not borrow the repo_hosts.
	theirs := ingConnection("chat")
	theirs.ID, theirs.TenantID, theirs.BaseURL = "conn-chat-t2", "t2", "https://chat.t2.example"
	if err := f.conns.Create(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	other := in
	other.TenantID, other.ConnectionID = "t2", "conn-chat-t2"
	if _, refused := pairRefusalOf(f.s.connectorRepoTrusted(ctx, other, "chat.t2.example")); refused {
		t.Fatal("the other tenant's own host was refused: the probe below would prove nothing")
	}
	if _, refused := pairRefusalOf(f.s.connectorRepoTrusted(ctx, other, "git.acme.example")); !refused {
		t.Error("another tenant's intake borrowed this ingress's repo_hosts")
	}
}

// A trigger its package no longer declares refuses the launch, even for a
// subscription with no filter to stumble on.
func TestTheConnectorSubmitterRefusesAVanishedTrigger(t *testing.T) {
	w := newSubmitWorld(t, "")
	ctx := context.Background()
	cur, _ := w.subs.Get(ctx, "s1")
	cur.Filters = nil
	if err := w.subs.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	bare := submitChatPackage()
	bare.Triggers = nil
	cat := connection.NewMemoryCatalog(bare)
	w.sub.matcher = &binding.Matcher{Subscriptions: w.subs, Catalog: func(string) connection.Catalog { return cat }}
	_, err := w.sub.Submit(ctx, w.in, w.pair)
	if reason, refused := pairRefusalOf(err); !refused || !strings.Contains(reason, "no longer declares") || len(w.launch.plans) != 0 {
		t.Fatalf("a vanished trigger = %v (%d launches), want a refusal naming it", err, len(w.launch.plans))
	}
}

// failingBotSources answers every read with an outage: the team tier's
// store is down.
type outageBotSources struct{ botsource.Store }

func (f outageBotSources) GetBySlug(context.Context, string, string) (botsource.BotSource, error) {
	return botsource.BotSource{}, errors.New("PROBE: the team bot store is down")
}

// The team tier of the bot catalog is read strictly: a store outage is a
// retried attempt, never a permanent refusal of the pair — only a bot
// absent from every tier refuses.
func TestTheConnectorSubmitterRetriesATeamTierOutage(t *testing.T) {
	w := newSubmitWorld(t, "")
	s := newOrgTestServer(t)
	s.cfg.TriggerStore = w.subs
	s.connectorCatalog = connection.NewMemoryCatalog(submitChatPackage())
	s.botSources = outageBotSources{botsource.NewMemoryStore()}
	sub := s.connectorSubmitter()
	if _, found, err := sub.botVars(context.Background(), "t1", "chatbot"); err == nil || found {
		t.Fatalf("a team tier outage = (%v, %v); want an error the worker retries", found, err)
	}
	_, err := sub.Submit(context.Background(), w.in, w.pair)
	if reason, refused := pairRefusalOf(err); refused {
		t.Fatalf("a team tier outage refused the pair (%s); want an error", reason)
	}
	if len(w.launch.plans) != 0 {
		t.Fatal("an outage launched")
	}
}

// The local scope (tenant "") holds no team tier: the fallback answers from
// the platform tier alone — a catalog bot's vars come through, and a bot
// absent everywhere is the clean absence, never an error.
func TestTheConnectorSubmitterReadsTheLocalScopeWithoutATeamTier(t *testing.T) {
	s := newOrgTestServer(t)
	botsDir := t.TempDir()
	dir := filepath.Join(botsDir, "chatbot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(chatBotMain), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: chatbot\nversion: 1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	sub := s.connectorSubmitter()
	names, found, err := sub.botVars(context.Background(), "", "chatbot")
	if err != nil || !found || len(names) == 0 {
		t.Fatalf("the local catalog bot = (%v, %v), want its vars", found, err)
	}
	if _, found, err := sub.botVars(context.Background(), "", "ghost"); err != nil || found {
		t.Fatalf("a bot absent everywhere = (%v, %v), want the clean absence", found, err)
	}
}
