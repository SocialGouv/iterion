package bots

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// The catalogue floor ADR-169 §4 names for C315: a run reads no environment
// (ADR-132), and over the shipped workflows the warning is a floor.
//
//   - A retired family is read by no shipped workflow, wherever C315 looks (a
//     node's or a supervisor's route field, a var default, a tool body, the
//     workflow budget's max_duration): `ITERION_VIBE_*` and
//     `ITERION_MODERNIZE_*` left the catalogue together (#2699), and a listing
//     below cannot let one back in. A shipped `.bot` does not even spell one
//     (TestCatalogSpellsNoRetiredDial): a bare `$NAME` in a tool body is the
//     child shell's own read (ADR-130), which C315 leaves alone by design.
//   - A workflow reads only the names stillDialled lists for it, so a
//     de-dialled bot cannot grow a dial back and a new workflow ships none —
//     an unlisted workflow is listed with nothing.
//   - The list only shrinks: a listed name the workflow no longer reads fails
//     until its line goes, so the ratchet records every de-dial that lands.
//
// The verdict is the compiler's own C315 diagnostic over each workflow loaded
// the way a launch loads it (`unit.LoadDir`, imports merged), as the sibling
// C188 floor does (catalog_braced_env_test.go); the name is read from the
// diagnostic, never from a second reading of the text.
//
// Mutation that reddens it: put back one `${ITERION_VIBE_MODEL_CLAUDE:-…}` in
// review-pr (retired family), add `${ITERION_X:-y}` to any clean workflow's
// route (unlisted dial), or de-dial a listed name without removing its line
// (stale listing). The walk itself is pkg/dsl/ir's
// (TestEnvDialNamesWhatDispatchReads holds it to what dispatch reads).
var retiredDialFamilies = []string{"ITERION_VIBE_", "ITERION_MODERNIZE_"}

