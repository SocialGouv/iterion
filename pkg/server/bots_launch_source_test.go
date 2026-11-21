package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #3416: a launch started from a bot is NAMED and PINNED. The launch form
// reads the bot's program from GET /api/v1/bots/{name}/launch-source and
// posts the bot's id beside the digest that read issued; the server launches
// the bot it resolves and refuses when its program is no longer that one.
//
// The witnesses below drive BOTH real handlers — the read and the launch —
// against the same server, so the claim "the form renders the program the
// launch runs" is asserted on the two ends of it, never on one alone. The
// oracle is what the run service hands its publisher: the LaunchSpec, as for
// #1368 (runs_launch_botid_split_test.go), and the COMPILED source — the
// bytes the launch's compile consumed and their identity, never a file read
// again after the fact.

// programCapture records every launch the run service publishes: its spec
// and the program its compile read.
type programCapture struct {
	mu       sync.Mutex
	launches []capturedLaunch
}

type capturedLaunch struct {
	spec runview.LaunchSpec
	// hash is the identity of the compiled program (CompiledSource.Hash),
	// main the entry source that compile consumed.
	hash, main string
}

func (c *programCapture) SubmitLaunch(_ context.Context, _ string, spec runview.LaunchSpec, _ *ir.Workflow, cs *runview.CompiledSource) (runview.LaunchSubmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := capturedLaunch{spec: spec}
	if cs != nil {
		l.hash, l.main = cs.Hash, cs.Files[cs.Main]
	}
	c.launches = append(c.launches, l)
	return runview.LaunchSubmission{}, nil
}

func (*programCapture) CancelRun(context.Context, string) error { return nil }

func (*programCapture) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (*programCapture) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

func (c *programCapture) all() []capturedLaunch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedLaunch(nil), c.launches...)
}

func (c *programCapture) only(t *testing.T) capturedLaunch {
	t.Helper()
	launches := c.all()
	if len(launches) != 1 {
		t.Fatalf("the launch published %d programs, want exactly 1", len(launches))
	}
	return launches[0]
}

// launchSourceBot is a workflow that compiles and names the tier it was
// stored in, so a source text says which tier served it.
func launchSourceBot(tier string) string {
	return "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"" + tier + "\"}'`\n  output: probe_out\n\nworkflow tier_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
}

// launchSourceFixture is a gated server of one team (t1) whose baked catalog
// holds the bundle `probe-bot`, with an empty bot-source store for the team
// and platform tiers, a capturing launch publisher and a monthly run counter.
type launchSourceFixture struct {
	s       *Server
	cap     *programCapture
	counter *orgusage.MemoryCounter
	// The four callers the access rule tells apart.
	member, admin, viewer, superAdmin context.Context
}

func newLaunchSourceFixture(t *testing.T, mode string) *launchSourceFixture {
	t.Helper()
	f := &launchSourceFixture{cap: &programCapture{}, counter: orgusage.NewMemoryCounter()}
	f.s = newOrgTestServer(t)
	f.member = seedGate(t, f.s, gateSpec{id: "t1"})
	f.s.cfg.Mode = mode
	catalog := t.TempDir()
	if err := os.MkdirAll(filepath.Join(catalog, "probe-bot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, "probe-bot", "main.bot"), []byte(launchSourceBot("BAKED")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, "probe-bot", "manifest.yaml"), []byte("name: probe-bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Bots.Paths = []string{catalog}
	f.s.botSources = botsource.NewMemoryStore()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Store = rs
	f.s.orgUsage = f.counter
	f.s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(f.cap))
	bg := context.Background()
	asTeam := func(id auth.Identity) context.Context {
		id.OrgID = "t1"
		return auth.WithIdentity(bg, id)
	}
	f.admin = asTeam(seedTeamMember(t, f.s, bg, "ad", identity.RoleAdmin))
	f.viewer = asTeam(seedTeamMember(t, f.s, bg, "vi", identity.RoleViewer))
	f.superAdmin = asTeam(auth.Identity{UserID: "admin", TeamID: "t1", IsSuperAdmin: true, Credential: auth.CredentialAccessToken})
	return f
}

// storeRow stores (or replaces) a one-file bot row of a tenant: the team's
// own, or the platform's override.
func (f *launchSourceFixture) storeRow(t *testing.T, tenant, slug, source string) {
	t.Helper()
	f.storeBundle(t, tenant, slug, map[string]string{botsource.MainBotFile: source})
}

// storeBundle stores (or replaces) a bot row of several files.
func (f *launchSourceFixture) storeBundle(t *testing.T, tenant, slug string, files map[string]string) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), tenant)
	prev, err := f.s.botSources.GetBySlug(ctx, tenant, slug)
	if err == nil {
		prev.Files = files
		if _, err := f.s.botSources.Update(ctx, prev); err != nil {
			t.Fatalf("update %s/%s: %v", tenant, slug, err)
		}
		return
	}
	if _, err := f.s.botSources.Create(ctx, botsource.BotSource{TenantID: tenant, Slug: slug, Files: files}); err != nil {
		t.Fatalf("create %s/%s: %v", tenant, slug, err)
	}
}

// readLaunchSource asks the route through the server's own mux, so the
// registration is part of what is witnessed.
func (f *launchSourceFixture) readLaunchSource(ctx context.Context, name string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/bots/"+name+"/launch-source", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.s.mux.ServeHTTP(w, r)
	return w
}

func (f *launchSourceFixture) launchSource(t *testing.T, ctx context.Context, name string) botLaunchSourceAnswer {
	t.Helper()
	w := f.readLaunchSource(ctx, name)
	if w.Code != http.StatusOK {
		t.Fatalf("GET launch-source of %q = %d, want 200; body=%s", name, w.Code, w.Body.String())
	}
	var got botLaunchSourceAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode the launch source: %v (%s)", err, w.Body.String())
	}
	return got
}

// botLaunchSourceAnswer is the route's answer as a client decodes it.
type botLaunchSourceAnswer struct {
	BotID    string                    `json:"bot_id"`
	Tier     string                    `json:"tier"`
	Source   string                    `json:"source"`
	SHA256   string                    `json:"sha256"`
	Document json.RawMessage           `json:"document"`
	Bindable bool                      `json:"bindable"`
	Unit     *unitInfo                 `json:"unit"`
	Presets  *botregistry.PresetsBlock `json:"presets"`
}

func (f *launchSourceFixture) meteredRuns(t *testing.T) int {
	t.Helper()
	return meteredRuns(t, f.counter)
}

func pinnedLaunch(botID, sha string) string {
	body, _ := json.Marshal(map[string]string{"bot_id": botID, "expect_source_sha256": sha})
	return string(body)
}

// The route answers the tier the launch runs — the team's row over the
// platform's override over the baked bundle — with the identity and the tier
// the launch then stamps, and the digest of the bytes the launch then runs.
// Each tier is read by the least-privileged caller its text is readable by.
func TestTheLaunchSourceIsTheProgramTheLaunchRuns(t *testing.T) {
	for _, mode := range []string{"local", "cloud"} {
		for _, tc := range []struct {
			name     string
			platform bool
			team     bool
			caller   func(*launchSourceFixture) context.Context
			wantTier string
			wantMark string
		}{
			{"the baked bundle alone", false, false, func(f *launchSourceFixture) context.Context { return f.member }, store.BotSourceTierBaked, "BAKED"},
			{"a platform override over the baked bundle", true, false, func(f *launchSourceFixture) context.Context { return f.superAdmin }, store.BotSourceTierPlatform, "PLATFORM"},
			{"the team's row over the baked bundle", false, true, func(f *launchSourceFixture) context.Context { return f.admin }, store.BotSourceTierTeam, "TEAM"},
			{"the team's row over both", true, true, func(f *launchSourceFixture) context.Context { return f.admin }, store.BotSourceTierTeam, "TEAM"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				f := newLaunchSourceFixture(t, mode)
				if tc.platform {
					f.storeRow(t, botsource.PlatformTenantID, "probe-bot", launchSourceBot("PLATFORM"))
				}
				if tc.team {
					f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
				}
				ctx := tc.caller(f)

				got := f.launchSource(t, ctx, "probe-bot")
				if got.BotID != "probe-bot" || got.Tier != tc.wantTier {
					t.Fatalf("launch source = (bot %q, tier %q), want (probe-bot, %s)", got.BotID, got.Tier, tc.wantTier)
				}
				if !strings.Contains(got.Source, tc.wantMark) {
					t.Fatalf("source = %q, want the %s tier's program", got.Source, tc.wantMark)
				}
				if !launchPinPattern.MatchString(got.SHA256) {
					t.Fatalf("sha256 = %q, want a sha256 in the shape a pin is checked for", got.SHA256)
				}
				if !got.Bindable || !strings.Contains(string(got.Document), "tier_probe") {
					t.Fatalf("document = %s (bindable %v), want the parsed program", got.Document, got.Bindable)
				}

				// A pin is the cloud lane's; elsewhere the launch by id is
				// the launch, and compiles the identity the route answered.
				body := pinnedLaunch(got.BotID, got.SHA256)
				if mode != "cloud" {
					body = `{"bot_id":"` + got.BotID + `"}`
				}
				rec := postLaunch(t, f.s, ctx, body)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("the launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
				}
				ran := f.cap.only(t)
				if ran.spec.BotID != got.BotID || ran.spec.BotSourceTier != got.Tier {
					t.Fatalf("the launch stamped (bot %q, tier %q), the form was told (bot %q, tier %q)", ran.spec.BotID, ran.spec.BotSourceTier, got.BotID, got.Tier)
				}
				// The two ends of the guarantee, on what the compile consumed.
				if ran.hash != got.SHA256 {
					t.Fatalf("the launch compiled a program of identity %q, the form was issued %q", ran.hash, got.SHA256)
				}
				if ran.main != got.Source {
					t.Fatalf("the launch compiled the entry source %q, the form rendered %q", ran.main, got.Source)
				}
			})
		}
	}
}

