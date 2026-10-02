package automaintguard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repo = "SocialGouv/fixture-repo"
	org  = "SocialGouv"
	// The exit codes the script's own header contracts.
	exitAgree    = 0
	exitBroken   = 1
	exitBlind    = 2
	renovateApp  = 149553497
	deliveryApp  = 4895793
	headSHA      = "aaa1111"
	renovateBot  = "socialgouv-renovate[bot]"
	scriptRelDir = "../../scripts/auto-maintenance-check.sh"
)

// rawBody is a fixture written verbatim, for payloads that are not valid JSON.
type rawBody string

// world is the fixture set, keyed by API path with the query string stripped —
// the same key the stand-in gh derives from its argv.
type world struct {
	api      map[string]any
	repoBots any
}

// conforming is the one configuration every agreement holds on. Each mutant
// below is this world with ONE thing changed, so a mutant's red is attributable
// to that thing and to nothing else.
func conforming() *world {
	renovateCfg := `{
  "extends": ["config:recommended", "helpers:pinGitHubActionDigests"],
  "dependencyDashboard": true,
  "vulnerabilityAlerts": { "enabled": true }
}`
	return &world{
		api: map[string]any{
			"repos/" + repo: map[string]any{"default_branch": "main"},

			"repos/" + repo + "/rulesets": []any{
				map[string]any{"id": 1, "target": "branch", "enforcement": "active"},
			},
			"repos/" + repo + "/rulesets/1": map[string]any{
				"bypass_actors": []any{},
				"rules": []any{map[string]any{
					"type": "required_status_checks",
					"parameters": map[string]any{"required_status_checks": []any{
						map[string]any{"context": "test", "integration_id": 15368},
						map[string]any{"context": "revi/review", "integration_id": deliveryApp},
					}},
				}},
			},

			"repos/" + repo + "/pulls": []any{map[string]any{
				"number": 1,
				"user":   map[string]any{"login": renovateBot, "type": "Bot"},
				"head":   map[string]any{"sha": headSHA},
			}},
			"repos/" + repo + "/commits/" + headSHA + "/check-runs": map[string]any{
				"total_count": 1,
				"check_runs":  []any{map[string]any{"name": "test"}},
			},
			"repos/" + repo + "/commits/" + headSHA + "/statuses": []any{
				map[string]any{"context": "revi/review"},
			},

			"orgs/" + org + "/installations": map[string]any{
				"total_count": 2,
				"installations": []any{
					map[string]any{
						"id": renovateApp, "app_slug": "socialgouv-renovate",
						"repository_selection": "all",
						"permissions": map[string]any{
							"contents": "write", "issues": "write",
							"vulnerability_alerts": "read", "workflows": "write",
						},
					},
					map[string]any{
						"id": deliveryApp, "app_slug": "iterion-forge-core",
						"repository_selection": "all",
						"permissions": map[string]any{
							"contents": "write", "statuses": "write", "workflows": "write",
						},
					},
				},
			},

			"repos/" + repo + "/contents/renovate.json5": map[string]any{
				"content": base64.StdEncoding.EncodeToString([]byte(renovateCfg)),
			},
			"repos/" + repo + "/actions/secrets": map[string]any{
				"total_count": 2,
				"secrets": []any{
					map[string]any{"name": "RENOVATE_APP_ID"},
					map[string]any{"name": "RENOVATE_APP_PRIVATE_KEY"},
				},
			},
		},
		repoBots: map[string]any{"integrations": []any{map[string]any{
			"id":             "11111111-1111-1111-1111-111111111111",
			"repo_full_name": repo,
			"bot_ids":        []any{"dep-update-guard", "review-pr"},
			"launch_vars": map[string]any{
				"gate_context": "revi/review", "arm_automerge": "false",
			},
		}}},
	}
}

// ruleset reaches the one required-checks list, so a mutant can edit it in place.
func (w *world) ruleset() map[string]any {
	rs := w.api["repos/"+repo+"/rulesets/1"].(map[string]any)
	return rs
}

func (w *world) requiredChecks() []any {
	rules := w.ruleset()["rules"].([]any)
	return rules[0].(map[string]any)["parameters"].(map[string]any)["required_status_checks"].([]any)
}

