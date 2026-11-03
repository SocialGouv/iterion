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
	"time"
)

const (
	repo = "SocialGouv/fixture-repo"
	org  = "SocialGouv"
	// The exit codes the script's own header contracts.
	exitAgree  = 0
	exitBroken = 1
	exitBlind  = 2
	exitUsage  = 3
	// An INSTALLATION id and an APP id are two different numbers on the same
	// payload, and the whole of agreement 2 turns on which one a ruleset pins:
	// `integration_id` and a `bypass_actors` entry of actor_type Integration
	// are APP ids, while `user/installations/<id>/repositories` takes the
	// installation. These four are the values SocialGouv actually carries
	// (read 2026-10-29), so a fixture that conflated them would bless an
	// implementation that compares the wrong pair.
	renovateApp     = 149553497 // the socialgouv-renovate INSTALLATION
	renovateAppID   = 4413416   // the App behind it
	deliveryInstall = 160560029 // the iterion-forge-core INSTALLATION
	deliveryApp     = 4895793   // the App iterion posts its verdict as
	actionsApp      = 15368     // GitHub Actions' own App
	headSHA         = "aaa1111"
	renovateBot     = "socialgouv-renovate[bot]"
	scriptRelDir    = "../../scripts/auto-maintenance-check.sh"
	docRelPath      = "../../docs/auto-maintenance.md"
	// The self-hosted Renovate workflow the recipe installs, and the id of a
	// run of it. A workflow id and a run id are different id spaces, so the
	// fixture keeps them far apart: a path built from the wrong one answers
	// 404 through the stand-in gh exactly as it would through the real one.
	renovateWorkflowID = 9912001
	renovateRunID      = 41431111877
	// The instant agreement 10 measures ages from. Pinned, so a fixture dated
	// 2026-11-02 is one day old on any machine on any day, and a verdict here
	// never depends on the wall clock of the machine running the bench.
	frozenNow = "2026-11-03T12:00:00Z"
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
			// `allow_auto_merge` is the forge half of the arming guarantee:
			// `enablePullRequestAutoMerge` is refused while it is off, so the
			// conforming world is one where the forge would accept the
			// gesture agreement 7 decides the timing of.
			"repos/" + repo: map[string]any{"default_branch": "main", "allow_auto_merge": true},

			"repos/" + repo + "/rulesets": []any{
				map[string]any{"id": 1, "target": "branch", "enforcement": "active"},
			},
			// The four fields the ruleset schema requires travel on the single
			// read as they do on the real endpoint: the script asks the
			// object for `enforcement` to tell a ruleset from an error object.
			"repos/" + repo + "/rulesets/1": map[string]any{
				"id": 1, "name": "main", "source": repo, "target": "branch", "enforcement": "active",
				"bypass_actors": []any{},
				"rules": []any{map[string]any{
					"type": "required_status_checks",
					"parameters": map[string]any{"required_status_checks": []any{
						map[string]any{"context": "test", "integration_id": actionsApp},
						map[string]any{"context": "revi/review", "integration_id": deliveryApp},
					}},
				}},
			},

			// `created_at` is what agreement 10 reads when the lane runs
			// forge-side: a pull request the installed App opened WITHIN the
			// bound proves production, one from years ago proves nothing.
			"repos/" + repo + "/pulls": []any{map[string]any{
				"number":     1,
				"user":       map[string]any{"login": renovateBot, "type": "Bot"},
				"head":       map[string]any{"sha": headSHA},
				"created_at": "2026-11-01T09:00:00Z",
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
						"id": renovateApp, "app_id": renovateAppID,
						"app_slug":             "socialgouv-renovate",
						"repository_selection": "all",
						"permissions": map[string]any{
							"contents": "write", "issues": "write",
							"vulnerability_alerts": "read", "workflows": "write",
						},
					},
					map[string]any{
						"id": deliveryInstall, "app_id": deliveryApp,
						"app_slug":             "iterion-forge-core",
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
			// GitHub's own verdict per push to the default branch. The
			// conforming world is one where every sampled push PASSED the
			// ruleset — the only shape in which "every merge went through the
			// gate" is a measured fact rather than a line that cannot fail.
			"repos/" + repo + "/rulesets/rule-suites": []any{
				map[string]any{
					"id": 90001, "actor_id": 4242, "actor_name": "a-maintainer",
					"after_sha": "cafe1234cafe5678", "ref": "refs/heads/main",
					"pushed_at": "2026-10-29T08:00:00+01:00", "result": "pass",
				},
			},
			"repos/" + repo + "/actions/secrets": map[string]any{
				"total_count": 2,
				"secrets": []any{
					map[string]any{"name": "RENOVATE_APP_ID"},
					map[string]any{"name": "RENOVATE_APP_PRIVATE_KEY"},
				},
			},
			// The lane itself. Every agreement above reads a setting; the
			// conforming world is one where the self-hosted workflow exists,
			// is active, and its last COMPLETED run concluded success — the
			// only shape in which "the dependency loop works here" is a
			// measurement rather than a hope about settings.
			"repos/" + repo + "/actions/workflows": map[string]any{
				"total_count": 2,
				"workflows": []any{
					map[string]any{
						"id": 7700001, "name": "CI",
						"path": ".github/workflows/ci.yml", "state": "active",
					},
					map[string]any{
						"id": renovateWorkflowID, "name": "Renovate",
						"path": ".github/workflows/renovate.yml", "state": "active",
					},
				},
			},
			workflowRunsPath(renovateWorkflowID): map[string]any{
				"total_count":   1,
				"workflow_runs": []any{successfulRun(renovateRunID, 7)},
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

func (w *world) setRuleSuites(s []any) {
	w.api["repos/"+repo+"/rulesets/rule-suites"] = s
}

func (w *world) setBypassActors(a []any) {
	w.ruleset()["bypass_actors"] = a
}

// workflowRunsPath is the API path of one workflow's runs — the same key the
// stand-in gh derives from its argv. A workflow id and a run id live in
// different id spaces, and a path built from the wrong one 404s.
func workflowRunsPath(workflowID int) string {
	return fmt.Sprintf("repos/%s/actions/workflows/%d/runs", repo, workflowID)
}

// completedRun is one FINISHED run. `status` says the run finished and
// `conclusion` is its verdict: a fixture carrying only the first would bless a
// script that judged on it and called a failure a success.
func completedRun(id, number int, conclusion string) map[string]any {
	return map[string]any{
		"id": id, "run_number": number, "run_attempt": 1,
		"status": "completed", "conclusion": conclusion,
		"updated_at": "2026-11-02T10:45:00Z",
		"html_url":   fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id),
	}
}

func successfulRun(id, number int) map[string]any {
	return completedRun(id, number, "success")
}

func (w *world) repoPayload() map[string]any {
	return w.api["repos/"+repo].(map[string]any)
}

// setWorkflows keeps `total_count` in step with the list: the script
// cross-checks a paginated read against the total the API reports, so a
// fixture that moved one without the other would fail closed for the wrong
// reason and hide the scenario under test.
func (w *world) setWorkflows(list []any) {
	w.api["repos/"+repo+"/actions/workflows"] = map[string]any{
		"total_count": len(list), "workflows": list,
	}
}

func (w *world) setWorkflowRuns(workflowID int, runs []any) {
	w.api[workflowRunsPath(workflowID)] = map[string]any{
		"total_count": len(runs), "workflow_runs": runs,
	}
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
		"AUTO_MAINT_NOW="+frozenNow,
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
		"7 every required gate executes", "8 conforming vs bypass merge",
		"9 forge allows auto-merge", "10 the lane produced",
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

func TestARequiredCheckPinnedToAnAppThatNeverPostsReddens(t *testing.T) {
	// #1589's second half: a pin that is PRESENT is not a pin that
	// authenticates. `revi/review` pinned to GitHub Actions while iterion
	// posts it as its own App is a required check nothing the ruleset accepts
	// will ever produce — green to a reader who only asks "is there an
	// integration_id?", and a pull request that never merges in practice.
	w := conforming()
	checks := w.requiredChecks()
	for _, raw := range checks {
		m := raw.(map[string]any)
		if m["context"] == "revi/review" {
			m["integration_id"] = actionsApp
		}
	}
	w.setRequiredChecks(checks)

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a check pinned to an App that never posts it must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "producing App != pinned App") {
		t.Errorf("the report does not name the mismatch between the pinned App and the producer\n%s", got.stdout)
	}
	// Both numbers, because this message's whole job is to tell two Apps
	// apart: naming only one sends the reader hunting the other.
	for _, want := range []string{fmt.Sprint(actionsApp), fmt.Sprint(deliveryApp), "iterion-forge-core"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the mismatch does not name %q\n%s", want, got.stdout)
		}
	}
}

func TestASecondRulesetRequiringTheSameContextUnpinnedReddens(t *testing.T) {
	// GitHub ACCUMULATES required checks across active rulesets, so the same
	// context can be required twice with a different pin each time. A reader
	// that took the first row it met and stopped let the order the API
	// happened to return decide the verdict: with the pinned ruleset returned
	// first, an unpinned duplicate — which any credential satisfies — was
	// invisible. The requirement is only decidable together with its source.
	w := conforming()
	rulesets := w.api["repos/"+repo+"/rulesets"].([]any)
	w.api["repos/"+repo+"/rulesets"] = append(rulesets,
		map[string]any{"id": 2, "target": "branch", "enforcement": "active"})
	w.api["repos/"+repo+"/rulesets/2"] = map[string]any{
		"id": 2, "name": "main-too", "source": repo, "target": "branch", "enforcement": "active",
		"bypass_actors": []any{},
		"rules": []any{map[string]any{
			"type": "required_status_checks",
			"parameters": map[string]any{"required_status_checks": []any{
				map[string]any{"context": "revi/review"},
			}},
		}},
	}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an unpinned duplicate requirement must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "ruleset 2 requires 'revi/review', which carries no integration_id") {
		t.Errorf("the report does not name the ruleset whose requirement is unpinned\n%s", got.stdout)
	}
	// And the conforming row must still be reported: a verdict that replaced
	// "first row wins" with "worst row wins" would hide which ruleset is
	// already right, which is the half the operator does not have to touch.
	if !strings.Contains(got.stdout, "ruleset 1 pins 'revi/review'") {
		t.Errorf("the report drops the ruleset that IS pinned correctly\n%s", got.stdout)
	}
}

func TestAnInstallationWithoutItsAppIDBlinds(t *testing.T) {
	// The comparison's pivot. An installation payload with no `app_id` leaves
	// the App iterion posts as unknown, and the two numbers on that payload
	// are NOT interchangeable: reading the installation id instead would
	// compare an unrelated identifier to the ruleset's pin and call a
	// correctly pinned repository broken. Unknown must stay unknown.
	w := conforming()
	delete(w.installation("iterion-forge-core"), "app_id")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an installation with no app_id must blind, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "carries no app_id") {
		t.Errorf("the blind spot does not name the missing App id\n%s", got.stdout)
	}
	// It blinds ONE agreement: the pin is undecidable, the rest is not.
	if !strings.Contains(got.stdout, "7 every required gate executes") {
		t.Errorf("the blind spot aborted the agreements after it\n%s", got.stdout)
	}
}

