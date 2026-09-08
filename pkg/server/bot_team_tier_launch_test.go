package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A team's own bot row serves EVERY launch surface of that team, not only
// the manual studio launch: the board dispatcher, the trigger spine, the
// cloud scheduler and the inbound webhooks resolve through the same
// team → platform → baked order (#871, docs/platform-bots.md). Four
// surfaces, four proofs: the defect existed because four launchers were
// assumed to behave like the fifth.

const (
	tierBakedBot = "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"BAKED\"}'`\n  output: probe_out\n\nworkflow tier_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
	tierForkBot  = "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"TEAMFORK\"}'`\n  output: probe_out\n\nworkflow tier_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
)

// tierPublisher records every LaunchSpec a surface publishes — the spec is
// what carries the resolved bundle, so it is the oracle for "which tier
// served this launch". onLaunch plays the runner pod for surfaces that wait
// on the run's outcome.
type tierPublisher struct {
	mu       sync.Mutex
	specs    []runview.LaunchSpec
	onLaunch func(runID string)
}

func (p *tierPublisher) SubmitLaunch(_ context.Context, runID string, spec runview.LaunchSpec, _ *ir.Workflow, _ string) (int, error) {
	p.mu.Lock()
	p.specs = append(p.specs, spec)
	p.mu.Unlock()
	if p.onLaunch != nil {
		p.onLaunch(runID)
	}
	return 0, nil
}

func (*tierPublisher) CancelRun(context.Context, string) error { return nil }
func (*tierPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (*tierPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, string) error {
	return nil
}

// only returns the single spec published, failing when a surface published
// none (or more than one).
func (p *tierPublisher) only(t *testing.T) runview.LaunchSpec {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.specs) != 1 {
		t.Fatalf("the surface published %d launches, want exactly 1", len(p.specs))
	}
	return p.specs[0]
}

// newTeamForkServer is a cloud-shaped server whose BAKED catalog holds
// `probe`, and whose team `t1` has forked that very slug — the studio-editor
// shape (pkg/botsource) the ticket is about.
func newTeamForkServer(t *testing.T, pub *tierPublisher) (*Server, *store.FilesystemRunStore) {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(tierBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "probe",
		Files: map[string]string{botsource.MainBotFile: tierForkBot},
	}); err != nil {
		t.Fatalf("seed the team's fork: %v", err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	return s, rs
}

// assertServedByTheFork is the shared verdict: the launch carries the team's
// own bundle, and says so.
func assertServedByTheFork(t *testing.T, surface string, spec runview.LaunchSpec) {
	t.Helper()
	if !strings.Contains(spec.Source, "TEAMFORK") {
		t.Errorf("%s launched a bundle that is not the team's fork of `probe` (source: %q)", surface, spec.Source)
	}
	if spec.BotBundle == nil || spec.BotBundle.TenantID != "t1" {
		t.Errorf("%s: bundle ref = %+v, want the team's row (tenant t1) so the runner rebuilds the fork, not the baked twin", surface, spec.BotBundle)
	}
	if spec.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("%s: bot_source_tier = %q, want %q — a launch must say which tier served it", surface, spec.BotSourceTier, store.BotSourceTierTeam)
	}
}

// Surface 1/4 — the board dispatcher (processBoardCard).
func TestTeamForkServesTheBoardDispatcher(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }

	if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:1", Bot: "probe", State: native.StateReady,
	}); err != nil {
		t.Fatalf("processBoardCard = %v, want nil", err)
	}
	assertServedByTheFork(t, "board dispatch", pub.only(t))
}

// Surface 2/4 — the trigger spine's direct launch (serviceLauncher.Launch).
func TestTeamForkServesTheTriggerSpine(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)

	if _, err := s.triggerLauncher().Launch(boundedCtx(t), trigger.LaunchPlan{
		BotID:    "probe",
		TenantID: "t1",
		Mode:     bundle.ExecutionDirect,
		Event: trigger.Event{
			ID: "custom:probe:1", Source: trigger.SourceCustom, Kind: "probe",
			TenantID: "t1", OccurredAt: time.Now().UTC(),
		},
	}); err != nil {
		t.Fatalf("trigger Launch = %v, want nil", err)
	}
	assertServedByTheFork(t, "trigger spine", pub.only(t))
}

// Surface 3/4 — the cloud scheduler's tick (launchScheduledBot).
func TestTeamForkServesTheCloudSchedule(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)

	if err := s.launchScheduledBot(boundedCtx(t), cloudsched.ScheduledBot{
		ID: "sched-1", TenantID: "t1", BotID: "probe", Cron: "0 2 * * 1",
	}); err != nil {
		t.Fatalf("launchScheduledBot = %v, want nil", err)
	}
	assertServedByTheFork(t, "cloud schedule", pub.only(t))
}

// Surface 4/4 — an inbound webhook delivery (launchWebhookBot).
func TestTeamForkServesTheInboundWebhook(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)

	if _, err := s.launchWebhookBot(boundedCtx(t), webhooks.Config{ID: "wh-1", TenantID: "t1"},
		"probe", map[string]string{}, "", "", "acme/repo", nil, nil); err != nil {
		t.Fatalf("launchWebhookBot = %v, want nil", err)
	}
	assertServedByTheFork(t, "inbound webhook", pub.only(t))
}

// The stamp answers for the OTHER two tiers as well — otherwise
// `bot_source_tier` would only ever be written when it happens to say
// "team", and the field that exists to make the tier visible would itself
// be a partial answer.
func TestBotSourceTierNamesEveryTier(t *testing.T) {
	tiers := []struct {
		name  string
		rows  map[string]string // tenant → main.bot marker
		want  string
		tenID string
	}{
		{"a team row is `team`", map[string]string{"t1": tierForkBot}, store.BotSourceTierTeam, "t1"},
		{"a platform override is `platform`", map[string]string{botsource.PlatformTenantID: tierForkBot}, store.BotSourceTierPlatform, botsource.PlatformTenantID},
		{"the baked catalog is `baked`", nil, store.BotSourceTierBaked, ""},
	}
	for _, tc := range tiers {
		t.Run(tc.name, func(t *testing.T) {
			pub := &tierPublisher{}
			s := newOrgTestServer(t)
			seedGate(t, s, gateSpec{id: "t1"})
			botsDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(tierBakedBot), 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Bots.Paths = []string{botsDir}
			s.botSources = botsource.NewMemoryStore()
			for tenant, body := range tc.rows {
				if _, err := s.botSources.Create(store.WithTenant(context.Background(), tenant), botsource.BotSource{
					TenantID: tenant, Slug: "probe",
					Files: map[string]string{botsource.MainBotFile: body},
				}); err != nil {
					t.Fatalf("seed %s row: %v", tenant, err)
				}
			}
			s.invalidatePlatformBots()
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Store = rs
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))

			if err := s.launchScheduledBot(boundedCtx(t), cloudsched.ScheduledBot{
				ID: "sched-1", TenantID: "t1", BotID: "probe", Cron: "0 2 * * 1",
			}); err != nil {
				t.Fatalf("launchScheduledBot = %v, want nil", err)
			}
			spec := pub.only(t)
			if spec.BotSourceTier != tc.want {
				t.Errorf("bot_source_tier = %q, want %q", spec.BotSourceTier, tc.want)
			}
			gotTenant := ""
			if spec.BotBundle != nil {
				gotTenant = spec.BotBundle.TenantID
			}
			if gotTenant != tc.tenID {
				t.Errorf("bundle ref tenant = %q, want %q", gotTenant, tc.tenID)
			}
		})
	}
}
