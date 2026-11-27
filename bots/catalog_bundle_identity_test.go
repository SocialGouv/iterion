package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A shipped catalog bot cannot declare workspace-relative authoring files:
// those paths would describe iterion's checkout, not an arbitrary target repo.
// Project-local bots outside bots/ may use the scope deliberately.
func TestCatalogBotsDoNotDeclareWorkspaceAuthoringFiles(t *testing.T) {
	manifests, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range manifests {
		m, err := bundle.LoadManifest(manifest)
		if err != nil {
			t.Errorf("%s: load: %v", manifest, err)
			continue
		}
		if m == nil || m.Authoring == nil {
			continue
		}
		for _, file := range m.Authoring.EditableFiles {
			if file.Scope == bundle.AuthoringScopeWorkspace {
				t.Errorf("%s: catalog bots must not declare authoring editable_files with scope workspace; use scope bundle or keep project-specific authoring in the target repository", manifest)
			}
		}
	}
}

// memVisibilityBotRe detects a per-bot memory declaration in a .bot file.
var memVisibilityBotRe = regexp.MustCompile(`visibility:\s*"bot"`)

// workflowNameRe captures the `workflow NAME:` keyword (column 0).
var workflowNameRe = regexp.MustCompile(`(?m)^workflow\s+([A-Za-z_][A-Za-z0-9_.-]*)\s*:`)

// manifestNameRe captures the top-level `name:` field in manifest.yaml
// (anchored at column 0 so `display_name:` does not match).
var manifestNameRe = regexp.MustCompile(`(?m)^name:\s*"?([A-Za-z0-9_.-]+)"?`)

// TestPerBotMemoryConsumersHaveStableBotID enforces that any catalog bot
// using per-bot memory (visibility: "bot") names its workflow, its bundle
// directory, and its manifest identically.
//
// Why: the per-bot memory tree is keyed by botID, which the runtime takes
// from the `workflow NAME:` keyword on a CLI run (executor.go: botID =
// wf.Name) but from the bundle name on a dispatcher/studio run (runview
// spec.BotID). If those differ, the bot's accumulated vision/decisions
// silently split across two on-disk trees between launch modes and the
// operator's "second session" sees empty memory. Requiring all three
// identical makes botID stable regardless of launch path.
//
// Existing multi-word bots (e.g. feature-dev dir / feature_dev workflow)
// are intentionally exempt: they don't use visibility:bot, so their botID
// divergence is harmless. Only memory consumers are gated.
func TestPerBotMemoryConsumersHaveStableBotID(t *testing.T) {
	bots := teamBotFiles(t)
	for _, botPath := range bots {
		data, err := botUnitSource(botPath)
		if err != nil {
			t.Errorf("%s: read: %v", botPath, err)
			continue
		}
		if !memVisibilityBotRe.Match(data) {
			continue // not a per-bot memory consumer; botID divergence is harmless
		}
		dir := filepath.Base(filepath.Dir(botPath))

		wf := workflowNameRe.FindSubmatch(data)
		if wf == nil {
			t.Errorf("%s: uses visibility:\"bot\" memory but no `workflow NAME:` found", botPath)
			continue
		}
		wfName := string(wf[1])

		manifestPath := filepath.Join(filepath.Dir(botPath), "manifest.yaml")
		mdata, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Errorf("%s: uses visibility:\"bot\" memory but manifest unreadable: %v", botPath, err)
			continue
		}
		mn := manifestNameRe.FindSubmatch(mdata)
		if mn == nil {
			t.Errorf("%s: manifest has no top-level name:", manifestPath)
			continue
		}
		manifestName := string(mn[1])

		if wfName != dir || manifestName != dir {
			t.Errorf(
				"%s: per-bot memory (visibility:\"bot\") requires workflow name == bundle dir == manifest name so botID is stable across CLI (wf.Name) and dispatcher (bundle name) launches; got workflow=%q dir=%q manifest=%q",
				botPath, wfName, dir, manifestName,
			)
		}
	}
}