func TestTheProducerComparedToThePinIsTheAppThatActuallyPosts(t *testing.T) {
	// The pivot of agreement 2 is DECLARED by the installation carrying the
	// delivery slug, and each tenant deployment carries its OWN
	// `iterion-forge-*` App. Comparing a correct pin to the wrong App sends
	// the operator to edit a ruleset that is already right — so when the
	// forge attributes the status to another App, that is the disagreement,
	// and the pin is not judged at all.
	w := conforming()
	w.api["repos/"+repo+"/commits/"+headSHA+"/statuses"] = []any{
		map[string]any{
			"context": "revi/review",
			"creator": map[string]any{"login": "iterion-forge-cdtn[bot]"},
		},
	}

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a producer that is not the declared App must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	for _, want := range []string{"iterion-forge-cdtn[bot]", "iterion-forge-core", "AUTO_MAINT_DELIVERY_APP"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the report does not name %q — the operator cannot act on it\n%s", want, got.stdout)
		}
	}
	// And it must NOT also blame the pin: ruleset 1 pins revi/review to the
	// delivery App correctly, and saying that is wrong would be a second,
	// false, disagreement on the same line of configuration.
	if strings.Contains(got.stdout, "producing App != pinned App") {
		t.Errorf("the pin was judged against an App that does not post it\n%s", got.stdout)
	}
}

func TestAnObservedProducerThatMatchesIsSaidToBeObserved(t *testing.T) {
	// The positive control. Without it, a cross-check that rejected EVERY
	// observed producer would pass the test above and redden every repository
	// in production — and the conforming world, whose fixture attributes no
	// creator, could not tell the two apart.
	w := conforming()
	w.api["repos/"+repo+"/commits/"+headSHA+"/statuses"] = []any{
		map[string]any{
			"context": "revi/review",
			"creator": map[string]any{"login": "iterion-forge-core[bot]"},
		},
	}

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("an observed producer matching the declared App must exit %d, got %d\n%s",
			exitAgree, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "observed posting it") {
		t.Errorf("the agreement does not say the producer was OBSERVED, so a reader cannot tell it from a declaration\n%s", got.stdout)
	}
}