func (w *world) setRequiredChecks(c []any) {
	rules := w.ruleset()["rules"].([]any)
	rules[0].(map[string]any)["parameters"].(map[string]any)["required_status_checks"] = c
}

func (w *world) launchVars() map[string]any {
	integs := w.repoBots.(map[string]any)["integrations"].([]any)
	return integs[0].(map[string]any)["launch_vars"].(map[string]any)
}

func (w *world) installation(slug string) map[string]any {
	ins := w.api["orgs/"+org+"/installations"].(map[string]any)["installations"].([]any)
	for _, raw := range ins {
		m := raw.(map[string]any)
		if m["app_slug"] == slug {
			return m
		}
	}
	panic("no installation " + slug)
}

// --------------------------------------------------------------------------
// The harness: a stand-in forge on PATH, and a catalog on disk.
// --------------------------------------------------------------------------

// standInGH answers from the fixture directory and fails the way gh fails —
// non-zero with a message — when a path has no fixture. It honours --jq
// because the script uses it, and ignores --paginate because one fixture is
// one page.
const standInGH = `#!/usr/bin/env bash
set -eu
[ "${1:-}" = "api" ] || { echo "stand-in gh only knows 'api', got: $*" >&2; exit 64; }
shift
path=""; jqexpr=""
while [ $# -gt 0 ]; do
  case "$1" in
    --paginate) ;;
    --jq) jqexpr="$2"; shift ;;
    -*) ;;
    *) [ -z "$path" ] && path="$1" ;;
  esac
  shift
done
key="${path%%\?*}"
f="$FIXTURE_DIR/$(printf '%s' "$key" | tr '/' '~').json"
if [ ! -f "$f" ]; then
  echo "HTTP 404: no fixture for $key" >&2
  exit 1
fi
if [ -n "$jqexpr" ]; then exec jq -r "$jqexpr" <"$f"; fi
exec cat "$f"
`

// The two catalog files the script reads: a gate context default per bot, and
// Vetty's webhook author filter. Written rather than symlinked from bots/ so a
// mutant can move them without touching the shipped catalog.
const vettyBot = `vars:
  gate_context: string = "vetty/deps"
`

const reviewBot = `vars:
  gate_context: string = "revi/review"
`

const vettyManifest = `forge:
  webhook:
    author_allowlist:
      ["dependabot[bot]", "*dependabot[bot]", "renovate[bot]", "*renovate[bot]"]
`

type result struct {
	code   int
	stdout string
	stderr string
}

func (r result) all() string { return r.stdout + "\n" + r.stderr }

// opts carries the few things a scenario needs beyond the fixture world.
type opts struct {
	env      []string
	args     []string // extra argv after the repository
	dir      string   // working directory; "" = the test's own temp dir
	manifest string   // Vetty's manifest body; "" = the shipped shape
}

func run(t *testing.T, w *world, env ...string) result {
	t.Helper()
	return runWith(t, w, opts{env: env})
}