// stillDialled is the set of environment names each shipped workflow still
// reads in a run setting, keyed by its path relative to bots/. It is the
// remaining de-dial work, family by family (ADR-169 §3): shrink it, never
// grow it.
var stillDialled = map[string][]string{
	"adr-cartograph/main.bot": {
		"ITERION_ADRY_EFFORT_CLAUDE", "ITERION_ADRY_MODEL_CLAUDE",
		"ITERION_ADR_EFFORT_CLAUDE", "ITERION_ADR_MODEL_CLAUDE",
		"ITERION_DISPATCHER_PORT",
	},
	"adr-rechallenge/main.bot": {
		"REARCHI_EFFORT", "REARCHI_MODEL_CLAUDE",
	},
	"app-dev/main.bot": {
		"ITERION_APP_DEV_DEPLOY_EFFORT", "ITERION_APP_DEV_DEPLOY_MODEL",
		"ITERION_APP_DEV_EFFORT_CLAUDE", "ITERION_APP_DEV_EFFORT_INTERVIEW",
		"ITERION_APP_DEV_MODEL_CLAUDE", "ITERION_APP_DEV_MR_EFFORT",
		"ITERION_APP_DEV_MR_MODEL", "ITERION_PLAN_REVIEW_BACKEND_GPT",
		"ITERION_PLAN_REVIEW_MODEL_GPT",
	},
	"arbitrate/main.bot": {
		"ITERION_ARBITRATE_EFFORT", "ITERION_ARBITRATE_MODEL",
	},
	"assessment/main.bot": {
		"ITERION_ASSESSMENT_GATE_PROBE_TIMEOUT_S",
		"ITERION_ASSESSMENT_JUDGEMENT_BACKEND",
		"ITERION_ASSESSMENT_JUDGEMENT_EFFORT", "ITERION_ASSESSMENT_JUDGEMENT_MODEL",
		"ITERION_ASSESSMENT_PLAN_BACKEND", "ITERION_ASSESSMENT_PLAN_EFFORT",
		"ITERION_ASSESSMENT_PLAN_MODEL", "ITERION_ASSESSMENT_SURVEY_BACKEND",
		"ITERION_ASSESSMENT_SURVEY_EFFORT", "ITERION_ASSESSMENT_SURVEY_MODEL",
	},
	"bmady/main.bot": {
		"BMADY_EFFORT_DEV", "BMADY_EFFORT_PLAN", "BMADY_EFFORT_QA",
		"BMADY_MODEL_CLAUDE", "BMADY_MODEL_QA",
	},
	"branch-improve-loop/main.bot": {
		"ITERION_BRANCH_IMPROVE_MR_EFFORT", "ITERION_BRANCH_IMPROVE_MR_MODEL",
		"ITERION_PLAN_REVIEW_BACKEND_GPT", "ITERION_PLAN_REVIEW_MODEL_GPT",
	},
	"copilot/main.bot": {
		"ITERION_COPILOT_EFFORT", "ITERION_COPILOT_ENTRY_MODEL",
		"ITERION_COPILOT_FALLBACK_MODEL", "ITERION_COPILOT_REFLECTION_EFFORT",
		"ITERION_COPILOT_REFLECTION_MODEL", "ITERION_COPILOT_REVIEWER_EFFORT",
		"ITERION_COPILOT_REVIEWER_MODEL",
	},
	"dep-update-guard/main.bot": {
		"VETTY_EFFORT_ALIGN", "VETTY_EFFORT_AUDIT", "VETTY_EFFORT_COMMIT",
		"VETTY_EFFORT_VALIDATE", "VETTY_MODEL_ALIGN", "VETTY_MODEL_AUDIT",
		"VETTY_MODEL_COMMIT", "VETTY_MODEL_ESCALATE", "VETTY_MODEL_VALIDATE",
	},
	"devbox-setup/main.bot": {
		"DEVBOX_SETUP_MODEL",
	},
	"docs-refresh/main.bot": {
		"ITERION_DOC_ALIGN_EFFORT_CLAUDE", "ITERION_DOC_ALIGN_MODEL_AUDIT",
		"ITERION_DOC_ALIGN_MODEL_CLAUDE", "ITERION_DOC_ALIGN_MR_EFFORT",
	},
	"e2e-coverage/main.bot": {
		"ITERION_PLAN_REVIEW_BACKEND_GPT", "ITERION_PLAN_REVIEW_MODEL_GPT",
	},
	"evolve/main.bot": {
		"ITERION_EVOLVE_EFFORT", "ITERION_EVOLVE_EFFORT_REVIEW",
		"ITERION_EVOLVE_EFFORT_SYNTH", "ITERION_EVOLVE_MODEL_CLAUDE",
		"ITERION_EVOLVE_MODEL_GPT",
	},
	"../examples/clarify/main.bot": {
		"CLARIFY_MODEL",
	},
	"feature-dev/main.bot": {
		"ITERION_FEATURE_DEV_MR_EFFORT", "ITERION_FEATURE_DEV_MR_MODEL",
	},
	"feature-gap-fill/main.bot": {
		"ITERION_PLAN_REVIEW_BACKEND_GPT", "ITERION_PLAN_REVIEW_MODEL_GPT",
	},
	"feed-watch/main.bot": {
		"FEED_WATCH_BACKEND", "FEED_WATCH_EFFORT", "FEED_WATCH_MODEL",
	},
	"golden-master/extend.bot": {
		"ITERION_GOLDEN_MASTER_EFFORT", "ITERION_GOLDEN_MASTER_MODEL_CLAUDE",
	},
	"golden-master/main.bot": {
		"ITERION_GOLDEN_MASTER_ADVERSARY_EFFORT",
		"ITERION_GOLDEN_MASTER_ADVERSARY_MODEL",
	},
	"golden-master/reanchor.bot": {
		"ITERION_GOLDEN_MASTER_EFFORT", "ITERION_GOLDEN_MASTER_MODEL_CLAUDE",
	},
	"instrument/main.bot": {
		"ITERION_INSTRUMENT_MR_EFFORT", "ITERION_INSTRUMENT_MR_MODEL",
	},
	"product-docs/main.bot": {
		"ITERION_PRODUCT_DOCS_EFFORT_CLAUDE", "ITERION_PRODUCT_DOCS_MODEL_CLAUDE",
		"ITERION_PRODUCT_DOCS_MR_EFFORT", "ITERION_PRODUCT_DOCS_PUBLISH_EFFORT",
	},
	"review-env/main.bot": {
		"ITERION_REVIEW_ENV_MODEL",
	},
	"rgaa-audit/main.bot": {
		"ITERION_RGAA_BACKEND_ADJUDICATE", "ITERION_RGAA_BACKEND_REFUTE",
		"ITERION_RGAA_EFFORT_ADJUDICATE", "ITERION_RGAA_EFFORT_REFUTE",
		"ITERION_RGAA_MAX_DURATION", "ITERION_RGAA_MODEL_ADJUDICATE",
		"ITERION_RGAA_MODEL_REFUTE",
	},
	"sec-audit-deps/main.bot": {
		"ITERION_SEC_AUDIT_EFFORT_ENUMERATE", "ITERION_SEC_AUDIT_EFFORT_REVIEW",
		"ITERION_SEC_AUDIT_MODEL", "ITERION_SEC_AUDIT_MODEL_CLAUDE",
	},
	"sec-audit-source/main.bot": {
		"HOME", "ITERION_SEC_AUDIT_BACKEND", "ITERION_SEC_AUDIT_DETECT_BACKEND",
		"ITERION_SEC_AUDIT_DETECT_MODEL", "ITERION_SEC_AUDIT_EFFORT_CONTEXT",
		"ITERION_SEC_AUDIT_EFFORT_DETECT", "ITERION_SEC_AUDIT_EFFORT_REPORT",
		"ITERION_SEC_AUDIT_EFFORT_TRIAGE", "ITERION_SEC_AUDIT_EFFORT_VOTER",
		"ITERION_SEC_AUDIT_MAX_DURATION", "ITERION_SEC_AUDIT_MODEL",
		"ITERION_SEC_AUDIT_MODEL_CLAUDE", "ITERION_SEC_AUDIT_PROVIDER_CHAIN",
		"ITERION_SEC_AUDIT_VOTER_V1_MODEL", "ITERION_SEC_AUDIT_VOTER_V2_MODEL",
		"ITERION_SEC_AUDIT_VOTER_V3_MODEL", "ITERION_SEC_PATCH_EFFORT_AUTHOR",
		"ITERION_SEC_PATCH_EFFORT_REATTACK", "ITERION_SEC_PATCH_EFFORT_REPORT",
		"ITERION_SEC_PATCH_EFFORT_REVIEW", "ITERION_SEC_PATCH_MODEL",
	},
	"secured-renovacy/main.bot": {
		"ITERION_RENOVACY_EFFORT_ALIGN", "ITERION_RENOVACY_EFFORT_AUDIT",
		"ITERION_RENOVACY_EFFORT_BATCH_UPGRADE",
		"ITERION_RENOVACY_EFFORT_CHANGELOG", "ITERION_RENOVACY_EFFORT_CLAUDE",
		"ITERION_RENOVACY_EFFORT_DETECT", "ITERION_RENOVACY_EFFORT_DISCOVER",
		"ITERION_RENOVACY_EFFORT_FAMILY_ALIGN", "ITERION_RENOVACY_EFFORT_FIX_AFTER",
		"ITERION_RENOVACY_EFFORT_INSTALL", "ITERION_RENOVACY_EFFORT_REVIEW",
		"ITERION_RENOVACY_EFFORT_UPGRADE", "ITERION_RENOVACY_EFFORT_VALIDATE",
		"ITERION_RENOVACY_MODEL_CLAUDE", "RESCUE_PROVIDER",
	},
	"supply-shield-cve/main.bot": {
		"ITERION_FORGE_REPORT_EFFORT", "ITERION_SEC_AUDIT_EFFORT_ENUMERATE",
		"ITERION_SEC_AUDIT_EFFORT_REVIEW", "ITERION_SEC_AUDIT_MODEL",
		"ITERION_SEC_AUDIT_MODEL_CLAUDE",
	},
	"supply-shield/main.bot": {
		"ITERION_FORGE_REPORT_EFFORT", "ITERION_SEC_AUDIT_EFFORT_ENUMERATE",
		"ITERION_SEC_AUDIT_EFFORT_REVIEW", "ITERION_SEC_AUDIT_MODEL",
		"ITERION_SEC_AUDIT_MODEL_CLAUDE",
	},
	"test-coverage/main.bot": {
		"ITERION_PLAN_REVIEW_BACKEND_GPT", "ITERION_PLAN_REVIEW_MODEL_GPT",
	},
	"whats-next/main.bot": {
		"ITERION_WHATS_NEXT_EFFORT_CLAUDE", "ITERION_WHATS_NEXT_MODEL_CLAUDE",
	},
	"whole-improve-loop/main.bot": {
		"ITERION_PLAN_REVIEW_BACKEND_GPT", "ITERION_PLAN_REVIEW_MODEL_GPT",
		"ITERION_WHOLE_IMPROVE_MR_EFFORT", "ITERION_WHOLE_IMPROVE_MR_MODEL",
	},
	"wiki-gen/main.bot": {
		"ITERION_WIKI_EFFORT_CLAUDE", "ITERION_WIKI_MODEL_CLAUDE",
	},
}