// The identity the route answers is the one discovery found, whatever
// spelling the caller typed — and a pinned launch posting it publishes that
// canonical identity (#1368's rule, kept on the named road).
func TestAPinnedLaunchCarriesTheCanonicalBotIdentity(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	got := f.launchSource(t, f.member, "probe_bot")
	if got.BotID != "probe-bot" {
		t.Fatalf("bot_id = %q, want the discovered canonical \"probe-bot\"", got.BotID)
	}
	rec := postLaunch(t, f.s, f.member, pinnedLaunch("probe_bot", got.SHA256))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the pinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if ran := f.cap.only(t); ran.spec.BotID != "probe-bot" {
		t.Fatalf("spec.BotID = %q, want the canonical \"probe-bot\"", ran.spec.BotID)
	}
	if got := f.meteredRuns(t); got != 1 {
		t.Fatalf("monthly runs = %d after one accepted launch, want 1", got)
	}
}

// The route grants no read another route refuses the same caller: a team
// row's text takes the right to edit the team's bots, a platform override's
// a platform admin, and the baked catalog is readable by any member. Each
// refusal is a whole sentence: which bundle, who may read it, what is left.
func TestTheLaunchSourceGrantsNoReadAnotherRouteRefuses(t *testing.T) {
	t.Run("a team row", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
		for name, ctx := range map[string]context.Context{"a member": f.member, "a viewer": f.viewer} {
			w := f.readLaunchSource(ctx, "probe-bot")
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s reading the team row = %d, want 403; body=%s", name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "TEAM") || strings.Contains(w.Body.String(), "tier_probe") {
				t.Fatalf("%s was refused with the program in the refusal: %s", name, w.Body.String())
			}
			if want := `bots: \"probe-bot\" is your team's own bot: its program is readable by a bot editor, a team admin or an owner of the team — you may still launch \"probe-bot\" by id (bot_id) from the CLI, the API or MCP, which shows no program`; !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s was refused with %s, want the whole sentence %s", name, w.Body.String(), want)
			}
		}
		if got := f.launchSource(t, f.admin, "probe-bot"); got.Tier != store.BotSourceTierTeam {
			t.Fatalf("a team admin reads tier %q, want the team's row", got.Tier)
		}
		editor := auth.WithIdentity(context.Background(), seedTeamMember(t, f.s, context.Background(), "ed", identity.RoleConfigEditor))
		if got := f.launchSource(t, editor, "probe-bot"); got.Tier != store.BotSourceTierTeam {
			t.Fatalf("a bot editor reads tier %q, want the team's row", got.Tier)
		}
	})

	t.Run("a platform override", func(t *testing.T) {
		// What the platform serves every tenant, as the baked catalog is:
		// every authenticated caller reads it
		// (TestAPlatformOverrideIsEveryCallersAndATeamRowItsEditors).
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, botsource.PlatformTenantID, "probe-bot", launchSourceBot("PLATFORM"))
		for name, ctx := range map[string]context.Context{"a member": f.member, "a viewer": f.viewer, "a team admin": f.admin, "a platform admin": f.superAdmin} {
			if got := f.launchSource(t, ctx, "probe-bot"); got.Tier != store.BotSourceTierPlatform || !strings.Contains(got.Source, "PLATFORM") {
				t.Fatalf("%s reads (tier %q, source %.60q), want the platform override", name, got.Tier, got.Source)
			}
		}
	})

	t.Run("the baked catalog", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		if got := f.launchSource(t, f.viewer, "probe-bot"); got.Tier != store.BotSourceTierBaked {
			t.Fatalf("a viewer reads tier %q, want the baked bundle", got.Tier)
		}
	})

	// The tier is decided before anything of it is read: a caller who may
	// not read a team row learns no diagnostic drawn from it either. A row
	// that does not parse is the probe — its refusal quotes the file.
	t.Run("a refusal is decided before the row is read", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, "t1", "broken", "workflow SECRET_TOKEN_IN_THE_ROW\n  this does not parse\n")
		w := f.readLaunchSource(f.member, "broken")
		if w.Code != http.StatusForbidden {
			t.Fatalf("a member reading a team row that does not parse = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		// The control: the caller who may read it is told why it does not
		// launch, by the refusal a launch of it would draw.
		w = f.readLaunchSource(f.admin, "broken")
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "does not resolve to a launchable program") {
			t.Fatalf("a team admin reading a team row that does not parse = %d %s, want 422 naming the refusal", w.Code, w.Body.String())
		}
	})

	t.Run("a name no tier serves", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		if w := f.readLaunchSource(f.admin, "ghost"); w.Code != http.StatusNotFound {
			t.Fatalf("an unknown bot = %d, want 404; body=%s", w.Code, w.Body.String())
		}
	})
}

// A marker no reader of the refused tier may meet, in a fragment that parses.
const secretFragment = "schema shared_state:\n  ok: bool\n  fail_log: string\n\nprompt leaked_prompt:\n  TIER_SECRET_MARKER do not show this to non readers\n"

// seedCatalogBundle adds a bundle to a catalog root of its own.
func (f *launchSourceFixture) seedCatalogBundle(t *testing.T, slug string, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, slug, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.s.cfg.Bots.Paths = append(f.s.cfg.Bots.Paths, root)
}