func runWith(t *testing.T, w *world, o opts) result {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is required by scripts/auto-maintenance-check.sh and by this witness; "+
			"it is declared in devbox.json, so run through `devbox run --`: %v", err)
	}

	dir := t.TempDir()
	fixtures := filepath.Join(dir, "fixtures")
	mustMkdir(t, fixtures)
	for path, body := range w.api {
		var raw string
		// rawBody goes to disk byte for byte: a fixture that is deliberately
		// NOT JSON cannot travel through json.Marshal, which is the only way
		// to exercise the parse-before-use guard.
		if lit, ok := body.(rawBody); ok {
			raw = string(lit)
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(b)
		}
		name := strings.ReplaceAll(path, "/", "~") + ".json"
		mustWrite(t, filepath.Join(fixtures, name), raw)
	}

	bin := filepath.Join(dir, "bin")
	mustMkdir(t, bin)
	gh := filepath.Join(bin, "gh")
	mustWrite(t, gh, standInGH)
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}

	bots := filepath.Join(dir, "bots")
	mustMkdir(t, filepath.Join(bots, "dep-update-guard"))
	mustMkdir(t, filepath.Join(bots, "review-pr"))
	manifest := o.manifest
	if manifest == "" {
		manifest = vettyManifest
	}
	mustWrite(t, filepath.Join(bots, "dep-update-guard", "main.bot"), vettyBot)
	mustWrite(t, filepath.Join(bots, "dep-update-guard", "manifest.yaml"), manifest)
	mustWrite(t, filepath.Join(bots, "review-pr", "main.bot"), reviewBot)

	repoBots, err := json.Marshal(w.repoBots)
	if err != nil {
		t.Fatal(err)
	}
	rbFile := filepath.Join(dir, "repobots.json")
	mustWrite(t, rbFile, string(repoBots))

	script, err := filepath.Abs(scriptRelDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script, repo}, o.args...)...)
	if o.dir != "" {
		cmd.Dir = o.dir
	} else {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"FIXTURE_DIR="+fixtures,
		"AUTO_MAINT_GH="+gh,
		"AUTO_MAINT_BOTS_DIR="+bots,
		"AUTO_MAINT_ITERION=cat "+rbFile,
		"AUTO_MAINT_PR_SAMPLE=20",
	)
	cmd.Env = append(cmd.Env, o.env...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("running the preflight: %v\n%s", err, errb.String())
		}
		code = ee.ExitCode()
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func asExitError(err error, out **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*out = ee
	}
	return ok
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --------------------------------------------------------------------------
// The control: the bench must be able to go green.
// --------------------------------------------------------------------------

func TestAConformingRepositoryPasses(t *testing.T) {
	got := run(t, conforming())
	if got.code != exitAgree {
		t.Fatalf("a conforming configuration must exit %d, got %d\n%s",
			exitAgree, got.code, got.all())
	}
	for _, want := range []string{
		"1 gate context", "2 authorised producer", "3 author allowlist",
		"4 renovate permissions", "5 delivery app", "6 installation covers the repo",
		"7 every required gate executes",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("agreement %q was never evaluated — a green that skipped it proves nothing\n%s",
				want, got.stdout)
		}
	}
}

// --------------------------------------------------------------------------
// The mutants. Each names the defect it replays.
// --------------------------------------------------------------------------

func TestTheGateContextRenameAppliedToOneOfTwoSitesReddens(t *testing.T) {
	// #1591, replayed: the ruleset was renamed to `revi/review` and the
	// integration override kept `iterion/review`. Both bots then post a
	// context nothing requires, and the required context is posted by nobody.
	w := conforming()
	w.launchVars()["gate_context"] = "iterion/review"
	w.api["repos/"+repo+"/commits/"+headSHA+"/statuses"] = []any{
		map[string]any{"context": "iterion/review"},
	}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("the #1591 disagreement must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "iterion/review") {
		t.Errorf("the report never names the context the bots actually post\n%s", got.stdout)
	}
	// Both directions must fire: the verdict is advisory, AND the required
	// name is produced by nothing. Catching only one leaves half the class.
	for _, want := range []string{"its verdict is advisory", "nothing produced it"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("missing half of the disagreement (%q)\n%s", want, got.stdout)
		}
	}
}

func TestARequiredCheckWithoutItsProducerReddens(t *testing.T) {
	// #1589: a name alone is satisfied by any credential that can post a status.
	w := conforming()
	checks := w.requiredChecks()
	for _, raw := range checks {
		m := raw.(map[string]any)
		if m["context"] == "revi/review" {
			delete(m, "integration_id")
		}
	}
	w.setRequiredChecks(checks)

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an unpinned required check must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "carries no integration_id") {
		t.Errorf("the report does not name the missing producer pin\n%s", got.stdout)
	}
}

func TestTheInstalledAppOutsideTheAllowlistReddens(t *testing.T) {
	// A self-hosted App named with renovate as a PREFIX: still a Renovate for
	// agreement 4, but its bot login ends in nothing the suffix wildcard
	// matches, so Vetty's webhook never fires on its PRs.
	w := conforming()
	w.installation("socialgouv-renovate")["app_slug"] = "renovate-socialgouv"

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an installed App outside the allowlist must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "renovate-socialgouv[bot]") {
		t.Errorf("the report does not name the author Vetty never sees\n%s", got.stdout)
	}
}

