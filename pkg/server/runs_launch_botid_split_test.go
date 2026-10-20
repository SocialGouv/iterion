package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1368: spec.BotID is a bot IDENTITY the server resolves from its own
// tiers, never the label the requester typed. On an inline-source launch
// the tiered resolution does not run, so nothing may read the typed label
// as an identity: the run is gated, funded, bound and attributed as the
// unnamed work it is. On a resolved launch the label is verified and
// refined to the canonical name discovery found — the same id whatever the
// caller typed, and the same id a team fork and a bare catalog produce.
//
// These witnesses sit at the one chokepoint the three credential readers
// (BYOK audience, bot secret bindings, credpool pledges) all hang from:
// the LaunchSpec the handler builds. Each reader's fail-closed behaviour
// on an empty id is witnessed at its own level
// (TestTheAudiencePredicateMatchesExactlyAndNothingNear,
// TestResolveGenericWithBindings_TierOrdering,
// TestAPledgeWithAnAudienceRefusesALaunchNamingNoBot).

// botidCapture records every LaunchSpec a launch publishes — the oracle for
// the bot identity the handler decided.
type botidCapture struct {
	mu    sync.Mutex
	specs []runview.LaunchSpec
}

func (c *botidCapture) SubmitLaunch(_ context.Context, _ string, spec runview.LaunchSpec, _ *ir.Workflow, _ *runview.CompiledSource) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.specs = append(c.specs, spec)
	return 0, nil
}

func (*botidCapture) CancelRun(context.Context, string) error { return nil }

func (*botidCapture) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (*botidCapture) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

func (c *botidCapture) only(t *testing.T) runview.LaunchSpec {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.specs) != 1 {
		t.Fatalf("the launch published %d specs, want exactly 1", len(c.specs))
	}
	return c.specs[0]
}

// newBotidSplitServer is a gated local-mode server whose baked catalog
// holds one loose bot, with the run service wired to the capturing
// publisher. Returns the run store so a test can shape cfg.Store around it
// (the launch gate reads an optional counter capability off it).
func newBotidSplitServer(t *testing.T, cap *botidCapture, catalogSlug string, gate gateSpec) (*Server, store.RunStore, context.Context) {
	t.Helper()
	s := newOrgTestServer(t)
	ctx := seedGate(t, s, gate)
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, catalogSlug+".bot"), []byte("workflow probe:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(cap))
	return s, rs, ctx
}

// An inline launch that NAMES a resolvable catalog bot must not carry that
// label as its bot identity: the resolution never ran, so the identity is
// empty and every bot-scoped read below the spec treats the run as naming
// no bot. (postLaunch, the shared driver, lives in
// runs_launch_rootcut_test.go.)
func TestAnInlineLaunchCarriesNoBotIdentity(t *testing.T) {
	cap := &botidCapture{}
	s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})

	rec := postLaunch(t, s, ctx, `{"file_path":"x.bot","source":"workflow probe:\n  entry: done\n","bot_id":"probe"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := cap.only(t).BotID; got != "" {
		t.Fatalf("spec.BotID = %q, want \"\" — the requester-typed label is not an identity the server resolved", got)
	}
}

// A launch's cost governance is judged on the bot identity the server
// resolved. An inline launch resolves nothing, so naming a reserved bot in
// the same request must not buy the launch that bot's reservation — the
// bands held for other workloads stay held against it.
func TestAnInlineLaunchCannotClaimAReservedBotsBand(t *testing.T) {
	cap := &botidCapture{}
	s, rs, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1", maxConcurrentRuns: 1})
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 0}
	s.orgUsage = orgusage.NewMemoryCounter()
	withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
		{BotID: "probe", Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
	}})

	rec := postLaunch(t, s, ctx, `{"file_path":"x.bot","source":"workflow probe:\n  entry: done\n","bot_id":"probe"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — an inline launch must be judged as work naming no bot, not as the reserved bot it named; body=%s", rec.Code, rec.Body.String())
	}

	// The positive half of the same contract: a launch the server DOES
	// resolve keeps its own band — the reservation never costs its holder.
	rec = postLaunch(t, s, ctx, `{"bot_id":"probe"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the resolved launch of the reserved bot itself = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// The catalog tier must hand back the name DISCOVERY found, not the
// spelling the caller typed: the identical launch must produce the same
// canonical bot id whether a team fork of the slug exists (stored tier,
// already canonical) or not (catalog tier). The run document, the BYOK
// audience lookup, bot-scoped memory and board attribution all key on this
// one value.
func TestACatalogLaunchCarriesTheDiscoveredCanonicalName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		teamRow bool
	}{
		{"no team fork — the catalog tier served", false},
		{"a team fork exists — the stored tier served", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &botidCapture{}
			s, _, ctx := newBotidSplitServer(t, cap, "probe-bot", gateSpec{id: "t1"})
			if tc.teamRow {
				if err := seedTeamForkRow(t, s, "t1", "probe-bot"); err != nil {
					t.Fatal(err)
				}
			}
			// The caller spells it with an underscore; the catalog bot is
			// slugged with the dash. The launched identity is the catalog's.
			rec := postLaunch(t, s, ctx, `{"bot_id":"probe_bot"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
			}
			if got := cap.only(t).BotID; got != "probe-bot" {
				t.Fatalf("spec.BotID = %q, want the discovered canonical \"probe-bot\" — the same launch must carry one identity on every tier", got)
			}
		})
	}
}