func TestAnUnattributedStatusLeavesTheProducerUnobservedNotWrong(t *testing.T) {
	// A forge that reports no creator — the shape the conforming fixture
	// carries — must read as UNOBSERVED, never as a mismatch: reddening there
	// would make the agreement depend on a field the provider may omit.
	got := run(t, conforming())
	if got.code != exitAgree {
		t.Fatalf("an unattributed status must not redden, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "unobserved on the last 20 PRs") {
		t.Errorf("the agreement hides that the producer was never observed\n%s", got.stdout)
	}
}

func TestARuleSuitePayloadThatIsNotAnArrayIsABlindSpot(t *testing.T) {
	// `gh` prints an error OBJECT with exit 0 on some paths. `jq length` on an
	// object counts its keys and `.[]` walks its values, so every count in
	// agreement 8 would be a number about the wrong thing — and a repository
	// with bypassed merges would report none.
	w := conforming()
	w.setRuleSuites(nil)
	w.api["repos/"+repo+"/rulesets/rule-suites"] = map[string]any{"message": "Not Found"}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a non-array rule-suite payload must exit %d, got %d\n%s", exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "did not answer a JSON array") {
		t.Errorf("the failure does not name the wrong payload shape\n%s", got.stderr)
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
	w.api[fmt.Sprintf("user/installations/%d/repositories", deliveryInstall)] = map[string]any{
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

// --------------------------------------------------------------------------
// Agreement 8 — a conforming merge told apart from a bypass merge.
//
// The line this replaces could not fail: with an empty bypass list the
// preflight printed "every merge went through the gate" about pushes it had
// never read. GitHub keeps its own verdict per push, so the scenarios below
// are measured rather than inferred.
// --------------------------------------------------------------------------

func TestABypassedPushIsNamedAndNotCountedAsConforming(t *testing.T) {
	// The sanctioned case, and the one the live repository is in: a
	// maintainer pushes past the ruleset under the admin escape hatch the
	// project keeps on purpose. Legitimate, so it must not redden — and
	// NAMED, so the reader never reads it as a merge that passed the gate.
	w := conforming()
	w.setBypassActors([]any{
		map[string]any{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"},
	})
	w.setRuleSuites([]any{
		map[string]any{
			"id": 90002, "actor_id": 4242, "actor_name": "a-maintainer",
			"after_sha": "deadbeefdeadbeef", "ref": "refs/heads/main",
			"pushed_at": "2026-10-29T09:00:00+01:00", "result": "bypass",
		},
		map[string]any{
			"id": 90001, "actor_id": 4242, "actor_name": "a-maintainer",
			"after_sha": "cafe1234cafe5678", "ref": "refs/heads/main",
			"pushed_at": "2026-10-29T08:00:00+01:00", "result": "pass",
		},
	})

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("a bypass the ruleset sanctions must not redden, got exit %d\n%s", got.code, got.all())
	}
	for _, want := range []string{"a-maintainer", "deadbeef", "did NOT pass the gate"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the report does not name the bypass (%q missing)\n%s", want, got.stdout)
		}
	}
	// The claim that used to be printed without reading anything.
	if strings.Contains(got.stdout, "every merge went through the gate") {
		t.Errorf("the report still claims every merge passed the gate while one bypassed it\n%s", got.stdout)
	}
}

func TestASanctionedAppBypassIsMatchedByItsAppIDNotItsName(t *testing.T) {
	// A bypassing App appears in rule-suites as `<slug>[bot]` with its BOT
	// USER id (measured: tokenbureau[bot] is actor 191878105 of App
	// 1088820), while the ruleset lists the APP id. Comparing the two ids
	// directly never matches, so the bridge is the installation's slug.
	w := conforming()
	w.setBypassActors([]any{
		map[string]any{"actor_id": deliveryApp, "actor_type": "Integration", "bypass_mode": "always"},
	})
	w.setRuleSuites([]any{
		map[string]any{
			"id": 90003, "actor_id": 191878105, "actor_name": "iterion-forge-core[bot]",
			"after_sha": "f00dbabef00dbabe", "ref": "refs/heads/main",
			"pushed_at": "2026-10-29T09:00:00+01:00", "result": "bypass",
		},
	})

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("an App bypass the ruleset lists must not redden, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, fmt.Sprintf("iterion-forge-core[bot] (App %d)", deliveryApp)) {
		t.Errorf("the report does not resolve the bot login to the App id the ruleset lists\n%s", got.stdout)
	}
}

func TestAnAppBypassTheInventoryDoesNotNameReddens(t *testing.T) {
	// The inventory lists one App; another one merged past the gate. That is
	// not an escape hatch anybody sanctioned, and it is exactly what the
	// inventory-only line could never surface.
	w := conforming()
	w.setBypassActors([]any{
		map[string]any{"actor_id": deliveryApp, "actor_type": "Integration", "bypass_mode": "always"},
	})
	w.setRuleSuites([]any{
		map[string]any{
			"id": 90004, "actor_id": 777, "actor_name": "socialgouv-renovate[bot]",
			"after_sha": "abcdef0123456789", "ref": "refs/heads/main",
			"pushed_at": "2026-10-29T09:00:00+01:00", "result": "bypass",
		},
	})

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an App bypass outside the inventory must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, fmt.Sprintf("does not name Integration %d", renovateAppID)) {
		t.Errorf("the report does not say which App id the bypass list is missing\n%s", got.stdout)
	}
}

func TestAHumanBypassWithNoRoleShapedActorListedReddens(t *testing.T) {
	// The other direction, and the reason the verdict is decided on SHAPE: a
	// ruleset that lists only an Integration sanctions no human bypass, so a
	// human push past the gate is unexplained by anything the operator wrote.
	w := conforming()
	w.setBypassActors([]any{
		map[string]any{"actor_id": deliveryApp, "actor_type": "Integration", "bypass_mode": "always"},
	})
	w.setRuleSuites([]any{
		map[string]any{
			"id": 90005, "actor_id": 4242, "actor_name": "a-maintainer",
			"after_sha": "0123456789abcdef", "ref": "refs/heads/main",
			"pushed_at": "2026-10-29T09:00:00+01:00", "result": "bypass",
		},
	})

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a human bypass no listed actor explains must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "names no role-shaped actor") {
		t.Errorf("the report does not say the bypass list cannot explain a human push\n%s", got.stdout)
	}
}

func TestAnUnreadableRuleSuiteHistoryIsABlindSpotNotAPass(t *testing.T) {
	// The read needs admin on the repository, and an operator without it must
	// get "unknown" — never the absence of a bypass. It must also not abort
	// the other agreements, which is why it is blind() and not fatal().
	w := conforming()
	delete(w.api, "repos/"+repo+"/rulesets/rule-suites")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an unreadable rule-suite history must exit %d (blind spot), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "is UNKNOWN, not none") {
		t.Errorf("the blind spot lets an unreadable history read as no bypass\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "7 every required gate executes") {
		t.Errorf("the blind spot aborted the agreements around it\n%s", got.stdout)
	}
}