func TestASelfHostedRenovateSuffixStillMatches(t *testing.T) {
	// The allowlist's leading "*" is a SUFFIX wildcard, and the conforming
	// world already relies on it (socialgouv-renovate[bot]). Pin it, because a
	// matcher that only handles exact names would pass every other test here
	// and reject every self-hosted Renovate in production.
	w := conforming()
	w.installation("socialgouv-renovate")["app_slug"] = "acme-renovate"

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("a renamed self-hosted Renovate must still match, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "acme-renovate[bot]") {
		t.Errorf("the report does not name the derived author\n%s", got.stdout)
	}
}

func TestAnotherBotOnTheRepositoryIsNotADisagreement(t *testing.T) {
	// A release bot, a tap updater — every repository has bots that are not
	// dependency bots. Calling each one a broken agreement buries the one that
	// matters under noise nobody can act on. Measured on buildkit-operator,
	// where github-actions[bot] opens the release PRs.
	w := conforming()
	prs := w.api["repos/"+repo+"/pulls"].([]any)
	w.api["repos/"+repo+"/pulls"] = append(prs, map[string]any{
		"number": 2,
		"user":   map[string]any{"login": "github-actions[bot]", "type": "Bot"},
		"head":   map[string]any{"sha": headSHA},
	})

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("an unrelated bot must not break an agreement, got exit %d\n%s", got.code, got.all())
	}
	if strings.Contains(got.stdout, "github-actions[bot]") {
		t.Errorf("an unrelated bot was reported as an agreement at all\n%s", got.stdout)
	}
}

func TestAConfigUnderDotGithubIsStillFound(t *testing.T) {
	// Both pilots of this epic keep their Renovate config at
	// .github/renovate.json5. A probe of the repository root alone reports
	// "no config" on a repository that has one — and then every permission the
	// config demands goes unchecked while the report reads as a single, minor
	// disagreement.
	w := conforming()
	cfg := w.api["repos/"+repo+"/contents/renovate.json5"]
	delete(w.api, "repos/"+repo+"/contents/renovate.json5")
	w.api["repos/"+repo+"/contents/.github/renovate.json5"] = cfg

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("a config under .github/ must be found, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, ".github/renovate.json5 demands") {
		t.Errorf("the report does not name the config path it actually read\n%s", got.stdout)
	}
}

func TestRenovateMissingAPermissionItsConfigDemandsReddens(t *testing.T) {
	// The config pins Action digests, which rewrites .github/workflows/**.
	w := conforming()
	delete(w.installation("socialgouv-renovate")["permissions"].(map[string]any), "workflows")

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a permission the config demands must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "pinGitHubActionDigests") {
		t.Errorf("the report does not say WHICH config setting demands it\n%s", got.stdout)
	}
}

func TestTheDeliveryAppThatCannotTouchWhatRenovateBumpsReddens(t *testing.T) {
	// #1595, replayed: Renovate may rewrite workflow files; the App that
	// delivers the alignment may not. The push is refused for a reason that has
	// nothing to do with the bump.
	w := conforming()
	delete(w.installation("iterion-forge-core")["permissions"].(map[string]any), "workflows")

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("the #1595 asymmetry must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "unrelated to the bump") {
		t.Errorf("the report does not explain the refusal\n%s", got.stdout)
	}
}

func TestAMissingWorkflowSecretReddens(t *testing.T) {
	w := conforming()
	w.api["repos/"+repo+"/actions/secrets"] = map[string]any{
		"total_count": 1,
		"secrets":     []any{map[string]any{"name": "RENOVATE_APP_ID"}},
	}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a missing workflow secret must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "RENOVATE_APP_PRIVATE_KEY") {
		t.Errorf("the report does not name the absent secret\n%s", got.stdout)
	}
}

func TestArmedAutoMergeWithNoRequiredCheckReddens(t *testing.T) {
	// With no required check, "clean" says nothing and the forge merges an
	// armed PR straight away.
	w := conforming()
	w.launchVars()["arm_automerge"] = "true"
	w.api["repos/"+repo+"/rulesets"] = []any{}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("armed auto-merge with no required check must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "requires NOTHING") {
		t.Errorf("the report does not name the empty ruleset\n%s", got.stdout)
	}
}