func seedTeamForkRow(t *testing.T, s *Server, tenant, slug string) error {
	t.Helper()
	s.botSources = botsource.NewMemoryStore()
	_, err := s.botSources.Create(store.WithTenant(context.Background(), tenant), botsource.BotSource{
		TenantID: tenant, Slug: slug,
		Files: map[string]string{botsource.MainBotFile: "workflow probe:\n  entry: done\n"},
	})
	return err
}

// A launch whose bot label resolves NOWHERE is refused, not demoted to
// unnamed work: the gate already judged that label, so the resolution is
// the one place it can die — otherwise a reservation held for a ghost id
// buys the launch the very band the honest unnamed launch is refused.
// The refusal runs in both modes (in cloud the same request is refused by
// the source requirement anyway; here it is refused at the resolution).
func TestALaunchNamingABotThatResolvesNowhereIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"a ghost id beside a file", `{"file_path":"x.bot","bot_id":"ghost"}`, http.StatusBadRequest},
		{"an unnamed launch is not the refusal", `{"file_path":"x.bot"}`, http.StatusAccepted},
		{"a resolvable id still launches", `{"bot_id":"probe"}`, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &botidCapture{}
			s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})
			work := t.TempDir()
			s.cfg.WorkDir = work
			if err := os.WriteFile(filepath.Join(work, "x.bot"), []byte("workflow probe:\n  entry: done\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := postLaunch(t, s, ctx, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			switch tc.name {
			case "a ghost id beside a file":
				if !strings.Contains(rec.Body.String(), `resolves nowhere`) {
					t.Fatalf("body = %s, want the refusal to name the unresolvable label", rec.Body.String())
				}
				if len(cap.specs) != 0 {
					t.Fatalf("a refused launch published %d specs", len(cap.specs))
				}
			case "a resolvable id still launches":
				if got := cap.only(t).BotID; got != "probe" {
					t.Fatalf("spec.BotID = %q, want \"probe\"", got)
				}
			}
		})
	}
}