func TestAnEmptyRuleSuiteWindowProvesNeitherDirection(t *testing.T) {
	// No push in the window is not "every merge passed": the honest wording
	// says both halves are unproven, so nobody reads the line as a clean bill.
	w := conforming()
	w.setRuleSuites([]any{})

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("an empty window must not redden, got exit %d\n%s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "nothing is known to have passed it") {
		t.Errorf("an empty window is reported as proof of something\n%s", got.stdout)
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

func TestAPinnedClockIsPrintedWhereTheVerdictIsRead(t *testing.T) {
	// AUTO_MAINT_NOW rewrites every "within / more than N days" verdict
	// against an instant the report would otherwise never show: a replay
	// variable left in a job's environment turns a dead lane green with no
	// visible sign. So the pinned instant is on the header line and in the
	// --json document, and the wall clock says it was the wall clock.
	type clock struct {
		Now    string `json:"now"`
		Pinned bool   `json:"now_pinned"`
	}
	parse := func(t *testing.T, stdout string) clock {
		t.Helper()
		var c clock
		if err := json.Unmarshal([]byte(stdout), &c); err != nil {
			t.Fatalf("--json stdout is not parseable JSON: %v\n%s", err, stdout)
		}
		return c
	}
	t.Run("pinned", func(t *testing.T) {
		got := runWith(t, conforming(), opts{args: []string{"--json"}})
		header := "→ " + repo + " (ages measured from " + frozenNow + ", pinned by AUTO_MAINT_NOW)"
		if !strings.Contains(got.stderr, header) {
			t.Errorf("the header does not announce the pinned clock %q\n%s", header, got.stderr)
		}
		if c := parse(t, got.stdout); c.Now != frozenNow || !c.Pinned {
			t.Errorf("the --json document does not carry the pinned clock: %+v", c)
		}
	})
	t.Run("wall clock", func(t *testing.T) {
		// The verdict is not asserted here — it would depend on the day the
		// bench runs — only the provenance the document states.
		got := runWith(t, conforming(), opts{args: []string{"--json"}, env: []string{"AUTO_MAINT_NOW="}})
		if strings.Contains(got.stderr, "pinned by AUTO_MAINT_NOW") {
			t.Errorf("the wall clock is announced as pinned\n%s", got.stderr)
		}
		c := parse(t, got.stdout)
		if c.Pinned {
			t.Errorf("the --json document claims a pinned clock on the wall clock: %+v", c)
		}
		if _, err := time.Parse(time.RFC3339, c.Now); err != nil {
			t.Errorf("the --json document's now %q is not an RFC 3339 instant: %v", c.Now, err)
		}
	})
}

func TestJSONKeepsADetailThatContainsTheSeparator(t *testing.T) {
	// `build | linux` is an ordinary matrix job name. Splitting the record on
	// every `|` drops the rest of the sentence in silence.
	w := conforming()
	w.setRequiredChecks([]any{map[string]any{"context": "build | linux", "integration_id": actionsApp}})
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
	// about" — every other agreement printed, and exit 0.
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

func TestASymlinkWhereTheConfigShouldBeIsBlindNotAbsent(t *testing.T) {
	// The contents API answers a symlink or a submodule entry as an object
	// with no `content`. Filtered through `--jq '.content'` that is `null`,
	// which `base64 -d` decodes into three bytes of nothing — a config that
	// demands nothing, so no line for agreement 4 at all, in the report or in
	// the --json document, and exit 0. No premise about gh here: this is a
	// shape GitHub serves today.
	w := conforming()
	w.api["repos/"+repo+"/contents/renovate.json5"] = map[string]any{
		"type": "symlink", "name": "renovate.json5", "path": "renovate.json5",
		"target": ".github/renovate.json5", "size": 22,
	}

	got := runWith(t, w, opts{args: []string{"--json"}})
	if got.code != exitBlind {
		t.Fatalf("a symlink where the config should be must blind, got exit %d\n%s", got.code, got.all())
	}
	want := "4 renovate permissions — BLIND SPOT: repos/" + repo + "/contents/renovate.json5 did not answer a JSON file object"
	if !strings.Contains(got.stderr, want) {
		t.Errorf("the blind spot does not name the read and its shape\n%s", got.stderr)
	}
	var doc struct {
		Agreements []struct{ State, Agreement string } `json:"agreements"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("--json stdout is not parseable JSON: %v\n%s", err, got.stdout)
	}
	found := false
	for _, a := range doc.Agreements {
		if a.Agreement == "4 renovate permissions" && a.State == "blind" {
			found = true
		}
	}
	if !found {
		t.Errorf("the --json document carries no blind entry for agreement 4 — the agreement vanished instead of failing closed: %+v", doc.Agreements)
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

// --------------------------------------------------------------------------
// Agreement 9 — the forge ALLOWS the auto-merge the integration is armed for.
//
// `arm_automerge` is one half of a guarantee living in two systems. The
// preflight read that half and never asked the forge, so a repository armed to
// merge unattended on a forge whose `allow_auto_merge` is off read as fully
// configured, exited 0, and merged nothing forever.
// --------------------------------------------------------------------------

func TestTheForgeHalfOfTheArmingGuaranteeIsRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		armed   string
		allowed bool
		code    int
		want    string
	}{
		// The defect: measured FALSE on this epic's second pilot while the
		// integration recipe ends with arming.
		{"armed on a forge that forbids it", "true", false, exitBroken, "allow_auto_merge=false"},
		// The other direction, because a check that reddens on every
		// repository is not a check.
		{"armed on a forge that allows it", "true", true, exitAgree, "the forge accepts"},
		// And the one that keeps it from being permanent noise: the setting is
		// owed before arming, not today.
		{"not armed on a forge that forbids it", "false", false, exitAgree, "not owed today"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := conforming()
			w.launchVars()["arm_automerge"] = tc.armed
			w.repoPayload()["allow_auto_merge"] = tc.allowed

			got := run(t, w)
			if got.code != tc.code {
				t.Fatalf("arm_automerge=%q with allow_auto_merge=%v must exit %d, got %d\n%s",
					tc.armed, tc.allowed, tc.code, got.code, got.all())
			}
			if !strings.Contains(got.stdout, tc.want) {
				t.Errorf("the report does not say %q\n%s", tc.want, got.stdout)
			}
		})
	}
}

func TestTheArmingDisagreementNamesBothHalves(t *testing.T) {
	// Naming one half sends the operator to the wrong system: the launch var
	// is iterion's and the setting is GitHub's, and only the pair says which
	// one to change.
	w := conforming()
	w.launchVars()["arm_automerge"] = "true"
	w.repoPayload()["allow_auto_merge"] = false

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("an armed bot on a forge that forbids auto-merge must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	for _, want := range []string{"arm_automerge is on", "allow_auto_merge=false", "enablePullRequestAutoMerge"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the disagreement does not name %q — the reader cannot tell which system to change\n%s",
				want, got.stdout)
		}
	}
}

func TestAnAbsentAllowAutoMergeIsABlindSpotNotAPass(t *testing.T) {
	// `false` and "the field is missing" are different answers, and jq's `//`
	// operator cannot tell them apart — it treats `false` as absent. An
	// unanswered question is never permission.
	w := conforming()
	delete(w.repoPayload(), "allow_auto_merge")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an absent allow_auto_merge must exit %d (blind spot), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "carries no allow_auto_merge") {
		t.Errorf("the blind spot does not name the field it could not read\n%s", got.stdout)
	}
}

// --------------------------------------------------------------------------
// Agreement 10 — the dependency lane PRODUCED.
//
// Every agreement above reads a SETTING. The scenarios below are the ones
// where every setting agrees and the lane still produces nothing: a red
// scheduled run nobody receives, and a workflow GitHub disabled for
// inactivity.
// --------------------------------------------------------------------------

func TestARedDependencyLaneReddens(t *testing.T) {
	// Measured on this epic's second pilot: three consecutive scheduled runs
	// failed over five weeks (the secrets the workflow mints its token from do
	// not exist) and no agreement could see it.
	//
	// Every verdict that is not a success is the same red: the set is a
	// literal in the script, and a literal set rots one value at a time, so
	// each member is pinned here and the complementary set (cancelled,
	// skipped — no verdict at all) is pinned in
	// TestACancelledOrSkippedRunIsNotAVerdict.
	for _, concl := range []string{"failure", "timed_out", "startup_failure", "action_required"} {
		t.Run(concl, func(t *testing.T) {
			w := conforming()
			w.setWorkflowRuns(renovateWorkflowID, []any{
				completedRun(renovateRunID, 3, concl),
				successfulRun(40293907302, 2),
			})

			got := run(t, w)
			if got.code != exitBroken {
				t.Fatalf("a dependency lane whose last completed run concluded %s must exit %d, got %d\n%s",
					concl, exitBroken, got.code, got.all())
			}
			for _, want := range []string{
				".github/workflows/renovate.yml", "concluded " + concl, fmt.Sprint(renovateRunID),
			} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("the disagreement does not name %q — a red run nobody can find is a red run nobody reads\n%s",
						want, got.stdout)
				}
			}
		})
	}
}

func TestTheLastCompletedRunIsDecidedByAStableKeyNotByTheOrderOfThePage(t *testing.T) {
	// The verdict must not depend on the order the API happened to return, and
	// a NEWER run that has not finished must not hide the last completed one:
	// `.conclusion` is the verdict, and a run still going has none.
	runs := []any{
		map[string]any{
			"id": 41431119999, "run_number": 4, "run_attempt": 1,
			"status": "in_progress", "conclusion": nil,
			"updated_at": "2026-11-03T10:45:00Z",
		},
		completedRun(renovateRunID, 3, "failure"),
		successfulRun(40293907302, 2),
	}
	reversed := make([]any, len(runs))
	for i, r := range runs {
		reversed[len(runs)-1-i] = r
	}

	for name, order := range map[string][]any{"newest first": runs, "oldest first": reversed} {
		t.Run(name, func(t *testing.T) {
			w := conforming()
			w.setWorkflowRuns(renovateWorkflowID, order)

			got := run(t, w)
			if got.code != exitBroken {
				t.Fatalf("the same runs in %s order must reach the same verdict (%d), got %d\n%s",
					name, exitBroken, got.code, got.all())
			}
			if !strings.Contains(got.stdout, fmt.Sprint(renovateRunID)) {
				t.Errorf("the verdict names a different run depending on the page order\n%s", got.stdout)
			}
		})
	}
}

func TestTwoCompletedRunsSharingAKeyAndDisagreeingAreABlindSpot(t *testing.T) {
	// The agreement rule behind the stable key: where the key cannot tell two
	// completed runs apart and they disagree, picking one would be the page
	// order deciding the verdict again.
	w := conforming()
	w.setWorkflowRuns(renovateWorkflowID, []any{
		completedRun(90001, 3, "failure"),
		completedRun(90002, 3, "success"),
	})

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("two indistinguishable completed runs that disagree must exit %d, got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "failure,success") {
		t.Errorf("the blind spot does not name the two conclusions it could not choose between\n%s", got.stdout)
	}
}

func TestADisabledDependencyWorkflowReddens(t *testing.T) {
	// GitHub disables a scheduled workflow by itself after 60 days without
	// repository activity. Its last run stays green forever, which is exactly
	// why the state is read BEFORE the runs.
	w := conforming()
	w.setWorkflows([]any{map[string]any{
		"id": renovateWorkflowID, "name": "Renovate",
		"path": ".github/workflows/renovate.yml", "state": "disabled_inactivity",
	}})

	got := run(t, w)
	if got.code != exitBroken {
		t.Fatalf("a disabled dependency workflow must exit %d, got %d\n%s",
			exitBroken, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "state=disabled_inactivity") {
		t.Errorf("the disagreement does not name the state that stops the lane\n%s", got.stdout)
	}
}

func TestALaneWithNoCompletedRunAndNoProductionIsABlindSpotNotAPass(t *testing.T) {
	// A workflow that has never finished a run proves nothing in either
	// direction, and the App has opened no pull request to prove it either way.
	// The sample keeps a human PR rather than emptying: the heads are what
	// agreements 2 and 7 decide on, and an empty sample would redden those
	// instead, making this mutant's verdict attributable to the wrong thing.
	w := conforming()
	w.setWorkflowRuns(renovateWorkflowID, []any{})
	w.api["repos/"+repo+"/pulls"] = []any{map[string]any{
		"number": 1,
		"user":   map[string]any{"login": "a-maintainer", "type": "User"},
		"head":   map[string]any{"sha": headSHA},
	}}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a lane with no completed run and no production must exit %d, got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "no completed run") {
		t.Errorf("the blind spot does not say the lane has never finished a run\n%s", got.stdout)
	}
}

func TestAForgeSideLaneIsProvenByThePullRequestsTheInstalledAppOpened(t *testing.T) {
	// The bound that keeps this agreement from being permanent noise: a lane
	// running forge-side leaves no workflow in the repository, and a pull
	// request the installed App actually opened proves the production a
	// workflow run would have proven.
	w := conforming()
	w.setWorkflows([]any{map[string]any{
		"id": 7700001, "name": "CI",
		"path": ".github/workflows/ci.yml", "state": "active",
	}})

	got := run(t, w)
	if got.code != exitAgree {
		t.Fatalf("a lane proven by the App's own pull requests must exit %d, got %d\n%s",
			exitAgree, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "the lane has produced, forge-side") {
		t.Errorf("the report does not credit the production it measured\n%s", got.stdout)
	}
}

func TestAPullRequestByAnotherRenovateAppProvesNothing(t *testing.T) {
	// The org carries TWO renovate-matching Apps — the hosted `renovate`
	// (suspended) and the self-hosted one this preflight judges. A PR opened
	// by the other one is the installation-versus-App conflation in author
	// form, and it must not clear the blind spot.
	w := conforming()
	w.setWorkflows([]any{map[string]any{
		"id": 7700001, "name": "CI",
		"path": ".github/workflows/ci.yml", "state": "active",
	}})
	w.api["repos/"+repo+"/pulls"] = []any{map[string]any{
		"number": 1,
		"user":   map[string]any{"login": "renovate[bot]", "type": "Bot"},
		"head":   map[string]any{"sha": headSHA},
	}}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a PR by another renovate App must leave the lane UNKNOWN (exit %d), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "opened no pull request") {
		t.Errorf("the blind spot credits a PR the judged installation did not open\n%s", got.stdout)
	}
}

func TestAnUnreadableWorkflowListIsABlindSpotNotAPass(t *testing.T) {
	// A repository with Actions disabled answers 403 here. That blinds ONE
	// agreement — the others must still be decided, or the operator fixes one
	// thing per run.
	w := conforming()
	delete(w.api, "repos/"+repo+"/actions/workflows")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an unreadable workflow list must exit %d (blind spot), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "is UNKNOWN, not fine") {
		t.Errorf("the blind spot lets an unreadable Actions API read as a working lane\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "8 conforming vs bypass merge") {
		t.Errorf("the blind spot aborted the agreements around it\n%s", got.stdout)
	}
}

func TestAWorkflowListThatIsNotAnObjectFailsClosed(t *testing.T) {
	// `gh` prints an error OBJECT with exit 0 on some paths, and an ARRAY here
	// would make `.workflows[]?` select nothing: a repository whose lane is
	// red would read as a repository with no lane, which is the one answer
	// this agreement exists to refuse.
	w := conforming()
	w.api["repos/"+repo+"/actions/workflows"] = []any{
		map[string]any{"id": renovateWorkflowID, "path": ".github/workflows/renovate.yml"},
	}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a workflow list that is not an object must exit %d, got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "did not answer a JSON object per page") {
		t.Errorf("the failure does not name the wrong payload shape\n%s", got.stderr)
	}
}