func TestArmedAutoMergeSharpensTheUnproducedGate(t *testing.T) {
	// The same missing producer is a defect either way, but on a repository
	// that merges unattended it is a configuration that CANNOT converge — the
	// arbitration of this epic, and the wording must carry it.
	w := conforming()
	w.launchVars()["arm_automerge"] = "true"
	w.api["repos/"+repo+"/commits/"+headSHA+"/statuses"] = []any{}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an unproduced required gate must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "cannot converge") {
		t.Errorf("the armed wording is missing — the report reads like the unarmed case\n%s", got.stdout)
	}
}

// --------------------------------------------------------------------------
// Fail-closed. Unknown must never read as "no disagreement".
// --------------------------------------------------------------------------

func TestAnUnreadableInstallationSelectionIsABlindSpotNotAPass(t *testing.T) {
	// `selected` means the repository may or may not be covered, and the read
	// that settles it needs a credential this script was not given. The wrong
	// answer here is a green.
	w := conforming()
	w.installation("socialgouv-renovate")["repository_selection"] = "selected"

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an unreadable installation must exit %d (blind spot), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.all(), "AUTO_MAINT_PAT") {
		t.Errorf("the blind spot does not name the credential that would lift it\n%s", got.all())
	}
	// And it must not abort the run: an operator who cannot read one thing
	// still needs every other disagreement in the SAME pass, or they fix one
	// setting per invocation.
	if !strings.Contains(got.stdout, "7 every required gate executes") {
		t.Errorf("the blind spot aborted the agreements after it\n%s", got.stdout)
	}
}

func TestARepositoryOutsideTheSelectedInstallationReddens(t *testing.T) {
	w := conforming()
	w.installation("socialgouv-renovate")["repository_selection"] = "selected"
	w.api[fmt.Sprintf("user/installations/%d/repositories", renovateApp)] = map[string]any{
		"total_count":  1,
		"repositories": []any{map[string]any{"full_name": "SocialGouv/some-other-repo"}},
	}

	got := run(t, w, "AUTO_MAINT_PAT=stand-in-token")
	if got.code != exitBroken {
		t.Fatalf("a repository outside a selected installation must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "is NOT among") {
		t.Errorf("the report does not say the repository is outside the installation\n%s", got.stdout)
	}
}

func TestAnAPIThatDoesNotAnswerIsABlindSpotNotAPass(t *testing.T) {
	w := conforming()
	delete(w.api, "repos/"+repo+"/rulesets")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an unanswered API must exit %d, got %d\n%s", exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "BLIND SPOT") {
		t.Errorf("the failure does not announce itself as a blind spot\n%s", got.stderr)
	}
}

func TestInvalidJSONIsABlindSpotNotAPass(t *testing.T) {
	// gh has been observed exiting 0 on a body that is not the payload asked
	// for; parsing before use is what separates "read" from "assumed".
	w := conforming()
	w.api["repos/"+repo+"/rulesets"] = rawBody(`{ this is not json `)

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an invalid payload must exit %d, got %d\n%s", exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "valid JSON") {
		t.Errorf("the failure does not name the unparseable payload\n%s", got.stderr)
	}
}

func TestATruncatedPageIsABlindSpotNotAPass(t *testing.T) {
	// The API reports more installations than it returned: page 2 was never
	// read, and everything on it would silently read as "no disagreement".
	w := conforming()
	ins := w.api["orgs/"+org+"/installations"].(map[string]any)
	ins["total_count"] = 7

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a truncated list must exit %d, got %d\n%s", exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "pagination truncated") {
		t.Errorf("the failure does not name the truncation\n%s", got.stderr)
	}
}

