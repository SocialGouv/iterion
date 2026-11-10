// The Revi bots this repo SHIPS (bots/review-pr, bots/revi-converse), read
// through the launch's own readers — the publisher's walk
// (model.EffectiveProviders), the ONE call-site enumeration (model.CallSites)
// and the supervisors' runtime specs (supervise.SpecsFromWorkflow) — with
// every `${…}` dial the bots read blanked, so the reading is the catalogue's
// own defaults whatever the host exports.
//
// The default launch (no routing policy) is pinned byte for byte to a golden
// generated on origin/main before the routing-cells change: whatever the
// cells learn, a run no cell reaches reads exactly what it read before.

package cloudpublisher

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// updateReviWalkGolden regenerates testdata/revi_walk_default.golden.json.
// Only ever run on a tree whose readers are the reference: the golden is the
// statement "a default launch reads what origin/main read".
var updateReviWalkGolden = flag.Bool("update-revi-walk-golden", false, "rewrite the Revi default-walk golden")

// reviBots are the two shipped Revi bundles.
var reviBots = []string{"review-pr", "revi-converse"}

// catalogBotDir is a shipped bot's directory, from this package.
func catalogBotDir(t *testing.T, bot string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "bots", bot))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// freezeCatalogBotDials blanks every `${…}` name any `.bot` file of the
// bundle references, plus the supervisor environment defaults, so each span
// reads its own default — the catalogue's reading, not the host's.
func freezeCatalogBotDials(t *testing.T, bot string) {
	t.Helper()
	var texts []string
	err := filepath.WalkDir(catalogBotDir(t, bot), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".bot") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		texts = append(texts, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(texts, "\n")
	for _, n := range envDialNames(all) {
		t.Setenv(n, "")
	}
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
}

// compileShippedBot compiles one shipped bot, imports included — the
// workflow its launch publishes.
func compileShippedBot(t *testing.T, bot string) *ir.Workflow {
	t.Helper()
	path := filepath.Join(catalogBotDir(t, bot), "main.bot")
	u := unittest.Whole(unit.LoadDir(path))
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse %s: %s", path, d.Error())
		}
	}
	if u.Merged == nil {
		t.Fatalf("parse %s: no AST", path)
	}
	cr := ir.Compile(u.Merged)
	if cr.HasErrors() {
		t.Fatalf("compile %s: %s", path, cr.Diagnostics[0].Error())
	}
	return cr.Workflow
}

// reviWalkSnapshot is what the readers say about one bot under one view.
type reviWalkSnapshot struct {
	Walk          model.ProviderResolution `json:"walk"`
	OnlyEnvFunded bool                     `json:"only_env_funded"`
	CallSites     []reviCallSite           `json:"call_sites"`
	Supervisors   []reviSupervisorSpec     `json:"supervisors"`
}

type reviCallSite struct {
	ID      string          `json:"id"`
	Kind    model.SiteKind  `json:"kind"`
	Backend string          `json:"backend"`
	Legs    []credroute.Leg `json:"legs"`
	Model   string          `json:"model"`
	Role    credroute.Role  `json:"role"`
	Host    bool            `json:"host"`
	Rescue  bool            `json:"rescue"`
}

type reviSupervisorSpec struct {
	Name           string `json:"name"`
	Model          string `json:"model"`
	ProviderHint   string `json:"provider_hint"`
	GatewayWatched bool   `json:"gateway_watched"`
}

// reviReadersUnder reads wf through the launch's readers under view.
func reviReadersUnder(t *testing.T, wf *ir.Workflow, view model.RouteView) reviWalkSnapshot {
	t.Helper()
	walk := model.EffectiveProviders(view, walkProviderSet)
	snap := reviWalkSnapshot{Walk: walk, OnlyEnvFunded: walk.OnlyEnvFunded()}
	for _, s := range model.CallSites(view, nil) {
		snap.CallSites = append(snap.CallSites, reviCallSite{
			ID: s.ID, Kind: s.Kind, Backend: s.Backend, Legs: s.Legs, Model: s.Model,
			Role: s.Role, Host: s.Host, Rescue: s.Rescue,
		})
	}
	for _, s := range reviSupervisorSpecs(wf, view) {
		snap.Supervisors = append(snap.Supervisors, reviSupervisorSpec{
			Name: s.Name, Model: s.Model, ProviderHint: s.ProviderHint, GatewayWatched: s.GatewayWatched,
		})
	}
	return snap
}

// reviSupervisorSpecs is the supervisors' runtime reading of wf under view.
func reviSupervisorSpecs(wf *ir.Workflow, view model.RouteView) []supervise.Spec {
	return supervise.SpecsFromWorkflow(wf, view.DecidedVars(), iterlog.Nop())
}

// TestReviShippedBots_DefaultLaunchReadsWhatOriginMainRead pins the default
// launch — no routing policy, no override row — of both shipped Revi bots,
// every reader, to the golden generated on origin/main before the routing
// cells learned their route-scoped and supervisor selectors.
func TestReviShippedBots_DefaultLaunchReadsWhatOriginMainRead(t *testing.T) {
	got := map[string]reviWalkSnapshot{}
	for _, bot := range reviBots {
		freezeCatalogBotDials(t, bot)
		wf := compileShippedBot(t, bot)
		view := model.RouteViewForLaunch(wf, model.ModelOverrides{}, nil, map[string]string(nil), nil)
		got[bot] = reviReadersUnder(t, wf, view)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	golden := filepath.Join("testdata", "revi_walk_default.golden.json")
	if *updateReviWalkGolden {
		if err := os.WriteFile(golden, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(want) != string(raw) {
		t.Fatalf("the default launch of the shipped Revi bots no longer reads what origin/main read — a cell-less run must not move.\n--- want (%s)\n%s\n--- got\n%s", golden, want, raw)
	}
	// The golden is a statement about real routes, not an empty file.
	if w := got["review-pr"].Walk; !slices.Contains(w.Providers, "anthropic") || !slices.Contains(w.Providers, "zai") {
		t.Fatalf("review-pr's default walk names %v — the golden fixture is not reading the shipped bot", w.Providers)
	}
}