func TestAnEmptyWorkflowListBodyFailsClosed(t *testing.T) {
	// A read that answered with nothing is not a repository with no workflow.
	// `gh` has been observed exiting 0 on a body that is not the payload asked
	// for, so the emptiness is checked before anything is concluded from it.
	w := conforming()
	w.api["repos/"+repo+"/actions/workflows"] = rawBody("")

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("an empty workflow list body must exit %d, got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "answered with an empty body") {
		t.Errorf("the failure does not say the read came back empty\n%s", got.stderr)
	}
}

func TestTheMissingHalvesOfAnUnwiredLaneAreAllNamedAtOnce(t *testing.T) {
	// The shape of a repository the recipe has not finished: no integration,
	// a selected installation this credential cannot list, no workflow
	// secrets, a red scheduled lane, and a forge that forbids the auto-merge
	// the recipe ends with. One run must name every missing half — blind()
	// exists so that a credential this script was not given blinds exactly one
	// agreement instead of aborting and hiding every disagreement after it,
	// which would have the operator fix one thing per run.
	//
	// The delivery App is left conforming on purpose: its `workflows` grant is
	// a separate, deliberately deferred decision, and mixing it in here would
	// make this scenario's reds somebody else's.
	w := conforming()
	w.repoBots = map[string]any{"integrations": []any{}}
	w.installation("socialgouv-renovate")["repository_selection"] = "selected"
	w.api["repos/"+repo+"/actions/secrets"] = map[string]any{"total_count": 0, "secrets": []any{}}
	w.repoPayload()["allow_auto_merge"] = false
	w.setWorkflowRuns(renovateWorkflowID, []any{
		completedRun(renovateRunID, 3, "failure"),
		completedRun(40293907302, 2, "failure"),
		completedRun(36404127448, 1, "failure"),
	})

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a blind spot open alongside disagreements must exit %d, got %d\n%s",
			exitBlind, got.code, got.all())
	}
	for _, want := range []string{
		"no iterion integration covers",           // 1
		"no posted context to authenticate",       // 2
		"secret RENOVATE_APP_ID is absent",        // 5
		"needs a classic PAT",                     // 6
		"allow_auto_merge=false",                  // 9
		"concluded failure on its last completed", // 10
		"cannot say the list is complete",         // the verdict
	} {
		if !strings.Contains(got.all(), want) {
			t.Errorf("one run does not name %q — the operator would fix one thing per run\n%s",
				want, got.all())
		}
	}
}