func TestAMissingToolIsABlindSpotNotAPass(t *testing.T) {
	got := run(t, conforming(), "AUTO_MAINT_GH=/nonexistent/gh-that-is-not-installed")
	if got.code != exitBlind {
		t.Fatalf("a missing tool must exit %d, got %d\n%s", exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "missing tool") {
		t.Errorf("the failure does not name the missing tool\n%s", got.stderr)
	}
}

// --------------------------------------------------------------------------
// The INPUTS the first bench never varied. Every scenario below was a full
// green before the fix it guards — the preflight blessed a loop that could not
// work.
// --------------------------------------------------------------------------

func TestARulesetScopedAwayFromTheDefaultBranchRequiresNothingHere(t *testing.T) {
	// Dependency PRs target the default branch. A ruleset gating
	// `refs/heads/release/*` requires its checks THERE, and agreement 1 must
	// not read them as "the ruleset requires it" — that is the #1591 class
	// again, arrived at through the condition instead of the name.
	w := conforming()
	w.ruleset()["conditions"] = map[string]any{
		"ref_name": map[string]any{"include": []any{"refs/heads/release/*"}, "exclude": []any{}},
	}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a ruleset that does not gate the default branch must not satisfy agreement 1, got exit %d\n%s",
			got.code, got.all())
	}
	if !strings.Contains(got.stdout, "its verdict is advisory") {
		t.Errorf("the report does not say the verdict is advisory on the branch that matters\n%s", got.stdout)
	}
}

func TestARulesetPatternThatDoesCoverTheDefaultBranchStillCounts(t *testing.T) {
	// The other direction, because a filter that rejects everything would pass
	// the test above and break every real repository.
	w := conforming()
	w.ruleset()["conditions"] = map[string]any{
		"ref_name": map[string]any{"include": []any{"refs/heads/ma*"}, "exclude": []any{}},
	}

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("a pattern covering the default branch must still count, got exit %d\n%s", got.code, got.all())
	}
}

func TestAnEvaluateModeRulesetRequiresNothing(t *testing.T) {
	// `evaluate` reports without enforcing. Counting its checks as required
	// would bless a repository whose gate is a dry run.
	w := conforming()
	rulesets := w.api["repos/"+repo+"/rulesets"].([]any)
	rulesets[0].(map[string]any)["enforcement"] = "evaluate"

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an evaluate-mode ruleset must require nothing, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "its verdict is advisory") {
		t.Errorf("the report treats a dry-run ruleset as enforcement\n%s", got.stdout)
	}
}

func TestAPermissionDemandedOnlyThroughAPresetIsStillSeen(t *testing.T) {
	// `config:best-practices` extends `config:recommended` (hence
	// `:dependencyDashboard`) and `helpers:pinGitHubActionDigests`. A config
	// whose entire body is that one preset demands issues:write and
	// workflows:write while containing neither literal — the shape a guard
	// that greps spellings can never catch.
	w := conforming()
	w.api["repos/"+repo+"/contents/renovate.json5"] = map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(`{"extends":["config:best-practices"]}`)),
	}
	w.installation("socialgouv-renovate")["permissions"] = map[string]any{"contents": "write"}

	got := run(t, w)
	if got.code == exitAgree {
		t.Fatalf("a preset-only config must still produce demands, got a full green\n%s", got.all())
	}
	for _, want := range []string{"config:best-practices extends helpers:pinGitHubActionDigests",
		"config:recommended extends :dependencyDashboard"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the report does not name the preset that demands it (%q)\n%s", want, got.stdout)
		}
	}
}

func TestAnUnexpandablePresetIsBlindOnlyWhenAPermissionIsMissing(t *testing.T) {
	// The bound that keeps the preset expansion from being an enumeration:
	// the blind spot is self-limiting. Missing permission + unknown preset =
	// unknown. Nothing missing = nothing an unknown preset could break.
	w := conforming()
	w.api["repos/"+repo+"/contents/renovate.json5"] = map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(`{"extends":["acme:house-style"]}`)),
	}

	full := run(t, w)
	if full.code != exitAgree {
		t.Fatalf("an unknown preset with every tracked permission held must not blind, got exit %d\n%s",
			full.code, full.all())
	}

	delete(w.installation("socialgouv-renovate")["permissions"].(map[string]any), "workflows")
	missing := run(t, w)
	if missing.code != exitBlind {
		t.Fatalf("an unknown preset with a permission missing must blind, got exit %d\n%s",
			missing.code, missing.all())
	}
	if !strings.Contains(missing.stdout, "acme:house-style") {
		t.Errorf("the blind spot does not name the preset it could not expand\n%s", missing.stdout)
	}
}