// The answer carries more than the bot's own tier: in cloud the freeze
// resolves the bundles the bot builds on — a shared fragment's producer —
// through the team and platform tiers, and the opened document merges their
// text. Every tier whose bytes the answer would carry passes the same rule,
// or the read is refused naming the producer, before anything of it is read
// or diagnosed (rva round 1, V1).
func TestTheLaunchSourceCarriesNoFragmentOfATierItsCallerMayNotRead(t *testing.T) {
	consumer := map[string]string{bundle.ManifestFile: fragmentConsumerManifest, "main.bot": fragmentConsumerMain}
	producer := func(fragment string) map[string]string {
		return map[string]string{bundle.ManifestFile: fragmentProducerManifest, "main.bot": testBotMain, "lib/forge-auth.bot": fragment}
	}

	// The refused caller reads the bot named — the baked bundle is everyone's —
	// and not the TEAM's bundle under it: a member, who edits no bot of the
	// team. The team's bot editors read both (the control of each case). A
	// platform bundle under it refuses nobody
	// (TestAPlatformOverrideIsEveryCallersAndATeamRowItsEditors).
	t.Run("a baked consumer over a team fragment, read by a member", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, "t1", "producer", producer(secretFragment))
		f.seedCatalogBundle(t, "consumer", consumer)
		if w := f.readLaunchSource(f.member, "producer"); w.Code != http.StatusForbidden {
			t.Fatalf("premise: a member reads the team producer directly = %d, want 403", w.Code)
		}
		w := f.readLaunchSource(f.member, "consumer")
		if w.Code != http.StatusForbidden {
			t.Fatalf("a member reading a baked bot built on a team fragment = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "TIER_SECRET_MARKER") {
			t.Fatalf("the refusal carries the fragment: %s", w.Body.String())
		}
		if want := `\"consumer\" builds on another bundle whose program you may not read — \"producer\" is your team's own bot`; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("the refusal = %s, want it to name the producer: %s", w.Body.String(), want)
		}
		// The control: the caller who may read both reads the merged program.
		w = f.readLaunchSource(f.admin, "consumer")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "TIER_SECRET_MARKER") {
			t.Fatalf("a team admin reading the same bot = %d, fragment merged = %v; want 200 with the fragment", w.Code, strings.Contains(w.Body.String(), "TIER_SECRET_MARKER"))
		}
	})

	// The guard stands at the one door every bundle of a freeze comes
	// through, so it refuses on a child and on a skill's producer too — whose
	// text the answer does not carry (rva round 2, V3).
	t.Run("a baked parent over a team child, read by a member", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, "t1", "kid", map[string]string{bundle.ManifestFile: "name: kid\n", "main.bot": "## KID_TEXT_MARKER\n" + testBotMain})
		f.seedCatalogBundle(t, "parent", map[string]string{bundle.ManifestFile: "name: parent\n", "main.bot": snapshotParent("kid")})
		w := f.readLaunchSource(f.member, "parent")
		if want := `\"parent\" builds on another bundle whose program you may not read — \"kid\" is your team's own bot`; w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("a member reading a baked bot whose child is the team's own = %d %s, want 403 naming the child", w.Code, w.Body.String())
		}
		w = f.readLaunchSource(f.admin, "parent")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "KID_TEXT_MARKER") {
			t.Fatalf("a team admin reading the same bot = %d, child text carried = %v; want 200 without it", w.Code, strings.Contains(w.Body.String(), "KID_TEXT_MARKER"))
		}
	})

	t.Run("a baked consumer of a team skill, read by a member", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, "t1", "producer", map[string]string{bundle.ManifestFile: skillProducerManifest, "main.bot": testBotMain, "skills/verify-build.md": "---\nname: verify-build\ndescription: build it\n---\n\nSKILL_TEXT_MARKER\n"})
		f.seedCatalogBundle(t, "consumer", map[string]string{bundle.ManifestFile: skillConsumerManifest, "main.bot": testBotMain})
		w := f.readLaunchSource(f.member, "consumer")
		if want := `\"consumer\" builds on another bundle whose program you may not read — \"producer\" is your team's own bot`; w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("a member reading a baked bot whose skill comes from the team's own bundle = %d %s, want 403 naming the producer", w.Code, w.Body.String())
		}
		w = f.readLaunchSource(f.admin, "consumer")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "SKILL_TEXT_MARKER") {
			t.Fatalf("a team admin reading the same bot = %d, skill text carried = %v; want 200 without it", w.Code, strings.Contains(w.Body.String(), "SKILL_TEXT_MARKER"))
		}
	})

	t.Run("a baked consumer over a team producer, read by a viewer", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.seedCatalogBundle(t, "consumer", consumer)
		f.storeBundle(t, "t1", "producer", producer(secretFragment))
		if w := f.readLaunchSource(f.viewer, "producer"); w.Code != http.StatusForbidden {
			t.Fatalf("premise: a viewer reads the team producer directly = %d, want 403", w.Code)
		}
		w := f.readLaunchSource(f.viewer, "consumer")
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "TIER_SECRET_MARKER") {
			t.Fatalf("a viewer reading a baked bot built on a team fragment = %d %s, want 403 without the fragment", w.Code, w.Body.String())
		}
		if want := `\"producer\" is your team's own bot`; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("the refusal = %s, want it to name the producer: %s", w.Body.String(), want)
		}
		w = f.readLaunchSource(f.admin, "consumer")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "TIER_SECRET_MARKER") {
			t.Fatalf("a team admin reading the same bot = %d, fragment merged = %v; want 200 with the fragment", w.Code, strings.Contains(w.Body.String(), "TIER_SECRET_MARKER"))
		}
	})

	// The 422 arm: a producer fragment that does not parse draws a
	// diagnostic of its own text. It is refused before it is diagnosed.
	t.Run("a broken team fragment is refused, never diagnosed, to a viewer", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.seedCatalogBundle(t, "consumer", consumer)
		f.storeBundle(t, "t1", "producer", producer("schema BROKEN_SECRET_LINE oops oops\n  ???\n"))
		w := f.readLaunchSource(f.viewer, "consumer")
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "forge-auth.bot") {
			t.Fatalf("a viewer reading a baked bot over a broken team fragment = %d %s, want 403 and no diagnostic of the fragment", w.Code, w.Body.String())
		}
	})

	// The launch itself reads every tier, as it always has: the guard rides
	// a read, and a pin — not a launch by id.
	t.Run("the launch by id is not guarded", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, botsource.PlatformTenantID, "producer", producer(secretFragment))
		f.storeBundle(t, "t1", "consumer", consumer)
		rec := postLaunch(t, f.s, f.member, `{"bot_id":"consumer"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("a member launching the team bot by id = %d, want 202; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// A bot in several files is answered as its unit, read from the launch's own
// materialization: the merged document holds the fragment's declarations,
// nothing of the answer names a server path — and the digest is the identity
// of the whole program, so a fragment edited under the form refuses the
// launch as an edit of the entry does.
func TestTheLaunchSourceOfABotInSeveralFilesIsItsUnit(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	main := "import \"lib/nodes.bot\"\n\nworkflow unit_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
	lib := func(mark string) string {
		return "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"" + mark + "\"}'`\n  output: probe_out\n"
	}
	f.storeBundle(t, "t1", "united", map[string]string{botsource.MainBotFile: main, "lib/nodes.bot": lib("LIB")})

	w := f.readLaunchSource(f.admin, "united")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got botLaunchSourceAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != main {
		t.Fatalf("source = %q, want the entry file", got.Source)
	}
	if got.Unit == nil || got.Unit.Root != "" || got.Unit.LoadErrors != 0 || len(got.Unit.Files) != 2 {
		t.Fatalf("unit = %+v, want the two files, loading, rooted nowhere", got.Unit)
	}
	if !strings.Contains(string(got.Document), "noop") {
		t.Fatalf("document = %s, want the fragment's tool merged in", got.Document)
	}
	if strings.Contains(w.Body.String(), os.TempDir()) {
		t.Fatalf("the answer names a server path: %s", w.Body.String())
	}
	if got.SHA256 == contentSHA256(main) {
		t.Fatalf("sha256 is the entry file's digest alone: the fragment the document shows is outside the pin")
	}

	// The entry is untouched; a fragment changes under the form.
	f.storeBundle(t, "t1", "united", map[string]string{botsource.MainBotFile: main, "lib/nodes.bot": lib("LIB-EDITED")})
	rec := postLaunch(t, f.s, f.admin, pinnedLaunch(got.BotID, got.SHA256))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
		t.Fatalf("the launch pinned before the fragment's edit = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
	}
	if n := len(f.cap.all()); n != 0 {
		t.Fatalf("the refused launch published %d programs, want 0", n)
	}
}

// The presets a form offers are the launch's own: the program's `presets:`
// block overlaid by the bundle's presets/<name>.md, read from the
// materialization the digest covers — so a preset edited under the form
// refuses the launch instead of applying values the form never showed
// (rva round 1, S1: the registry listing is a session-old snapshot).
func TestTheLaunchSourceAnswersThePresetsTheLaunchApplies(t *testing.T) {
	main := "vars:\n  target: string = \"default\"\n  depth: string = \"shallow\"\n\npresets:\n  quick:\n    target: \"in-source-quick\"\n  prod:\n    target: \"in-source-prod\"\n\nschema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"x\"}'`\n  output: probe_out\n\nworkflow preset_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
	filePreset := func(target string) string {
		return "---\ndisplay_name: Production\nvars:\n  target: " + target + "\n  depth: deep\n---\nFocus on production.\n"
	}
	valuesOf := func(t *testing.T, presets *botregistry.PresetsBlock, name string) map[string]string {
		t.Helper()
		if presets == nil {
			t.Fatalf("the answer carries no presets")
		}
		for _, p := range presets.Entries {
			if p.Name != name {
				continue
			}
			out := map[string]string{}
			for _, v := range p.Values {
				out[v.Key] = v.Value.StrVal
			}
			return out
		}
		t.Fatalf("no preset %q among %d", name, len(presets.Entries))
		return nil
	}

	for _, mode := range []string{"local", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaunchSourceFixture(t, mode)
			f.storeBundle(t, "t1", "presetted", map[string]string{botsource.MainBotFile: main, "presets/prod.md": filePreset("file-prod-v1")})
			got := f.launchSource(t, f.admin, "presetted")

			// The in-source preset, and the file preset winning on its name.
			if quick := valuesOf(t, got.Presets, "quick"); quick["target"] != "in-source-quick" {
				t.Fatalf("preset quick = %v, want the program's own", quick)
			}
			if prod := valuesOf(t, got.Presets, "prod"); prod["target"] != "file-prod-v1" || prod["depth"] != "deep" {
				t.Fatalf("preset prod = %v, want the bundle's presets/prod.md over the in-source entry", prod)
			}

			if mode != "cloud" {
				return // the pin below is the cloud lane's
			}
			// The preset file is edited under the form; the entry is not.
			f.storeBundle(t, "t1", "presetted", map[string]string{botsource.MainBotFile: main, "presets/prod.md": filePreset("file-prod-v2")})
			body, _ := json.Marshal(map[string]string{"bot_id": got.BotID, "expect_source_sha256": got.SHA256, "preset": "prod"})
			rec := postLaunch(t, f.s, f.admin, string(body))
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
				t.Fatalf("the launch pinned before the preset's edit = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
			}

			fresh := f.launchSource(t, f.admin, "presetted")
			if prod := valuesOf(t, fresh.Presets, "prod"); prod["target"] != "file-prod-v2" {
				t.Fatalf("the fresh read's preset prod = %v, want the edited file", prod)
			}
			body, _ = json.Marshal(map[string]string{"bot_id": fresh.BotID, "expect_source_sha256": fresh.SHA256, "preset": "prod"})
			if rec := postLaunch(t, f.s, f.admin, string(body)); rec.Code != http.StatusAccepted {
				t.Fatalf("the launch of the fresh read = %d, want 202; body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("a bot with no preset answers none", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		if got := f.launchSource(t, f.member, "probe-bot"); got.Presets != nil {
			t.Fatalf("presets = %+v, want none", got.Presets)
		}
	})
}