func TestAWorkflowWhoseStateTheListDoesNotCarryBlinds(t *testing.T) {
	// "disabled" is a measurement; a state the list did not carry is not one.
	// Reading the absence as a disabled workflow would claim a fact about a
	// lane that may well be firing, and reading it as active would claim the
	// opposite — so it blinds, like every other unanswered question here.
	w := conforming()
	w.setWorkflows([]any{map[string]any{
		"id": renovateWorkflowID, "name": "Renovate",
		"path": ".github/workflows/renovate.yml",
	}})

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a workflow with no state must exit %d (blind spot), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "carries no state for .github/workflows/renovate.yml") {
		t.Errorf("the blind spot does not name the workflow whose state was unsaid\n%s", got.stdout)
	}
}

// --------------------------------------------------------------------------
// Agreement 10, the recency bound. A verdict measures production only together
// with its age: the lane that cost this org a hosted Renovate service kept an
// `active` workflow and a green last run for years, and a read with no clock
// called it "producing".
// --------------------------------------------------------------------------

// staleLane is the conforming world with a lane that last ran in January 2021:
// still active, still green, silent ever since.
func staleLane() *world {
	w := conforming()
	old := successfulRun(renovateRunID, 7)
	old["updated_at"] = "2021-01-05T03:00:00Z"
	old["created_at"] = "2021-01-05T03:00:00Z"
	w.setWorkflowRuns(renovateWorkflowID, []any{old})
	return w
}

func TestAGreenVerdictOlderThanTheBoundIsALaneThatStoppedFiring(t *testing.T) {
	t.Run("under the default bound it reddens", func(t *testing.T) {
		got := run(t, staleLane())
		if got.code != exitBroken {
			t.Fatalf("a lane whose last verdict is five years old must exit %d, got %d\n%s",
				exitBroken, got.code, got.all())
		}
		for _, want := range []string{"more than 60 days ago", "stopped firing", "2021-01-05T03:00:00Z"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("the disagreement does not say %q — the operator cannot tell a stale lane from a red one\n%s",
					want, got.stdout)
			}
		}
	})
	t.Run("the bound is the operator's to move", func(t *testing.T) {
		// 2021-01-05 to the pinned clock is 2128 days: inside a 3000-day
		// bound the same world is a producing lane, so the bound is a knob
		// and not a constant the operator cannot reach.
		got := run(t, staleLane(), "AUTO_MAINT_LANE_MAX_AGE_DAYS=3000")
		if got.code != exitAgree {
			t.Fatalf("a bound the operator widened past the run's age must exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
	})
	t.Run("newer runs that reached no verdict travel with the red", func(t *testing.T) {
		// A push-guarded lane fires on every push and skips every run: the
		// red is right (nothing produced within the bound) and "stopped
		// firing" is wrong — the count of what fired since is the fact.
		w := staleLane()
		runs := []any{w.api[workflowRunsPath(renovateWorkflowID)].(map[string]any)["workflow_runs"].([]any)[0]}
		for k := 1; k <= 3; k++ {
			runs = append(runs, completedRun(renovateRunID+k, 7+k, "skipped"))
		}
		w.setWorkflowRuns(renovateWorkflowID, runs)

		got := run(t, w)
		if got.code != exitBroken {
			t.Fatalf("a stale verdict under newer skipped runs must still exit %d, got %d\n%s",
				exitBroken, got.code, got.all())
		}
		want := "more than 60 days ago; 3 newer run(s) concluded skipped — the lane fires and has reached no verdict within the bound"
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the disagreement does not say %q — the lane is reported dead while it fires\n%s", want, got.stdout)
		}
	})
	t.Run("a run in progress right now travels with the red", func(t *testing.T) {
		// The operator who just re-enabled and dispatched the lane is not
		// told it is dead while it runs: a run that has not finished is
		// counted, though it is no verdict.
		w := staleLane()
		old := w.api[workflowRunsPath(renovateWorkflowID)].(map[string]any)["workflow_runs"].([]any)[0]
		live := map[string]any{
			"id": renovateRunID + 1, "run_number": 8, "run_attempt": 1,
			"status": "in_progress", "conclusion": nil,
			"created_at": "2026-11-03T11:57:00Z", "updated_at": "2026-11-03T11:58:00Z",
		}
		w.setWorkflowRuns(renovateWorkflowID, []any{live, old})

		got := run(t, w)
		if got.code != exitBroken {
			t.Fatalf("a stale verdict with a run in progress must still exit %d, got %d\n%s",
				exitBroken, got.code, got.all())
		}
		want := "more than 60 days ago; 1 run(s) not yet completed — the lane fires and has reached no verdict within the bound"
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the disagreement does not say %q — the lane is reported dead while it runs\n%s", want, got.stdout)
		}
	})
	t.Run("a bound that is not a number of days is a usage error", func(t *testing.T) {
		got := run(t, staleLane(), "AUTO_MAINT_LANE_MAX_AGE_DAYS=sixty")
		if got.code != exitUsage {
			t.Fatalf("a malformed bound must exit %d rather than be replaced by the default, got %d\n%s",
				exitUsage, got.code, got.all())
		}
	})
	t.Run("a clock that is not an instant is a usage error", func(t *testing.T) {
		got := run(t, staleLane(), "AUTO_MAINT_NOW=yesterday")
		if got.code != exitUsage {
			t.Fatalf("a malformed clock must exit %d rather than fall back to the wall clock, got %d\n%s",
				exitUsage, got.code, got.all())
		}
	})
}

func TestAVerdictWithoutATimestampBlinds(t *testing.T) {
	// An age needs a date, and a run the forge dated with nothing readable has
	// none. Reading it as recent would be the stale lane again; reading it as
	// stale would redden a lane that may well be firing. Unknown blinds — and
	// a timestamp that is not even a string is the same unknown, answered
	// with this script's own exit code rather than a jq death on string
	// concatenation outside the 0/1/2/3 contract.
	for name, stamp := range map[string]any{
		"absent":       nil,
		"not a string": map[string]any{"oops": 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := conforming()
			undated := successfulRun(renovateRunID, 7)
			if stamp == nil {
				delete(undated, "updated_at")
			} else {
				undated["updated_at"] = stamp
			}
			w.setWorkflowRuns(renovateWorkflowID, []any{undated})

			got := run(t, w)
			if got.code != exitBlind {
				t.Fatalf("a verdict run with no readable timestamp must exit %d, got %d\n%s",
					exitBlind, got.code, got.all())
			}
			if !strings.Contains(got.stdout, "carries no readable timestamp") {
				t.Errorf("the blind spot does not say the date could not be read\n%s", got.stdout)
			}
		})
	}
}

func TestARunListWhoseEntriesAreNotRunsFailsClosed(t *testing.T) {
	// `expect_list` proves `workflow_runs` is an array; what the array holds
	// is proven here. An entry that is not an object kills the verdict read
	// inside jq, and a jq death is not an exit code this script contracts —
	// so the read is caught and answered as the fail-closed exit, naming the
	// workflow whose run list could not be judged.
	w := conforming()
	w.api[workflowRunsPath(renovateWorkflowID)] = map[string]any{
		"total_count": 1, "workflow_runs": []any{42},
	}

	got := run(t, w)
	if got.code != exitBlind {
		t.Fatalf("a run list whose entries are not runs must fail closed (exit %d), got %d\n%s",
			exitBlind, got.code, got.all())
	}
	if !strings.Contains(got.stderr, "BLIND SPOT: the run list of .github/workflows/renovate.yml did not read as workflow runs") {
		t.Errorf("the failure does not name the run list that could not be judged\n%s", got.stderr)
	}
}