func TestTwoInstallationsOfTheSameScopeAreRefusedNotPicked(t *testing.T) {
	// Which App opens PRs here is then decided by the order the API happened
	// to return. A preferred scope is not an answer when two candidates share
	// it.
	w := conforming()
	ins := w.api["orgs/"+org+"/installations"].(map[string]any)
	list := ins["installations"].([]any)
	ins["installations"] = append(list, map[string]any{
		"id": 999001, "app_slug": "other-renovate",
		"repository_selection": "all",
		"permissions":          map[string]any{"contents": "write"},
	})
	// Both renovate installations now share `all`.
	w.installation("socialgouv-renovate")["repository_selection"] = "all"
	ins["total_count"] = 3

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("two same-scope renovate installations must blind, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "other-renovate") || !strings.Contains(got.stdout, "socialgouv-renovate") {
		t.Errorf("the refusal does not name both candidates\n%s", got.stdout)
	}
}

func TestASuspendedAppReddens(t *testing.T) {
	// A suspended App holds every permission it ever held and does nothing
	// with them. Asserted on BOTH apps: the same guarantee honoured on one of
	// two sites is the defect this preflight exists to catch.
	for _, slug := range []string{"socialgouv-renovate", "iterion-forge-core"} {
		t.Run(slug, func(t *testing.T) {
			w := conforming()
			w.installation(slug)["suspended_at"] = "2026-09-01T00:00:00Z"

			got := run(t, w)
			if got.code != exitBroken {
				t.Fatalf("a suspended %s must exit %d, got %d\n%s", slug, exitBroken, got.code, got.all())
			}
			if !strings.Contains(got.stdout, "SUSPENDED") {
				t.Errorf("the report does not say the App is suspended\n%s", got.stdout)
			}
		})
	}
}