// A launch carrying an inline source beside a bot_id launches unnamed (the
// arbitrated label-at-most shape), and says so on the log: a scoped key, a
// binding or a pledge stops applying to this launch, and the caller should
// learn why from the log rather than from funding that vanished in silence.
func TestALaunchIgnoringABotIDSaysSoOnTheLog(t *testing.T) {
	cap := &botidCapture{}
	s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})
	var logs syncBuffer
	s.logger = iterlog.New(iterlog.LevelWarn, &logs)

	rec := postLaunch(t, s, ctx, `{"file_path":"x.bot","source":"workflow probe:\n  entry: done\n","bot_id":"probe"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := cap.only(t).BotID; got != "" {
		t.Fatalf("spec.BotID = %q, want \"\"", got)
	}
	if !strings.Contains(logs.String(), `ignored beside an inline source`) {
		t.Fatalf("log = %q, want the ignored-bot_id warning", logs.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A fork stored under a spelling that differs from the catalog name
// launches under the ROW's own slug: the serving tier's own name is the
// decided semantics — a cross-tier fold would be the third spelling rule
// docs/byok.md rejects — and the divergence it costs is named there.
func TestAForkStoredUnderAnotherSpellingLaunchesUnderTheRowsOwnSlug(t *testing.T) {
	cap := &botidCapture{}
	s, _, ctx := newBotidSplitServer(t, cap, "probe-bot", gateSpec{id: "t1"})
	if err := seedTeamForkRow(t, s, "t1", "probe_bot"); err != nil {
		t.Fatal(err)
	}
	rec := postLaunch(t, s, ctx, `{"bot_id":"probe_bot"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := cap.only(t).BotID; got != "probe_bot" {
		t.Fatalf("spec.BotID = %q, want the serving row's own slug \"probe_bot\"", got)
	}
}

// The gate judges its subject against the reservations with EXACT equality
// (budgetfloor.sameID) while the tiered resolution tolerates spelling
// variants — so a tolerated spelling of a RESERVED bot is judged as other
// work: the launch is the reserved bot's own, yet it faces the very ceiling
// the bot's own reservation lowered. The gate comment names this residual.
// Folding the match onto the resolver's rule is the deliberate NOT-taken
// alternative — NormalizeName is many-to-one, so one reservation would
// shadow another and a bot could never find its own specifically-sized band
// — and this pin asserts the refusal: change that policy and this witness
// changes with it.
func TestAReservedBotsToleratedSpellingIsJudgedAsOtherWork(t *testing.T) {
	// The witness's two premises: the resolver tolerates the spelling, and
	// the tolerance is many-to-one (both stored spellings normalize onto one
	// key — the shadowing hazard that rules out the fold).
	if botregistry.NormalizeName("PROBE_BOT") != "probe-bot" {
		t.Fatalf("NormalizeName no longer tolerates PROBE_BOT beside probe-bot — the resolution this witness assumes moved")
	}
	if botregistry.NormalizeName("my_bot") != botregistry.NormalizeName("my-bot") {
		t.Fatalf("NormalizeName stopped folding my_bot and my-bot onto one key — the shadowing hazard the gate comment names is gone")
	}
	cap := &botidCapture{}
	s, rs, ctx := newBotidSplitServer(t, cap, "probe-bot", gateSpec{id: "t1", maxConcurrentRuns: 1})
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 0}
	s.orgUsage = orgusage.NewMemoryCounter()
	withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
		{BotID: "probe-bot", Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
	}})

	// The gate judges the raw label "PROBE_BOT", matches no reservation
	// exactly, and refuses on the band held FOR probe-bot — before the
	// resolution that would have carried the canonical id.
	rec := postLaunch(t, s, ctx, `{"bot_id":"PROBE_BOT"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — a tolerated spelling of the reserved bot is matched EXACTLY against reservations and judged as other work; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reserved for other workloads") {
		t.Fatalf("body = %s, want the reserved-elsewhere detail", rec.Body.String())
	}
	// Control: the exact spelling serves the holder — the reservation never
	// costs its holder a slot.
	rec = postLaunch(t, s, ctx, `{"bot_id":"probe-bot"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the exact spelling = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// Bot-scoped memory is the one bot-scoped read that does NOT see an empty id
// on a path where the launch resolution did not run: the executor derives
// the memory identity with runview.ResolveBotID — the explicit id, then the
// bundle's declared name, then a name derived from the bundle path — and the
// typed label never enters that rule. The declared name must beat the path:
// a `.botz` cache slot is named after its content hash, so a path-derived id
// would orphan the bot's memory on every edit.
func TestBotScopedMemoryKeysOnTheBundleRuleNotTheTypedLabel(t *testing.T) {
	if got := runview.ResolveBotID("probe-bot", "other-name", "cache/abc123/main.bot"); got != "probe-bot" {
		t.Fatalf("ResolveBotID(\"probe-bot\", ...) = %q, want the explicit id to win", got)
	}
	if got := runview.ResolveBotID("", "declared-name", "cache/abc123/main.bot"); got != "declared-name" {
		t.Fatalf("ResolveBotID(\"\", \"declared-name\", ...) = %q, want the bundle's declared name to beat the path-derived one", got)
	}
	if got := runview.ResolveBotID("", "probe-bot", "anywhere/probe-bot/main.bot"); got != "probe-bot" {
		t.Fatalf("ResolveBotID(\"\", \"probe-bot\", ...) = %q, want the bundle's declared name", got)
	}
	if got := runview.ResolveBotID("", "", "anywhere/probe-bot/main.bot"); got != "probe-bot" {
		t.Fatalf("ResolveBotID(\"\", \"\", <bundle path>) = %q, want the path-derived name", got)
	}
	if got := runview.ResolveBotID("", "", "standalone.bot"); got != "" {
		t.Fatalf("ResolveBotID(\"\", \"\", \"standalone.bot\") = %q, want empty — a standalone file invents no identity", got)
	}
}