// dialNameInMessage reads the one braced name a C315 message carries
// (validateEnvDials writes exactly one `${NAME}` into each).
var dialNameInMessage = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func TestCatalogEnvDialFloor(t *testing.T) {
	targets := catalogWorkflowFiles(t)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}
	seen := map[string]bool{}
	var failures []string
	for _, path := range targets {
		seen[path] = true
		u := unittest.Whole(unit.LoadDir(path))
		if u.Merged == nil {
			t.Errorf("%s: no merged file — this floor could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no workflow — this floor could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		read := map[string]bool{}
		for _, d := range cr.Diagnostics {
			if d.Code != ir.DiagEnvDialInRunSetting {
				continue
			}
			m := dialNameInMessage.FindAllStringSubmatch(d.Message, -1)
			if len(m) != 1 {
				t.Fatalf("%s: a C315 message carries %d braced names, want exactly one — this floor reads the name from the diagnostic: %s", path, len(m), d.Message)
			}
			read[m[0][1]] = true
		}
		allowed := stillDialled[path]
		for name := range read {
			for _, family := range retiredDialFamilies {
				if strings.HasPrefix(name, family) {
					failures = append(failures, fmt.Sprintf("%s reads ${%s}: the %s* family is retired (ADR-169) — write the literal, and re-target a tenant with a routing cell", path, name, family))
				}
			}
			if !slices.Contains(allowed, name) {
				failures = append(failures, fmt.Sprintf("%s reads ${%s}, which stillDialled does not list for it — a run reads no environment (ADR-132): write the literal, and re-target a tenant with a routing cell", path, name))
			}
		}
		for _, name := range allowed {
			if !read[name] {
				failures = append(failures, fmt.Sprintf("%s no longer reads ${%s}: remove it from stillDialled — the list only shrinks", path, name))
			}
		}
	}
	for path := range stillDialled {
		if !seen[path] {
			failures = append(failures, fmt.Sprintf("stillDialled lists %s, which is no shipped workflow — remove the line", path))
		}
		for _, name := range stillDialled[path] {
			for _, family := range retiredDialFamilies {
				if strings.HasPrefix(name, family) {
					failures = append(failures, fmt.Sprintf("stillDialled lists ${%s} for %s: the %s* family is retired and cannot be listed back", name, path, family))
				}
			}
		}
	}
	sort.Strings(failures)
	for _, f := range failures {
		t.Error(f)
	}
}

// TestCatalogSpellsNoRetiredDial: no shipped `.bot` names a retired family,
// in any form. C315 names what the engine's expansion reads; a bare `$NAME`
// in a tool body is read by the child shell from the environment the engine
// builds for it (ADR-130's escape hatch, which C315 leaves alone on
// purpose), and a comment that names a retired dial documents a lever that
// no longer exists. The retired families are gone from the catalogue's text.
//
// Mutation that reddens it: prepend `MODEL=$ITERION_VIBE_MODEL_CLAUDE` to a
// review-pr tool command — C315 and the floor above stay green, this fails.
func TestCatalogSpellsNoRetiredDial(t *testing.T) {
	// Every shipped `.bot`, declaration fragments (`lib/`) included: the
	// bundles and examples by walk, then catalogRoots' other roots — the
	// operator scripts and the dispatcher's embedded fallback.
	files := []string{dispatchFallbackPath}
	adhoc, err := filepath.Glob("../scripts/adhoc/*.bot")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, adhoc...)
	for _, root := range []string{".", "../examples"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".bot") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(adhoc) == 0 || len(files) <= len(adhoc)+1 {
		t.Fatalf("found %d .bot files (%d under scripts/adhoc) — the walk's roots likely moved", len(files), len(adhoc))
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, family := range retiredDialFamilies {
				if strings.Contains(line, family) {
					t.Errorf("%s:%d names the retired %s* family (ADR-169): %s", path, i+1, family, strings.TrimSpace(line))
				}
			}
		}
	}
}