func TestTheDeliveryAppScopedAwayFromTheRepositoryReddens(t *testing.T) {
	// Agreement 5 checked the delivery App's PERMISSIONS and never whether its
	// installation reaches this repository. Every alignment push would be
	// refused, with every permission line green.
	w := conforming()
	w.installation("iterion-forge-core")["repository_selection"] = "selected"
	w.api[fmt.Sprintf("user/installations/%d/repositories", deliveryApp)] = map[string]any{
		"total_count":  1,
		"repositories": []any{map[string]any{"full_name": "SocialGouv/some-other-repo"}},
	}

	got := run(t, w, "AUTO_MAINT_PAT=stand-in-token")
	if got.code != exitBroken {
		t.Fatalf("a delivery App scoped away must exit %d, got %d\n%s", exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "iterion-forge-core is repository_selection=selected") {
		t.Errorf("the report does not name the delivery App's scope\n%s", got.stdout)
	}
}

func TestArmAutomergeIsReadTheWayTheEngineCoercesIt(t *testing.T) {
	// launch_vars is a string map and arm_automerge is a bool var, so the
	// value passes through ir.CoerceVarValue: "true", "1" and "yes" all arm,
	// case-insensitively. A preflight that only knew the literal "true" would
	// call an armed repository unarmed — on the one setting this epic's
	// arbitration hangs on.
	for _, spelling := range []string{"yes", "1", "TRUE", " true "} {
		t.Run(spelling, func(t *testing.T) {
			w := conforming()
			w.launchVars()["arm_automerge"] = spelling
			w.api["repos/"+repo+"/commits/"+headSHA+"/statuses"] = []any{}

			got := run(t, w)
			if got.code != exitBroken {
				t.Fatalf("arm_automerge=%q must read as armed, got exit %d\n%s", spelling, got.code, got.all())
			}
			if !strings.Contains(got.stdout, "cannot converge") {
				t.Errorf("arm_automerge=%q was read as unarmed — the report drops the arbitration's wording\n%s",
					spelling, got.stdout)
			}
		})
	}
}

func TestJSONOutputIsTheOnlyThingOnStdout(t *testing.T) {
	// An advertised --json whose output cannot be parsed is worse than none:
	// the caller's jq fails in a way that looks like the preflight crashed.
	got := runWith(t, conforming(), opts{args: []string{"--json"}})
	if got.code != exitAgree {
		t.Fatalf("expected a green run, got %d\n%s", got.code, got.all())
	}
	var doc struct {
		Repo       string `json:"repo"`
		Broken     int    `json:"broken"`
		Blind      int    `json:"blind"`
		Agreements []struct {
			State, Agreement, Detail string
		} `json:"agreements"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("--json stdout is not parseable JSON: %v\nstdout was:\n%s", err, got.stdout)
	}
	if doc.Repo != repo || len(doc.Agreements) == 0 {
		t.Errorf("the JSON document is missing its payload: %+v", doc)
	}
}

func TestJSONKeepsADetailThatContainsTheSeparator(t *testing.T) {
	// `build | linux` is an ordinary matrix job name. Splitting the record on
	// every `|` drops the rest of the sentence in silence.
	w := conforming()
	w.setRequiredChecks([]any{map[string]any{"context": "build | linux", "integration_id": 15368}})
	w.api["repos/"+repo+"/commits/"+headSHA+"/check-runs"] = map[string]any{
		"total_count": 1,
		"check_runs":  []any{map[string]any{"name": "build | linux"}},
	}

	got := runWith(t, w, opts{args: []string{"--json"}})
	var doc struct {
		Agreements []struct{ Detail string } `json:"agreements"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("--json stdout is not parseable: %v\n%s", err, got.stdout)
	}
	found := false
	for _, a := range doc.Agreements {
		if strings.Contains(a.Detail, "build | linux") {
			found = true
		}
	}
	if !found {
		t.Errorf("a detail containing the separator was truncated; details were:\n%+v", doc.Agreements)
	}
}

func TestTheAllowlistReadsTheInlineManifestForm(t *testing.T) {
	// The flow sequence is legal on the key's own line, and a reformat would
	// produce it. A sed range would run to the NEXT `]` in the file and swallow
	// the following keys, reddening the preflight with an unactionable message.
	const inlineManifest = `forge:
  webhook:
    author_allowlist: ["dependabot[bot]", "*dependabot[bot]", "renovate[bot]", "*renovate[bot]"]
    author_scope: exclusive
    label_allowlist: [deps, security]
`
	got := runWith(t, conforming(), opts{manifest: inlineManifest})
	if got.code != exitAgree {
		t.Fatalf("the inline manifest form must parse the same, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "matches the allowlist dependabot[bot]") {
		t.Errorf("the allowlist read from the inline form is not the allowlist\n%s", got.stdout)
	}
}

func TestTheVerdictDoesNotDependOnTheWorkingDirectory(t *testing.T) {
	// The allowlist entries are bracket EXPRESSIONS to the glob engine, so an
	// unquoted split with globbing on replaces them with whatever filenames
	// match where the operator happened to be standing.
	poisoned := t.TempDir()
	for _, name := range []string{"xrenovateb", "ydependabott", "zdependabot"} {
		mustWrite(t, filepath.Join(poisoned, name), "")
	}

	got := runWith(t, conforming(), opts{dir: poisoned})
	if got.code != exitAgree {
		t.Fatalf("the verdict changed with the working directory, got exit %d\n%s", got.code, got.all())
	}
}

func TestAConfigGitHubWithheldIsBlindNotAbsent(t *testing.T) {
	// GitHub answers `content: ""` for a blob over 1 MB. Decoding nothing
	// yields no demand, and no demand would read as "nothing to disagree
	// about" — six agreements printed out of seven, and exit 0.
	w := conforming()
	w.api["repos/"+repo+"/contents/renovate.json5"] = map[string]any{"content": "", "encoding": "none"}
	w.installation("socialgouv-renovate")["permissions"] = map[string]any{"contents": "write"}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a withheld config must blind, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "came back empty") {
		t.Errorf("the blind spot does not say the config was withheld\n%s", got.stdout)
	}
}

func TestARepositoryWithNoIntegrationReddens(t *testing.T) {
	// Nothing posts a verdict: the repository is in the ruleset's world but
	// not in iterion's. Silence here would be the worst answer of all.
	w := conforming()
	w.repoBots = map[string]any{"integrations": []any{}}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a repository with no integration must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "nothing posts a verdict") {
		t.Errorf("the report does not say the repository is unwired\n%s", got.stdout)
	}
}