// A pinned launch whose bot changed since the form read it is refused by
// code, publishes nothing, and hands its admission back — the launch that
// follows a fresh read is the one that runs, and counts once.
func TestAStalePinRefusesTheLaunchAndHandsTheAdmissionBack(t *testing.T) {
	for _, mode := range []string{"cloud"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaunchSourceFixture(t, mode)
			f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
			read := f.launchSource(t, f.admin, "probe-bot")

			f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM-EDITED"))

			rec := postLaunch(t, f.s, f.admin, pinnedLaunch(read.BotID, read.SHA256))
			if rec.Code != http.StatusConflict {
				t.Fatalf("the stale launch = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			var refusal struct {
				Error     string `json:"error"`
				ErrorCode string `json:"error_code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal: %v (%s)", err, rec.Body.String())
			}
			// The literal, not the constant: the studio reads this very word.
			if refusal.ErrorCode != "bot_source_changed" {
				t.Fatalf("error_code = %q, want \"bot_source_changed\"; body=%s", refusal.ErrorCode, rec.Body.String())
			}
			if !strings.Contains(refusal.Error, `"probe-bot"`) || !strings.Contains(refusal.Error, "the team tier") || !strings.Contains(refusal.Error, "reload the form") {
				t.Fatalf("error = %q, want it to name the bot, its tier and the remedy", refusal.Error)
			}
			if n := len(f.cap.all()); n != 0 {
				t.Fatalf("the refused launch published %d programs, want 0", n)
			}
			if got := f.meteredRuns(t); got != 0 {
				t.Fatalf("monthly runs = %d after a refused launch, want 0 — a stale form would otherwise spend the org's month", got)
			}

			// The remedy the refusal names: read again, launch what it answers.
			fresh := f.launchSource(t, f.admin, "probe-bot")
			if fresh.SHA256 == read.SHA256 {
				t.Fatalf("the fresh read issued the stale digest")
			}
			rec = postLaunch(t, f.s, f.admin, pinnedLaunch(fresh.BotID, fresh.SHA256))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("the launch of the fresh read = %d, want 202; body=%s", rec.Code, rec.Body.String())
			}
			if ran := f.cap.only(t); ran.hash != fresh.SHA256 || !strings.Contains(ran.main, "TEAM-EDITED") {
				t.Fatalf("the launch compiled (identity %q, entry %q), the fresh read issued %q", ran.hash, ran.main, fresh.SHA256)
			}
			if got := f.meteredRuns(t); got != 1 {
				t.Fatalf("monthly runs = %d after one accepted launch, want 1", got)
			}
		})
	}
}

// The pin also catches a change of TIER when the tier that now serves the
// name serves ANOTHER PROGRAM: a team row of another text, appearing under
// the name after the form read the baked bundle. The tier is not itself part
// of the identity — a fork of the same program is the same program
// (TestAPinCoversTheStatedPartsOfTheProgramAndNoOther's last rows).
func TestAStalePinCatchesAnotherTierTakingTheName(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	read := f.launchSource(t, f.admin, "probe-bot")
	if read.Tier != store.BotSourceTierBaked {
		t.Fatalf("tier = %q, want the baked bundle before the fork", read.Tier)
	}
	f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
	rec := postLaunch(t, f.s, f.admin, pinnedLaunch(read.BotID, read.SHA256))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
		t.Fatalf("the launch pinned to the baked program = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
	}
	if n := len(f.cap.all()); n != 0 {
		t.Fatalf("the refused launch published %d programs, want 0", n)
	}
}

// A pin is the cloud lane's guarantee. Outside cloud the launch reads the
// bot's files again after it is accepted — the in-process engine merges the
// presets from disk, a detached runner compiles its own copy, a parked launch
// compiles when its slot frees — so a pin compared there would be outrun
// (rva round 2, V1: 69 of 150 accepted pinned runs applied a preset swapped
// after the pin). It is refused by name, right or wrong, before the gate is
// asked for a slot; the launch by id stays the launch.
func TestAPinIsRefusedByNameWhereNoFrozenProgramRuns(t *testing.T) {
	f := newLaunchSourceFixture(t, "local")
	read := f.launchSource(t, f.member, "probe-bot")
	for name, pin := range map[string]string{"the right pin": read.SHA256, "a stale pin": strings.Repeat("0", 64)} {
		rec := postLaunch(t, f.s, f.member, pinnedLaunch("probe-bot", pin))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"launch_pin_unsupported"`) || !strings.Contains(rec.Body.String(), "the cloud lane") {
			t.Fatalf("%s outside cloud = %d %s, want the 400 launch_pin_unsupported that names the cloud lane", name, rec.Code, rec.Body.String())
		}
	}
	if n := len(f.cap.all()); n != 0 {
		t.Fatalf("the refused launches published %d programs, want 0", n)
	}
	if got := f.meteredRuns(t); got != 0 {
		t.Fatalf("monthly runs = %d after refused launches, want 0", got)
	}
	// The control: the same launch with no pin runs, and compiles the
	// identity the read answered.
	if rec := postLaunch(t, f.s, f.member, `{"bot_id":"probe-bot"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("the launch by id = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if ran := f.cap.only(t); ran.hash != read.SHA256 {
		t.Fatalf("the launch by id compiled identity %q, the read answered %q", ran.hash, read.SHA256)
	}

	// The request alone decides it, so it is said before the gate: a
	// suspended team's well-formed launch is the gate's 403, its pinned one
	// the payload's 400.
	t.Run("before the gate", func(t *testing.T) {
		g := &launchSourceFixture{cap: &programCapture{}, counter: orgusage.NewMemoryCounter()}
		g.s = newOrgTestServer(t)
		suspended := seedGate(t, g.s, gateSpec{id: "t1", teamStatus: identity.TeamStatusSuspended})
		g.s.cfg.Mode = "local"
		g.s.orgUsage = g.counter
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		g.s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(g.cap))
		if rec := postLaunch(t, g.s, suspended, `{"bot_id":"probe-bot"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("premise: a launch on a suspended team = %d, want the gate's 403; body=%s", rec.Code, rec.Body.String())
		}
		rec := postLaunch(t, g.s, suspended, pinnedLaunch("probe-bot", read.SHA256))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"launch_pin_unsupported"`) {
			t.Fatalf("a pin outside cloud on a suspended team = %d %s, want the payload's 400, not the gate's refusal", rec.Code, rec.Body.String())
		}
	})
}

// The lane decision is the run service's own: a cloud server whose service
// publishes nothing cannot hold a pin either, and the handler says so in the
// same words — after the gate this time, which gets its slot back.
func TestAPinIsRefusedByARunServiceThatPublishesNothing(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	read := f.launchSource(t, f.member, "probe-bot")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
	rec := postLaunch(t, f.s, f.member, pinnedLaunch("probe-bot", read.SHA256))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"launch_pin_unsupported"`) || !strings.Contains(rec.Body.String(), "the cloud lane") {
		t.Fatalf("a pinned launch on a service with no publisher = %d %s, want the 400 launch_pin_unsupported", rec.Code, rec.Body.String())
	}
	if got := f.meteredRuns(t); got != 0 {
		t.Fatalf("monthly runs = %d after the refused launch, want 0 — the slot goes back", got)
	}
}

// pinTier stores a bundle on one tier and names the least-privileged caller
// who may read that tier.
type pinTier struct {
	name   string
	put    func(t *testing.T, f *launchSourceFixture, slug string, files map[string]string)
	reader func(f *launchSourceFixture) context.Context
}

var pinTiers = []pinTier{
	{"team", func(t *testing.T, f *launchSourceFixture, slug string, files map[string]string) {
		f.storeBundle(t, "t1", slug, files)
	}, func(f *launchSourceFixture) context.Context { return f.admin }},
	{"platform", func(t *testing.T, f *launchSourceFixture, slug string, files map[string]string) {
		f.storeBundle(t, botsource.PlatformTenantID, slug, files)
	}, func(f *launchSourceFixture) context.Context { return f.superAdmin }},
	{"baked", func(t *testing.T, f *launchSourceFixture, slug string, files map[string]string) {
		t.Helper()
		for rel, body := range files {
			path := filepath.Join(f.s.cfg.Bots.Paths[0], slug, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}, func(f *launchSourceFixture) context.Context { return f.member }},
}

const pinnedAgentMain = "import \"lib/nodes.bot\"\n\nschema out:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: out\n  system: helper\n\nworkflow w:\n  worktree: none\n  entry: a\n  a -> done\n"

// pinnedBundle is a bot of every kind of file a bundle holds, each carrying v.
func pinnedBundle(v string) map[string]string {
	return map[string]string{
		bundle.ManifestFile:    "name: whole\nversion: 1.0.0\n",
		"main.bot":             pinnedAgentMain,
		"lib/nodes.bot":        "prompt libp:\n  LIB-" + v + "\n",
		"prompts/helper.md":    "Helper.\n{{include \"shared/r.txt\"}}\n",
		"prompts/shared/r.txt": "INCLUDED-" + v,
		"presets/prod.md":      "---\nvars: {}\n---\nfocus " + v + "\n",
		"skills/own.md":        "---\nname: own\ndescription: d\n---\nSKILL-" + v + "\n",
		"scripts/x.sh":         "echo " + v + "\n",
	}
}

const pinnedDepConsumerManifest = "name: depends\nversion: 1.0.0\ndependencies:\n  fragments:\n    - name: forge-auth\n      from: producer\n  skills:\n    - name: verify-build\n      from: producer\n"
const pinnedDepProducerManifest = "name: producer\nversion: 1.0.0\nexports:\n  fragments:\n    - id: forge-auth\n      path: lib/forge-auth.bot\n  skills:\n    - id: verify-build\n      path: skills/verify-build.md\n"

func pinnedProducer(fragment, skill string) map[string]string {
	return map[string]string{
		bundle.ManifestFile:      pinnedDepProducerManifest,
		"main.bot":               testBotMain,
		"lib/forge-auth.bot":     strings.Replace(secretFragment, "TIER_SECRET_MARKER", "FRAG-"+fragment, 1),
		"skills/verify-build.md": strings.Replace(sharedSkillBody, "Run the build", "Run the build "+skill, 1),
	}
}

func pinnedChild(v string) map[string]string {
	return map[string]string{bundle.ManifestFile: "name: kid\n", "main.bot": strings.Replace(testBotMain, "printf ok", "printf ok"+v, 1)}
}

// What a pin covers is a stated list (docs/cloud-rest-api.md, "The pinned
// launch"), witnessed here part by part on the cloud lane and on every tier:
// a part is edited between the read and the pinned launch, and the launch is
// refused — or, for the parts the program's identity does not carry, runs.
// The second half is as much the contract as the first: a pin that claimed a
// child's program or a skill would be believed and not kept (rva round 2).
func TestAPinCoversTheStatedPartsOfTheProgramAndNoOther(t *testing.T) {
	edit := func(rel string) func(t *testing.T, f *launchSourceFixture, tier pinTier) {
		return func(t *testing.T, f *launchSourceFixture, tier pinTier) {
			files := pinnedBundle("V1")
			files[rel] = pinnedBundle("V2")[rel]
			switch rel {
			case "main.bot":
				files[rel] = strings.Replace(pinnedAgentMain, "model: \"m\"", "model: \"m2\"", 1)
			case "prompts/helper.md":
				files[rel] = "Helper V2.\n{{include \"shared/r.txt\"}}\n"
			case bundle.ManifestFile:
				files[rel] = "name: whole\nversion: 1.0.1\n"
			}
			tier.put(t, f, "whole", files)
		}
	}
	for _, part := range []struct {
		name    string
		slug    string
		covered bool
		edit    func(t *testing.T, f *launchSourceFixture, tier pinTier)
	}{
		{"the entry", "whole", true, edit("main.bot")},
		{"a fragment of the bundle", "whole", true, edit("lib/nodes.bot")},
		{"a bundle prompt", "whole", true, edit("prompts/helper.md")},
		{"an included file", "whole", true, edit("prompts/shared/r.txt")},
		{"a preset file", "whole", true, edit("presets/prod.md")},
		{"a shared fragment of a producer", "depends", true, func(t *testing.T, f *launchSourceFixture, tier pinTier) {
			tier.put(t, f, "producer", pinnedProducer("V2", "V1"))
		}},
		{"the bundle's own skill", "whole", false, edit("skills/own.md")},
		{"another file of the bundle", "whole", false, edit("scripts/x.sh")},
		{"the manifest", "whole", false, edit(bundle.ManifestFile)},
		{"a shared skill of a producer", "depends", false, func(t *testing.T, f *launchSourceFixture, tier pinTier) {
			tier.put(t, f, "producer", pinnedProducer("V1", "V2"))
		}},
		{"a child bot's program", "parent", false, func(t *testing.T, f *launchSourceFixture, tier pinTier) {
			tier.put(t, f, "kid", pinnedChild("V2"))
		}},
	} {
		for _, tier := range pinTiers {
			t.Run(part.name+"/"+tier.name, func(t *testing.T) {
				f := newLaunchSourceFixture(t, "cloud")
				tier.put(t, f, "whole", pinnedBundle("V1"))
				tier.put(t, f, "depends", map[string]string{bundle.ManifestFile: pinnedDepConsumerManifest, "main.bot": fragmentConsumerMain})
				tier.put(t, f, "producer", pinnedProducer("V1", "V1"))
				tier.put(t, f, "parent", map[string]string{bundle.ManifestFile: "name: parent\n", "main.bot": snapshotParent("kid")})
				tier.put(t, f, "kid", pinnedChild("V1"))
				ctx := tier.reader(f)
				read := f.launchSource(t, ctx, part.slug)

				// Premise: untouched, the pinned launch runs the pinned program.
				if rec := postLaunch(t, f.s, ctx, pinnedLaunch(part.slug, read.SHA256)); rec.Code != http.StatusAccepted {
					t.Fatalf("premise: the pinned launch of the untouched bot = %d, want 202; body=%s", rec.Code, rec.Body.String())
				}
				if ran := f.cap.only(t); ran.hash != read.SHA256 {
					t.Fatalf("premise: the launch compiled identity %q, the read answered %q", ran.hash, read.SHA256)
				}

				part.edit(t, f, tier)
				rec := postLaunch(t, f.s, ctx, pinnedLaunch(part.slug, read.SHA256))
				moved := f.launchSource(t, ctx, part.slug).SHA256 != read.SHA256
				if part.covered {
					if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
						t.Fatalf("a launch pinned before the edit = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
					}
					if !moved {
						t.Fatal("the edit is refused by the launch, and a fresh read answers the old identity")
					}
					if n := len(f.cap.all()); n != 1 {
						t.Fatalf("%d programs published, want the premise's alone", n)
					}
					return
				}
				if rec.Code != http.StatusAccepted || moved {
					t.Fatalf("a part outside the program's identity: the pinned launch = %d (want 202), a fresh read's identity moved = %v (want false); body=%s", rec.Code, moved, rec.Body.String())
				}
			})
		}
	}

	// "Another tier serving the name" is covered as far as its PROGRAM
	// differs, and no further: the baked bundle is read, then the team forks
	// it. The identity carries no tier.
	for _, fork := range []struct {
		name    string
		covered bool
		files   func() map[string]string
	}{
		{"another tier serving the name with another program", true, func() map[string]string {
			files := pinnedBundle("V1")
			files["main.bot"] = strings.Replace(pinnedAgentMain, "model: \"m\"", "model: \"m2\"", 1)
			return files
		}},
		{"another tier serving the name with the same program, other skills and scripts", false, func() map[string]string {
			files := pinnedBundle("V1")
			files["skills/own.md"] = pinnedBundle("V2")["skills/own.md"]
			files["scripts/x.sh"] = pinnedBundle("V2")["scripts/x.sh"]
			return files
		}},
	} {
		t.Run(fork.name, func(t *testing.T) {
			f := newLaunchSourceFixture(t, "cloud")
			baked := pinTiers[2]
			baked.put(t, f, "whole", pinnedBundle("V1"))
			read := f.launchSource(t, f.admin, "whole")
			if read.Tier != store.BotSourceTierBaked {
				t.Fatalf("premise: tier = %q, want the baked bundle before the fork", read.Tier)
			}
			f.storeBundle(t, "t1", "whole", fork.files())
			rec := postLaunch(t, f.s, f.admin, pinnedLaunch("whole", read.SHA256))
			if fork.covered {
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
					t.Fatalf("a launch pinned before a fork of another program = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusAccepted {
				t.Fatalf("a launch pinned before a fork of the same program = %d, want 202; body=%s", rec.Code, rec.Body.String())
			}
			// It runs the team's row — the pin held the program, not the tier.
			if ran := f.cap.only(t); ran.hash != read.SHA256 || ran.spec.BotSourceTier != store.BotSourceTierTeam {
				t.Fatalf("the launch compiled identity %q on tier %q, want the pinned identity %q served by the team tier", ran.hash, ran.spec.BotSourceTier, read.SHA256)
			}
		})
	}
}

// The cloud lane holds a pin because the launch publishes the frozen
// collection its compile read: nothing is read again. A team row whose preset
// file flips under a stream of pinned launches is the swap that outran the
// local lanes — here every accepted launch compiled the pinned identity and
// published one and the same frozen collection, the pinned one.
func TestAPinnedCloudLaunchPublishesThePinnedFrozenProgram(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	files := func(v string) map[string]string {
		b := pinnedBundle("V1")
		b["presets/prod.md"] = pinnedBundle(v)["presets/prod.md"]
		return b
	}
	f.storeBundle(t, "t1", "whole", files("V1"))
	read := f.launchSource(t, f.admin, "whole")
	if rec := postLaunch(t, f.s, f.admin, pinnedLaunch("whole", read.SHA256)); rec.Code != http.StatusAccepted {
		t.Fatalf("premise: the pinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	reference := f.cap.only(t)
	if reference.spec.BotBundle == nil || reference.spec.BotBundle.SnapshotDigest == "" {
		t.Fatalf("premise: the launch published no frozen collection: %+v", reference.spec.BotBundle)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := store.WithTenant(context.Background(), "t1")
		for i := 0; !stop.Load(); i++ {
			row, err := f.s.botSources.GetBySlug(ctx, "t1", "whole")
			if err != nil {
				continue
			}
			row.Files = files([]string{"V2", "V1"}[i%2])
			_, _ = f.s.botSources.Update(ctx, row)
		}
	}()
	codes := map[int]int{}
	deadline := time.Now().Add(30 * time.Second)
	for (codes[http.StatusAccepted] < 20 || codes[http.StatusConflict] < 20) && time.Now().Before(deadline) {
		codes[postLaunch(t, f.s, f.admin, pinnedLaunch("whole", read.SHA256)).Code]++
	}
	stop.Store(true)
	wg.Wait()

	launches := f.cap.all()[1:]
	for _, l := range launches {
		if l.hash != read.SHA256 {
			t.Fatalf("an accepted launch pinned to %q compiled identity %q; statuses %v", read.SHA256, l.hash, codes)
		}
		if l.spec.BotBundle == nil || l.spec.BotBundle.SnapshotDigest != reference.spec.BotBundle.SnapshotDigest {
			t.Fatalf("an accepted launch published another frozen collection than the pinned one: %+v, want digest %q; statuses %v", l.spec.BotBundle, reference.spec.BotBundle.SnapshotDigest, codes)
		}
	}
	// The witness must have seen both outcomes to say anything.
	if codes[http.StatusAccepted] == 0 || codes[http.StatusConflict] == 0 {
		t.Fatalf("statuses = %v: the swap never crossed a launch, so nothing was witnessed", codes)
	}
	if codes[http.StatusAccepted] != len(launches) {
		t.Fatalf("%d launches answered 202, %d were published", codes[http.StatusAccepted], len(launches))
	}
}

// botTierOutageStore is a bot-source store whose read of one slug fails with
// something other than an absence — on every tenant, or on one.
type botTierOutageStore struct {
	botsource.Store
	failSlug   string
	failTenant string
}

// storeOutageText is what the store says of its outage: an address a tenant
// has no business reading.
const storeOutageText = "server selection error: Addr: db-internal-7:27017, dial tcp 10.0.0.7:27017: connection refused"

func (s botTierOutageStore) GetBySlug(ctx context.Context, tenant, slug string) (botsource.BotSource, error) {
	if slug == s.failSlug && (s.failTenant == "" || tenant == s.failTenant) {
		return botsource.BotSource{}, errors.New(storeOutageText)
	}
	return s.Store.GetBySlug(ctx, tenant, slug)
}

// tierOutages are the stored tiers that can fail under a bot: its own team
// row, the platform's override of its name, and the row of a bundle it
// builds on.
var tierOutages = []struct{ name, slug, tenant string }{
	{"the bot's own team tier", "consumer", "t1"},
	{"the platform tier of its name", "consumer", botsource.PlatformTenantID},
	{"the tier of a bundle it builds on", "producer", ""},
}

func newTierOutageFixture(t *testing.T) *launchSourceFixture {
	t.Helper()
	f := newLaunchSourceFixture(t, "cloud")
	f.storeBundle(t, "t1", "producer", map[string]string{bundle.ManifestFile: fragmentProducerManifest, "main.bot": testBotMain, "lib/forge-auth.bot": secretFragment})
	f.storeBundle(t, "t1", "consumer", map[string]string{bundle.ManifestFile: fragmentConsumerManifest, "main.bot": fragmentConsumerMain})
	return f
}

// A stored tier that does not answer is transient, and said so: 503, whether
// it is the tier of the bot asked for — the team's, or the platform's, which
// is asked even when the team does not hold the name — or of a bundle it
// builds on. Never the 500 of a server that failed (rva round 2, V4).
func TestTheLaunchSourceAnswers503WhenAStoredTierDoesNotAnswer(t *testing.T) {
	for _, down := range tierOutages {
		t.Run(down.name, func(t *testing.T) {
			f := newTierOutageFixture(t)
			name := "consumer"
			if down.tenant == botsource.PlatformTenantID {
				// The platform tier is asked when the team does not hold the name.
				name = "probe-bot"
			}
			if w := f.readLaunchSource(f.admin, name); w.Code != http.StatusOK {
				t.Fatalf("premise: the read with every tier up = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			slug := down.slug
			if down.tenant == botsource.PlatformTenantID {
				slug = name
			}
			f.s.botSources = botTierOutageStore{Store: f.s.botSources, failSlug: slug, failTenant: down.tenant}
			w := f.readLaunchSource(f.admin, name)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("the read while %s does not answer = %d, want 503; body=%s", down.name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "10.0.0.7") || strings.Contains(w.Body.String(), "db-internal-7") {
				t.Fatalf("the 503 carries the store's address: %s", w.Body.String())
			}
		})
	}
}

// The same outage under a LAUNCH: the resolution is the launch's own, its
// 400 "resolve bot: …" would hand the caller the store's words — a host and
// an address — and say the request was wrong when the server was unavailable
// (rva round 3). It is a masked 503, and the admission goes back.
func TestALaunchAnswersAMasked503WhenAStoredTierDoesNotAnswer(t *testing.T) {
	for _, down := range tierOutages {
		t.Run(down.name, func(t *testing.T) {
			f := newTierOutageFixture(t)
			name := "consumer"
			slug := down.slug
			if down.tenant == botsource.PlatformTenantID {
				name, slug = "probe-bot", "probe-bot"
			}
			f.s.botSources = botTierOutageStore{Store: f.s.botSources, failSlug: slug, failTenant: down.tenant}
			rec := postLaunch(t, f.s, f.admin, `{"bot_id":"`+name+`"}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("the launch while %s does not answer = %d, want 503; body=%s", down.name, rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); strings.Contains(body, "10.0.0.7") || strings.Contains(body, "db-internal-7") || strings.Contains(body, "connection refused") {
				t.Fatalf("the refusal carries the store's words: %s", body)
			}
			if n := len(f.cap.all()); n != 0 {
				t.Fatalf("the refused launch published %d programs, want 0", n)
			}
			if got := f.meteredRuns(t); got != 0 {
				t.Fatalf("monthly runs = %d after the refused launch, want 0 — the slot goes back", got)
			}
		})
	}

	// A child bot is a bundle of the freeze as a fragment's producer is.
	t.Run("the tier of a child it delegates to", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, "t1", "kid", map[string]string{bundle.ManifestFile: "name: kid\n", "main.bot": testBotMain})
		f.storeBundle(t, "t1", "parent", map[string]string{"main.bot": snapshotParent("kid")})
		if rec := postLaunch(t, f.s, f.admin, `{"bot_id":"parent"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("premise: the launch with every tier up = %d, want 202; body=%s", rec.Code, rec.Body.String())
		}
		f.s.botSources = botTierOutageStore{Store: f.s.botSources, failSlug: "kid"}
		rec := postLaunch(t, f.s, f.admin, `{"bot_id":"parent"}`)
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "10.0.0.7") {
			t.Fatalf("the launch while the child's tier does not answer = %d %s, want a masked 503", rec.Code, rec.Body.String())
		}
	})
}

// Only a reader of the program can have been issued a pin for it: a pinned
// launch from a caller who may launch the bot but not read it is refused in
// the read's own words, before anything is compared (rva round 1, V3) — and
// the launch by id, with no pin, is theirs as before. The guard protects the
// text, not the digest: the digest is the workflow_hash every viewer of a run
// of the bot reads on its header.
func TestAPinFromACallerWhoMayNotReadTheBotIsRefused(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	secret := launchSourceBot("PIN-4821")
	f.storeRow(t, "t1", "probe-bot", secret)
	issued := f.launchSource(t, f.admin, "probe-bot").SHA256

	for name, pin := range map[string]string{"the right guess": issued, "a wrong guess": contentSHA256(launchSourceBot("PIN-0000"))} {
		rec := postLaunch(t, f.s, f.member, pinnedLaunch("probe-bot", pin))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s from a member = %d, want 403 either way; body=%s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "expect_source_sha256 is issued by the read of the bot's program") {
			t.Fatalf("%s: refusal = %s, want it to say who is issued a pin", name, rec.Body.String())
		}
	}
	if n := len(f.cap.all()); n != 0 {
		t.Fatalf("the refused launches published %d programs, want 0", n)
	}
	if got := f.meteredRuns(t); got != 0 {
		t.Fatalf("monthly runs = %d after refused launches, want 0", got)
	}

	// The same holds for a bundle the bot builds on: a member reads the baked
	// bot named, and not the team's own fragment under it.
	f.storeBundle(t, "t1", "producer", map[string]string{bundle.ManifestFile: fragmentProducerManifest, "main.bot": testBotMain, "lib/forge-auth.bot": secretFragment})
	f.seedCatalogBundle(t, "consumer", map[string]string{bundle.ManifestFile: fragmentConsumerManifest, "main.bot": fragmentConsumerMain})
	consumerPin := f.launchSource(t, f.admin, "consumer").SHA256
	if rec := postLaunch(t, f.s, f.member, pinnedLaunch("consumer", consumerPin)); rec.Code != http.StatusForbidden {
		t.Fatalf("a member pinning a bot built on a team fragment = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// The controls: the reader's pin launches, and so does the member's
	// launch by id.
	if rec := postLaunch(t, f.s, f.admin, pinnedLaunch("probe-bot", issued)); rec.Code != http.StatusAccepted {
		t.Fatalf("the reader's pinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postLaunch(t, f.s, f.member, `{"bot_id":"probe-bot"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("the member's launch by id = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

// The access rule of the read (#3416): a platform override is what the
// platform serves every tenant, as the baked catalog is, and every
// authenticated caller reads it — named, or under another bot. A narrower
// rule was a boundary in name only (every account owns a personal team, whose
// bots it edits), so there is none: nothing confidential belongs in a
// platform override. A team's OWN row stays its bot editors'. One arm each.
func TestAPlatformOverrideIsEveryCallersAndATeamRowItsEditors(t *testing.T) {
	const overrideText = "OVERRIDE_TEXT_MARKER"
	fragment := func(v string) map[string]string {
		return map[string]string{bundle.ManifestFile: fragmentProducerManifest, "main.bot": testBotMain, "lib/forge-auth.bot": strings.Replace(secretFragment, "TIER_SECRET_MARKER", overrideText+v, 1)}
	}
	consumer := map[string]string{bundle.ManifestFile: fragmentConsumerManifest, "main.bot": fragmentConsumerMain}

	t.Run("a plain member reads a platform override named", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, botsource.PlatformTenantID, "probe-bot", "## "+overrideText+"\n"+launchSourceBot("PLATFORM"))
		read := f.launchSource(t, f.member, "probe-bot")
		if read.Tier != store.BotSourceTierPlatform || !strings.Contains(read.Source, overrideText) {
			t.Fatalf("the member read (tier %q, source %.80q), want the platform override", read.Tier, read.Source)
		}
		if rec := postLaunch(t, f.s, f.member, pinnedLaunch("probe-bot", read.SHA256)); rec.Code != http.StatusAccepted {
			t.Fatalf("the member's pinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a plain member reads it under a baked bot, and the pin covers it", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, botsource.PlatformTenantID, "producer", fragment("-V1"))
		f.seedCatalogBundle(t, "consumer", consumer)
		w := f.readLaunchSource(f.member, "consumer")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), overrideText+"-V1") {
			t.Fatalf("the member's read = %d, override fragment shown = %v; want 200 with it; body=%.300s", w.Code, strings.Contains(w.Body.String(), overrideText), w.Body.String())
		}
		read := f.launchSource(t, f.member, "consumer")
		if rec := postLaunch(t, f.s, f.member, pinnedLaunch("consumer", read.SHA256)); rec.Code != http.StatusAccepted {
			t.Fatalf("the member's pinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
		}
		// The override changes under the form: its text is part of the program.
		f.storeBundle(t, botsource.PlatformTenantID, "producer", fragment("-V2"))
		rec := postLaunch(t, f.s, f.member, pinnedLaunch("consumer", read.SHA256))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"bot_source_changed"`) {
			t.Fatalf("a launch pinned before the override changed = %d %s, want 409 bot_source_changed", rec.Code, rec.Body.String())
		}
	})

	t.Run("an anonymous caller is not one", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, botsource.PlatformTenantID, "probe-bot", "## "+overrideText+"\n"+launchSourceBot("PLATFORM"))
		// Through the server's whole handler, as a request with no credential
		// arrives: the route is behind the authentication every /api route is.
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/bots/probe-bot/launch-source", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("an anonymous read = %d, want 401; body=%.200s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), overrideText) {
			t.Fatalf("the 401 carries the override: %s", w.Body.String())
		}
	})

	t.Run("a plain member is still refused their team's own row", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, "t1", "probe-bot", launchSourceBot("ROW-TEXT-MARKER"))
		w := f.readLaunchSource(f.member, "probe-bot")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `is your team's own bot`) {
			t.Fatalf("a member reading the team's own row = %d %s, want 403 in the team row's sentence", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "ROW-TEXT-MARKER") {
			t.Fatalf("the refusal carries the row's text: %s", w.Body.String())
		}
	})

	t.Run("a team bot over a platform override is refused to them at the bot named, naming nothing under it", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeBundle(t, botsource.PlatformTenantID, "producer", fragment("-V1"))
		f.storeBundle(t, "t1", "consumer", consumer)
		w := f.readLaunchSource(f.member, "consumer")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `\"consumer\" is your team's own bot`) {
			t.Fatalf("a member reading a team bot = %d %s, want 403 on the bot named", w.Code, w.Body.String())
		}
		for _, leak := range []string{"producer", overrideText, "builds on another bundle"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Fatalf("the refusal says %q of what the bot builds on: %s", leak, w.Body.String())
			}
		}
	})

	// Every member reads a platform override — and a platform override can be
	// built on the caller's team's own rows: a child, a fragment's producer, a
	// skill's producer, each resolved in the caller's scope. The guard is
	// asked of every bundle of that freeze, whatever tier the bot named is
	// on: a member who edits no bot of the team is refused, naming the row,
	// on the read and at a pinned launch's door; an editor reads.
	for _, dep := range []struct {
		name     string
		root     map[string]string
		depSlug  string
		depFiles map[string]string
		teamText string // the team row's text, which the refusal must not carry
	}{
		{"a fragment's producer", consumer, "producer", fragment("-TEAM"), overrideText + "-TEAM"},
		{"a child", map[string]string{"main.bot": snapshotParent("kid")}, "kid", map[string]string{bundle.ManifestFile: "name: kid\n", "main.bot": "## TEAM_KID_TEXT\n" + testBotMain}, "TEAM_KID_TEXT"},
		{"a skill's producer", map[string]string{bundle.ManifestFile: skillConsumerManifest, "main.bot": testBotMain}, "producer", map[string]string{bundle.ManifestFile: skillProducerManifest, "main.bot": testBotMain, "skills/verify-build.md": "---\nname: verify-build\ndescription: build it\n---\n\nTEAM_SKILL_TEXT\n"}, "TEAM_SKILL_TEXT"},
	} {
		t.Run("a platform override built on the team's own row — "+dep.name, func(t *testing.T) {
			f := newLaunchSourceFixture(t, "cloud")
			f.storeBundle(t, botsource.PlatformTenantID, "consumer", dep.root)
			f.storeBundle(t, "t1", dep.depSlug, dep.depFiles)
			wantRefusal := `\"consumer\" builds on another bundle whose program you may not read — \"` + dep.depSlug + `\" is your team's own bot`

			w := f.readLaunchSource(f.member, "consumer")
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), wantRefusal) {
				t.Fatalf("a member reading it = %d %s, want 403 naming the team row", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), dep.teamText) {
				t.Fatalf("the refusal carries the team row's text: %s", w.Body.String())
			}
			// An editor of the team reads it — and is issued the pin the
			// member then presents.
			read := f.launchSource(t, f.admin, "consumer")
			if read.Tier != store.BotSourceTierPlatform {
				t.Fatalf("the editor read tier %q, want the platform override", read.Tier)
			}
			rec := postLaunch(t, f.s, f.member, pinnedLaunch("consumer", read.SHA256))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `\"`+dep.depSlug+`\" is your team's own bot`) {
				t.Fatalf("a member's pinned launch of it = %d %s, want 403 naming the team row", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), dep.teamText) {
				t.Fatalf("the refusal carries the team row's text: %s", rec.Body.String())
			}
			if n := len(f.cap.all()); n != 0 {
				t.Fatalf("the refused launch published %d programs, want 0", n)
			}
		})
	}

	t.Run("another team's row is not there for a caller scoped elsewhere", func(t *testing.T) {
		f := newLaunchSourceFixture(t, "cloud")
		f.storeRow(t, "t2", "theirs", launchSourceBot("OTHER-TEAM"))
		for name, ctx := range map[string]context.Context{"a team admin": f.admin, "a platform admin": f.superAdmin} {
			if w := f.readLaunchSource(ctx, "theirs"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "OTHER-TEAM") {
				t.Fatalf("%s of team t1 reading team t2's row = %d %s, want 404", name, w.Code, w.Body.String())
			}
		}
	})
}

// The right to edit a team's bots is the right on the team the request is
// scoped to: an identity that names no team edits none, whatever it carries.
// The authentication chain refuses a credential with no active team before
// either route, so this half of the predicate is asked here directly — it is
// what stands if a route ever lets one through.
func TestEditsScopedTeamBotsTakesATeamScope(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	asked := func(id auth.Identity) bool {
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithIdentity(context.Background(), id))
		return f.s.editsScopedTeamBots(r, id)
	}
	for name, id := range map[string]auth.Identity{
		"a config editor's token scoped to no team": {UserID: "ce", Role: identity.RoleConfigEditor, Credential: auth.CredentialAccessToken},
		"a platform admin scoped to no team":        {UserID: "root", IsSuperAdmin: true, Credential: auth.CredentialAccessToken},
	} {
		if asked(id) {
			t.Fatalf("%s edits the bots of no team: editsScopedTeamBots = true", name)
		}
		// The control: the same identity, scoped to a team, does.
		id.TeamID = "t1"
		if !asked(id) {
			t.Fatalf("%s, once scoped to t1, edits its bots: editsScopedTeamBots = false", name)
		}
	}
}

// A caller the guard refuses is answered the same 403 whatever the row
// weighs: the guard is asked of the bot named before the read admission, so
// a refusal neither waits on the bound nor says anything of the row's size
// (rva round 4, S4).
func TestARefusedReadIsThe403WhateverTheRowWeighsOrTheBoundHolds(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	f.storeRow(t, "t1", "small", launchSourceBot("SMALL"))
	f.storeRow(t, "t1", "large", launchSourceBot("LARGE")+"## "+strings.Repeat("x", 64<<10)+"\n")
	f.s.readAdmission = &readAdmissionGate{sem: semaphore.NewWeighted(8), capacity: 8}
	// A concurrent reading holds the whole bound.
	if err := f.s.readAdmission.sem.Acquire(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	defer f.s.readAdmission.sem.Release(8)
	var bodies []string
	for _, name := range []string{"small", "large"} {
		w := f.readLaunchSource(f.member, name)
		if w.Code != http.StatusForbidden {
			t.Fatalf("a member reading the %s row while the bound is held = %d, want 403; body=%s", name, w.Code, w.Body.String())
		}
		bodies = append(bodies, strings.ReplaceAll(w.Body.String(), name, "<name>"))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the two refusals differ beyond the name:\n%s\n%s", bodies[0], bodies[1])
	}
	// The premise: a reader of those rows does wait on the bound.
	if w := f.readLaunchSource(f.admin, "small"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("premise: a reader while the bound is held = %d, want 503", w.Code)
	}
}

// A pin is judged with the payload, before admission: what it must look
// like — the digest the read wrote, nothing near it — and what it may stand
// beside. A malformed request meters nothing and is told what a pin is,
// instead of "the bot changed" after a full resolution (rva round 1, V4).
func TestAMalformedOrMisplacedPinIsRefusedBeforeAdmission(t *testing.T) {
	sha := contentSHA256(launchSourceBot("BAKED"))
	for _, tc := range []struct {
		name string
		body map[string]string
		want string
	}{
		{"uppercase hex", map[string]string{"bot_id": "probe-bot", "expect_source_sha256": strings.ToUpper(sha)}, "expect_source_sha256 is not a pin"},
		{"padded", map[string]string{"bot_id": "probe-bot", "expect_source_sha256": " " + sha + " "}, "expect_source_sha256 is not a pin"},
		{"a sha256: prefix", map[string]string{"bot_id": "probe-bot", "expect_source_sha256": "sha256:" + sha}, "expect_source_sha256 is not a pin"},
		{"too short", map[string]string{"bot_id": "probe-bot", "expect_source_sha256": sha[:63]}, "expect_source_sha256 is not a pin"},
		{"a megabyte of garbage", map[string]string{"bot_id": "probe-bot", "expect_source_sha256": strings.Repeat("z", 1<<20)}, "this value is 1048576 characters long"},
		{"beside an inline source", map[string]string{"source": launchSourceBot("INLINE"), "bot_id": "probe-bot", "expect_source_sha256": sha}, "an inline source is its own program"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchSourceFixture(t, "cloud")
			body, _ := json.Marshal(tc.body)
			rec := postLaunch(t, f.s, f.member, string(body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status = %d body = %.300s, want the 400 saying %q", rec.Code, rec.Body.String(), tc.want)
			}
			if len(rec.Body.String()) > 1024 {
				t.Fatalf("the refusal echoes the value: %d bytes", len(rec.Body.String()))
			}
			if n := len(f.cap.all()); n != 0 {
				t.Fatalf("the refused launch published %d programs, want 0", n)
			}
		})
	}

	// "Before admission" is the claim, so it is witnessed on a gate that
	// refuses: a suspended team answers a well-formed launch 403, and the
	// malformed one is still told what is wrong with it — the payload was
	// judged first, and no slot was asked for.
	t.Run("the payload is judged before the gate", func(t *testing.T) {
		f := &launchSourceFixture{cap: &programCapture{}, counter: orgusage.NewMemoryCounter()}
		f.s = newOrgTestServer(t)
		suspended := seedGate(t, f.s, gateSpec{id: "t1", teamStatus: identity.TeamStatusSuspended})
		f.s.orgUsage = f.counter
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		f.s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(f.cap))
		if rec := postLaunch(t, f.s, suspended, `{"bot_id":"probe-bot"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("premise: a launch on a suspended team = %d, want the gate's 403; body=%s", rec.Code, rec.Body.String())
		}
		rec := postLaunch(t, f.s, suspended, `{"bot_id":"probe-bot","expect_source_sha256":"not-a-pin"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expect_source_sha256 is not a pin") {
			t.Fatalf("a malformed pin on a suspended team = %d %s, want the payload's 400, not the gate's refusal", rec.Code, rec.Body.String())
		}
		body, _ := json.Marshal(map[string]string{"source": launchSourceBot("INLINE"), "expect_source_sha256": sha})
		rec = postLaunch(t, f.s, suspended, string(body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "an inline source is its own program") {
			t.Fatalf("a pin beside an inline source on a suspended team = %d %s, want the payload's 400", rec.Code, rec.Body.String())
		}
	})
}

// A pin is held against a bot the server resolved, or the launch does not
// run: a file launch names no bot, so no read can have issued its pin — and
// a pin nobody checks would be a guarantee the caller believes and nobody
// keeps.
func TestAPinOnALaunchThatResolvesNoBotIsRefused(t *testing.T) {
	sha := contentSHA256(launchSourceBot("BAKED"))
	f := newLaunchSourceFixture(t, "cloud")
	work := t.TempDir()
	f.s.cfg.WorkDir = work
	// A path the cloud launch infers no bot label from (inferCatalogBotID).
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sub", "x.bot"), []byte(launchSourceBot("FILE")), 0o600); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"file_path": "sub/x.bot", "expect_source_sha256": sha}
	raw, _ := json.Marshal(body)
	rec := postLaunch(t, f.s, f.member, string(raw))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "this launch resolves none (a file names no bot)") {
		t.Fatalf("the pinned file launch = %d %s, want the 400 that refuses a pin nobody checks", rec.Code, rec.Body.String())
	}
	if n := len(f.cap.all()); n != 0 {
		t.Fatalf("the refused launch published %d programs, want 0", n)
	}
	if got := f.meteredRuns(t); got != 0 {
		t.Fatalf("monthly runs = %d after a refused launch, want 0", got)
	}

	// The control: without the pin the request is the cloud's own refusal of
	// a file that names no bot — the pin's sentence was about the pin.
	delete(body, "expect_source_sha256")
	raw, _ = json.Marshal(body)
	rec = postLaunch(t, f.s, f.member, string(raw))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "expect_source_sha256") || !strings.Contains(rec.Body.String(), "a catalog bot_id is required") {
		t.Fatalf("the same launch without a pin = %d %s, want the cloud's refusal of a file launch", rec.Code, rec.Body.String())
	}
}

// No pin, nothing compared: a launch naming a bot that changed a moment ago
// runs the bot as it is now — the CLI's, the MCP server's, a webhook's and a
// schedule's launch, none of which read the program first.
func TestALaunchWithoutAPinComparesNothing(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
	f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM-EDITED"))
	rec := postLaunch(t, f.s, f.member, `{"bot_id":"probe-bot"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the unpinned launch = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	ran := f.cap.only(t)
	if ran.spec.BotID != "probe-bot" || !strings.Contains(ran.main, "TEAM-EDITED") {
		t.Fatalf("the launch ran (bot %q, entry %q), want the team's row as stored now", ran.spec.BotID, ran.main)
	}
}

// countMaterializations counts the directories a resolution materializes
// (iterion-launch-bot-*, iterion-bundle-snapshot-*) under dir.
func countMaterializations(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "iterion-") {
			n++
		}
	}
	return n
}

// Every read and every refused launch gives back what its resolution
// materialized: a form opened fifty times leaves no bundle on the server's
// disk, whatever each answer was (rva round 1, V5).
func TestTheLaunchSourceLeavesNoMaterializationBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := newLaunchSourceFixture(t, "cloud")
	f.storeRow(t, "t1", "probe-bot", launchSourceBot("TEAM"))
	f.storeRow(t, "t1", "broken", "workflow nope\n  this does not parse\n")
	f.storeRow(t, botsource.PlatformTenantID, "plat", launchSourceBot("PLATFORM"))
	stale := contentSHA256("another program")
	before := countMaterializations(t, tmp)
	statuses := map[string]int{}
	for i := 0; i < 25; i++ {
		statuses[fmt.Sprintf("read %d", f.readLaunchSource(f.admin, "probe-bot").Code)]++
		statuses[fmt.Sprintf("refused read %d", f.readLaunchSource(f.member, "probe-bot").Code)]++
		statuses[fmt.Sprintf("unlaunchable %d", f.readLaunchSource(f.admin, "broken").Code)]++
		statuses[fmt.Sprintf("platform %d", f.readLaunchSource(f.member, "plat").Code)]++
		statuses[fmt.Sprintf("unknown %d", f.readLaunchSource(f.admin, "ghost").Code)]++
		statuses[fmt.Sprintf("stale launch %d", postLaunch(t, f.s, f.admin, pinnedLaunch("probe-bot", stale)).Code)]++
	}
	want := map[string]int{"read 200": 25, "refused read 403": 25, "unlaunchable 422": 25, "platform 200": 25, "unknown 404": 25, "stale launch 409": 25}
	for k, n := range want {
		if statuses[k] != n {
			t.Fatalf("statuses = %v, want %v — the witness did not walk the paths it names", statuses, want)
		}
	}
	if after := countMaterializations(t, tmp); after != before {
		t.Fatalf("%d materialized directories left behind (before %d, after %d)", after-before, before, after)
	}
}

// The read takes its share of the aggregate DSL-reading bound and gives it
// back: with a ceiling one reading fills, a second read is served only
// because the first released — and a read is refused while the bound is held.
func TestTheLaunchSourceTakesAndReleasesItsReadAdmission(t *testing.T) {
	f := newLaunchSourceFixture(t, "cloud")
	f.s.readAdmission = &readAdmissionGate{sem: semaphore.NewWeighted(8), capacity: 8}
	for i := 1; i <= 3; i++ {
		if w := f.readLaunchSource(f.member, "probe-bot"); w.Code != http.StatusOK {
			t.Fatalf("read #%d = %d, want 200 — the read before it kept its share of the bound; body=%s", i, w.Code, w.Body.String())
		}
	}
	// A refused and a failed read give it back too.
	f.storeRow(t, "t1", "broken", "workflow nope\n  this does not parse\n")
	if w := f.readLaunchSource(f.admin, "broken"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("premise: the broken row = %d, want 422", w.Code)
	}
	if w := f.readLaunchSource(f.member, "probe-bot"); w.Code != http.StatusOK {
		t.Fatalf("a read after a failed one = %d, want 200", w.Code)
	}

	if err := f.s.readAdmission.sem.Acquire(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	w := f.readLaunchSource(f.member, "probe-bot")
	f.s.readAdmission.sem.Release(8)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "dsl_read_admission") {
		t.Fatalf("a read while the bound is held = %d %s, want 503 dsl_read_admission", w.Code, w.Body.String())
	}
}