func TestAForgeSideLaneIsProvenOnlyByAPullRequestWithinTheBound(t *testing.T) {
	// A hosted service that stopped org-wide leaves no workflow to read and a
	// pull request history that never empties: on a quiet repository a 2021
	// pull request is still "one of the last 20". Within the bound it proves
	// production; outside it, or without a readable date, it proves nothing.
	appPR := func(createdAt string) map[string]any {
		pr := map[string]any{
			"number": 1,
			"user":   map[string]any{"login": renovateBot, "type": "Bot"},
			"head":   map[string]any{"sha": headSHA},
		}
		if createdAt != "" {
			pr["created_at"] = createdAt
		}
		return pr
	}
	for _, tc := range []struct {
		name      string
		createdAt string
		code      int
		want      string
	}{
		{"opened yesterday", "2026-11-02T09:00:00Z", exitAgree, "within the last 60 days (newest #1 (2026-11-02T09:00:00Z))"},
		{"opened in 2021", "2021-01-05T03:00:00Z", exitBlind, "the newest #1 (2021-01-05T03:00:00Z), prove nothing about today"},
		{"opened on no readable date", "", exitBlind, "1 carry no readable created_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := conforming()
			w.setWorkflows([]any{map[string]any{
				"id": 7700001, "name": "CI",
				"path": ".github/workflows/ci.yml", "state": "active",
			}})
			w.api["repos/"+repo+"/pulls"] = []any{appPR(tc.createdAt)}

			got := run(t, w)
			if got.code != tc.code {
				t.Fatalf("a forge-side lane whose only pull request was %s must exit %d, got %d\n%s",
					tc.name, tc.code, got.code, got.all())
			}
			if !strings.Contains(got.stdout, tc.want) {
				t.Errorf("the report does not say %q\n%s", tc.want, got.stdout)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Every list read asserts the shape the endpoint contracts.
// --------------------------------------------------------------------------

func TestAnErrorObjectServedWithExitZeroFailsClosedOnEveryRead(t *testing.T) {
	// `gh` prints GitHub's error object with exit 0 on some paths. It is valid
	// JSON and it is an object, so it passed every guard that asked only that;
	// `.workflows[]?` over it yields nothing, and a list never read became an
	// empty list — on the runs read, the App's own pull request (which the
	// conforming world carries) then turned that nothing into a green. The
	// object reads had the same hole with a worse shape: `.rules[]?` over it
	// emptied the ruleset the list had just named as active, and
	// `--jq '.content'` printed `null`, which decoded to a config demanding
	// nothing — agreement 4 vanished from the report and from --json rather
	// than reddening. Every read now asks for the shape its endpoint
	// contracts; a list read decides several agreements and fails the run
	// closed, a file read decides one and blinds it, and both name the path.
	errObj := map[string]any{
		"message": "Not Found", "documentation_url": "https://docs.github.com/rest", "status": "404",
	}
	type site struct {
		path  string
		blind string // the one agreement blinded, or "" when the whole run fails closed
	}
	for _, site := range []site{
		{"orgs/" + org + "/installations", ""},
		{"repos/" + repo + "/rulesets", ""},
		{"repos/" + repo + "/rulesets/1", ""},
		{"repos/" + repo + "/pulls", ""},
		{"repos/" + repo + "/commits/" + headSHA + "/check-runs", ""},
		{"repos/" + repo + "/commits/" + headSHA + "/statuses", ""},
		{"repos/" + repo + "/actions/secrets", ""},
		{"repos/" + repo + "/actions/workflows", ""},
		{workflowRunsPath(renovateWorkflowID), ""},
		{"repos/" + repo + "/contents/renovate.json5", "4 renovate permissions"},
		{"repos/" + repo + "/contents/.github/dependabot.yml", "3 author allowlist"},
	} {
		t.Run(site.path, func(t *testing.T) {
			w := conforming()
			w.api[site.path] = errObj

			got := run(t, w)
			if got.code != exitBlind {
				t.Fatalf("an error object on %s must fail closed (exit %d), got %d\n%s",
					site.path, exitBlind, got.code, got.all())
			}
			named := site.path + " did not answer a JSON"
			if site.blind == "" {
				if !strings.Contains(got.stderr, "BLIND SPOT: "+named) {
					t.Errorf("the failure does not name the read that answered the wrong shape\n%s", got.stderr)
				}
			} else if !strings.Contains(got.stdout, site.blind+" — BLIND SPOT: "+named) {
				t.Errorf("agreement %q is not blinded by name on the read that answered the wrong shape\n%s",
					site.blind, got.stdout)
			}
		})
	}

	t.Run("the repo-bots read", func(t *testing.T) {
		// The other producer of the same class: an error body from iterion
		// is an object too, and `.integrations[]?` over it read as a
		// repository nobody wired — a red with the wrong diagnosis.
		w := conforming()
		w.repoBots = errObj

		got := run(t, w)
		if got.code != exitBlind {
			t.Fatalf("an error object from the repo-bots read must fail closed (exit %d), got %d\n%s",
				exitBlind, got.code, got.all())
		}
		if !strings.Contains(got.stderr, "the repo-bots read (cat ") ||
			!strings.Contains(got.stderr, "did not answer a JSON object carrying 'integrations' as a list") {
			t.Errorf("the failure does not name the read and the key it lacked\n%s", got.stderr)
		}
	})
}

// --------------------------------------------------------------------------
// Agreement 10, which workflow IS the lane.
// --------------------------------------------------------------------------

const validatorWorkflowID = 7700055

func validatorWorkflow() map[string]any {
	return map[string]any{
		"id": validatorWorkflowID, "name": "Renovate config validator",
		"path": ".github/workflows/renovate-config-validator.yml", "state": "active",
	}
}

func laneWorkflow() map[string]any {
	return map[string]any{
		"id": renovateWorkflowID, "name": "Renovate",
		"path": ".github/workflows/renovate.yml", "state": "active",
	}
}

func ciWorkflow() map[string]any {
	return map[string]any{
		"id": 7700001, "name": "CI", "path": ".github/workflows/ci.yml", "state": "active",
	}
}

func TestAWorkflowThatMerelyMentionsRenovateIsInventoryNotTheLane(t *testing.T) {
	// Renovate's own docs recommend a config-validator workflow next to the
	// lane. Judging every workflow with "renovate" in its name rendered two
	// contradictory sentences under one label — and the rule "any of them
	// succeeded" would have let a green validator hide a red lane. The lane is
	// the workflow at the recipe's path or one named Renovate; the rest is
	// inventory, printed so a lane under an unknown spelling stays visible.
	const inventory = "inventory — names Renovate but is not judged as the lane: .github/workflows/renovate-config-validator.yml"
	withValidator := func(validator, lane string) *world {
		w := conforming()
		w.setWorkflows([]any{ciWorkflow(), laneWorkflow(), validatorWorkflow()})
		w.setWorkflowRuns(validatorWorkflowID, []any{completedRun(55500001, 3, validator)})
		w.setWorkflowRuns(renovateWorkflowID, []any{completedRun(renovateRunID, 7, lane)})
		return w
	}

	t.Run("a red validator next to a green lane is green", func(t *testing.T) {
		got := run(t, withValidator("failure", "success"))
		if got.code != exitAgree {
			t.Fatalf("a failing config validator next to a producing lane must exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
		if !strings.Contains(got.stdout, inventory) {
			t.Errorf("the validator is not printed as inventory — a lane under an unknown spelling would vanish silently\n%s", got.stdout)
		}
		if n := strings.Count(got.stdout, "10 the lane produced"); n != 1 {
			t.Errorf("agreement 10 rendered %d sentences, want exactly one — the agreement answers its own question once\n%s", n, got.stdout)
		}
	})
	t.Run("a green validator next to a red lane is still red", func(t *testing.T) {
		got := run(t, withValidator("success", "failure"))
		if got.code != exitBroken {
			t.Fatalf("a passing config validator must not hide a failing lane: want exit %d, got %d\n%s",
				exitBroken, got.code, got.all())
		}
		if !strings.Contains(got.stdout, ".github/workflows/renovate.yml concluded failure") {
			t.Errorf("the disagreement does not name the lane that failed\n%s", got.stdout)
		}
	})
	t.Run("a validator alone is never the lane", func(t *testing.T) {
		// The forge-side shape with a validator kept in the repository: the
		// hosted service is what produces, and its pull request is the proof.
		w := conforming()
		w.setWorkflows([]any{ciWorkflow(), validatorWorkflow()})
		w.setWorkflowRuns(validatorWorkflowID, []any{completedRun(55500001, 3, "success")})

		got := run(t, w)
		if got.code != exitAgree {
			t.Fatalf("a validator alone must leave the verdict to the App's pull requests: want exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
		if !strings.Contains(got.stdout, "the lane has produced, forge-side") {
			t.Errorf("the production was not credited to the pull request\n%s", got.stdout)
		}
		if strings.Contains(got.stdout, "renovate-config-validator.yml concluded") {
			t.Errorf("the validator's run was judged as the lane's\n%s", got.stdout)
		}
	})
}

func TestTheLaneIsNamedByItsPathOrItsNameOrTheOperator(t *testing.T) {
	named := func(name, path string) map[string]any {
		return map[string]any{"id": renovateWorkflowID, "name": name, "path": path, "state": "active"}
	}

	t.Run("a workflow named Renovate on another path is the lane", func(t *testing.T) {
		w := conforming()
		w.setWorkflows([]any{ciWorkflow(), named("Renovate", ".github/workflows/deps.yml")})

		got := run(t, w)
		if got.code != exitAgree {
			t.Fatalf("the lane under another file name must be judged: want exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
		if !strings.Contains(got.stdout, ".github/workflows/deps.yml concluded SUCCESS") {
			t.Errorf("deps.yml was not judged as the lane\n%s", got.stdout)
		}
	})
	t.Run("the operator's path wins over the recipe's", func(t *testing.T) {
		// The recipe's file is red and the operator says the lane is
		// elsewhere: the explicit choice is honoured, the recipe's file
		// becomes inventory, and nothing second-guesses the operator.
		const depsID = 7700077
		w := conforming()
		w.setWorkflows([]any{ciWorkflow(), laneWorkflow(), map[string]any{
			"id": depsID, "name": "Dependency bot", "path": ".github/workflows/deps.yml", "state": "active",
		}})
		w.setWorkflowRuns(renovateWorkflowID, []any{completedRun(renovateRunID, 7, "failure")})
		w.setWorkflowRuns(depsID, []any{completedRun(77000001, 2, "success")})

		got := run(t, w, "AUTO_MAINT_LANE_WORKFLOW=.github/workflows/deps.yml")
		if got.code != exitAgree {
			t.Fatalf("the lane the operator named is green and must decide: want exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
		if !strings.Contains(got.stdout, "not judged as the lane: .github/workflows/renovate.yml") {
			t.Errorf("the recipe's file was not demoted to inventory\n%s", got.stdout)
		}
	})
	t.Run("a path the operator names and the repository lacks is a disagreement", func(t *testing.T) {
		got := run(t, conforming(), "AUTO_MAINT_LANE_WORKFLOW=.github/workflows/nope.yml")
		if got.code != exitBroken {
			t.Fatalf("an operator-named lane the repository does not carry must exit %d, not fall back to the default rule; got %d\n%s",
				exitBroken, got.code, got.all())
		}
		if !strings.Contains(got.stdout, "carries no workflow at that path") {
			t.Errorf("the disagreement does not say the named path is absent\n%s", got.stdout)
		}
	})
}

// --------------------------------------------------------------------------
// Agreement 10, what counts as a verdict.
// --------------------------------------------------------------------------

func TestACancelledOrSkippedRunIsNotAVerdict(t *testing.T) {
	// `cancelled` is what a `concurrency: cancel-in-progress` lane does to its
	// own older run and `skipped` what a job-level `if:` does. Neither says
	// the lane produced or failed, so neither reddens: alone they are the
	// workflow read saying nothing, answered like every such arm by the
	// App's own pull requests within the bound — silence on both sides
	// blinds; above an older verdict they are looked past, counted and named.
	for _, concl := range []string{"cancelled", "skipped"} {
		t.Run(concl+" alone, with no pull request within the bound, blinds", func(t *testing.T) {
			w := conforming()
			w.setWorkflowRuns(renovateWorkflowID, []any{completedRun(renovateRunID, 7, concl)})
			w.api["repos/"+repo+"/pulls"].([]any)[0].(map[string]any)["created_at"] = "2021-01-05T03:00:00Z"

			got := run(t, w)
			if got.code != exitBlind {
				t.Fatalf("a lane whose only completed run was %s, with no production to show, must exit %d, got %d\n%s",
					concl, exitBlind, got.code, got.all())
			}
			for _, want := range []string{
				"neither a production nor a failure", "all concluded " + concl,
				"AUTO_MAINT_RUN_SAMPLE widens the sample",
				"the newest #1 (2021-01-05T03:00:00Z), prove nothing about today",
			} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("the blind spot does not say %q\n%s", want, got.stdout)
				}
			}
		})
	}

	t.Run("a sample of skipped runs is credited by the pull request the App opened within the bound", func(t *testing.T) {
		// A push-guarded lane on a busy repository: twenty skipped pushes in
		// two days fill the whole bounded sample and the weekly verdict has
		// fallen out of it — while the App's pull request of two days ago is
		// in hand. The blind is a false one; the shared evidence answers it.
		w := conforming()
		runs := make([]any, 0, 20)
		for k := 0; k < 20; k++ {
			r := completedRun(renovateRunID+k, 8+k, "skipped")
			r["updated_at"] = fmt.Sprintf("2026-11-0%dT%02d:00:00Z", 1+k%2, k)
			runs = append(runs, r)
		}
		w.setWorkflowRuns(renovateWorkflowID, runs)

		got := run(t, w)
		if got.code != exitAgree {
			t.Fatalf("a skipped sample next to a fresh pull request by the App must exit %d, got %d\n%s",
				exitAgree, got.code, got.all())
		}
		for _, want := range []string{
			"the 20 completed run(s) of .github/workflows/renovate.yml among the last 20 all concluded skipped",
			"opened 1 of the last 20 pull request(s) within the last 60 days",
			"the lane has produced, forge-side",
		} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("the agreement does not say %q\n%s", want, got.stdout)
			}
		}
	})

	newerFirst := []any{completedRun(renovateRunID+1, 8, "skipped"), successfulRun(renovateRunID, 7)}
	olderFirst := []any{successfulRun(renovateRunID, 7), completedRun(renovateRunID+1, 8, "skipped")}
	for name, order := range map[string][]any{"newest first": newerFirst, "oldest first": olderFirst} {
		t.Run("a newer skipped run is looked past to the last verdict, "+name, func(t *testing.T) {
			w := conforming()
			w.setWorkflowRuns(renovateWorkflowID, order)

			got := run(t, w)
			if got.code != exitAgree {
				t.Fatalf("a skipped run above a green verdict must leave the lane green (exit %d), got %d\n%s",
					exitAgree, got.code, got.all())
			}
			if !strings.Contains(got.stdout, "1 newer run(s) concluded skipped") {
				t.Errorf("the looked-past run is not counted and named\n%s", got.stdout)
			}
		})
	}

	t.Run("a newer cancelled run does not hide a red verdict", func(t *testing.T) {
		w := conforming()
		w.setWorkflowRuns(renovateWorkflowID, []any{
			completedRun(renovateRunID+1, 8, "cancelled"),
			completedRun(renovateRunID, 7, "failure"),
		})

		got := run(t, w)
		if got.code != exitBroken {
			t.Fatalf("a cancelled run above a failure must leave the lane red (exit %d), got %d\n%s",
				exitBroken, got.code, got.all())
		}
	})
}
