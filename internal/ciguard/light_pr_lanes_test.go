package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const reviewAndMergePath = "../../docs/agents/workflow/review-and-merge.md"

// minDiscoveryBound is the least -list-timeout a CI test discovery may pass.
// Discovery is the cold -race compile of the package's test binary on an ARC
// pod with no Go cache: test-e2e's five-minute bound killed it 4 times in 96
// jobs, and 5 of 14 sampled green jobs took within a minute of it. The run
// that follows reuses the compile, so the bound costs no wall time on success.
const minDiscoveryBound = 10 * time.Minute

var (
	discoveryCall = regexp.MustCompile(`testshards\b[^\n]*?\s-tests[ =](\S+)[^\n]*`)
	listTimeout   = regexp.MustCompile(`-list-timeout[ =]([0-9a-z.]+)`)
)

// TestTestDiscoveryBoundCoversColdCompile reads every by-name discovery the
// workflows run and requires an explicit bound of at least
// minDiscoveryBound: a bound under the cold compile turns a required check
// red on healthy code, and a red queue entry is ejected. The bound stays
// finite and non-zero, so a hung discovery still reddens its shard.
func TestTestDiscoveryBoundCoversColdCompile(t *testing.T) {
	calls := 0
	for _, path := range discoveryWorkflows(t) {
		n, problems := discoveryProblems(t, path, string(mustRead(t, path)))
		calls += n
		for _, p := range problems {
			t.Error(p)
		}
	}
	if calls < 1 {
		t.Fatalf("found %d by-name discoveries, want at least test-e2e's", calls)
	}
	if !strings.Contains(string(mustRead(t, mergePolicyPath)), "CI passes ten minutes") {
		t.Errorf("%s does not state the discovery bound CI passes (ten minutes)", mergePolicyPath)
	}
}

// discoveryWorkflows returns every workflow file, so a discovery added to any
// workflow is judged, not only one in the merge queue's files.
func discoveryWorkflows(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"../../.github/workflows/*.yml", "../../.github/workflows/*.yaml"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		t.Fatalf("no workflow file under ../../.github/workflows")
	}
	return paths
}

// discoveryProblems returns how many by-name discoveries text runs and one
// message per discovery whose -list-timeout is missing or under
// minDiscoveryBound.
func discoveryProblems(t *testing.T, path, text string) (int, []string) {
	t.Helper()
	var problems []string
	matches := discoveryCall.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		bound := listTimeout.FindStringSubmatch(m[0])
		if bound == nil {
			problems = append(problems, fmt.Sprintf("%s: discovery of %s passes no -list-timeout, so it inherits the five-minute default: %s", path, m[1], m[0]))
			continue
		}
		d, err := time.ParseDuration(bound[1])
		if err != nil {
			t.Fatalf("%s: discovery of %s: -list-timeout %q: %v", path, m[1], bound[1], err)
		}
		if d < minDiscoveryBound {
			problems = append(problems, fmt.Sprintf("%s: discovery of %s is bounded at %s, under the %s a cold -race compile on ARC needs", path, m[1], d, minDiscoveryBound))
		}
	}
	return len(matches), problems
}

// TestDiscoveryBoundGuardReachesEveryCall pins the guard's reach: a discovery
// whose -tests flag follows other flags, or spelled -tests=, is still judged,
// and every workflow file is read, not only the merge queue's.
func TestDiscoveryBoundGuardReachesEveryCall(t *testing.T) {
	for _, line := range []string{
		"x=$(go run ./scripts/ci/testshards -n 4 -i 0 -tests ./pkg/runtime -list-timeout 1m)",
		"x=$(go run ./scripts/ci/testshards -n 4 -tests=./pkg/runtime -list-timeout=1m)",
		"x=$(go run ./scripts/ci/testshards -tests ./pkg/runtime -n 4 -list-timeout 1m)",
	} {
		n, problems := discoveryProblems(t, "scratch.yml", line+"\n")
		if n != 1 || len(problems) != 1 || !strings.Contains(problems[0], "bounded at 1m0s") {
			t.Errorf("discovery %q: %d calls, problems %q; want one call bounded at 1m0s", line, n, problems)
		}
	}
	read := strings.Join(discoveryWorkflows(t), "\n")
	for _, want := range []string{"tests.yml", "merge-queue-gate.yml", "test-quarantine.yml"} {
		if !strings.Contains(read, want) {
			t.Errorf("discovery guard does not read %s; it reads:\n%s", want, read)
		}
	}
}

// mdSection returns the markdown section opened by the first heading that
// starts with prefix, up to the next heading of the same or a higher level,
// with its whitespace runs folded to one space so a phrase reads the same
// across a line wrap. A `#` line inside a fenced code block is no heading.
func mdSection(t *testing.T, path, prefix string) string {
	t.Helper()
	lines := strings.Split(string(mustRead(t, path)), "\n")
	start, level, fenced := -1, 0, false
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if hashes == 0 || !strings.HasPrefix(line[hashes:], " ") {
			continue
		}
		if start < 0 {
			if strings.HasPrefix(line, prefix) {
				start, level = i, hashes
			}
			continue
		}
		if hashes <= level {
			return strings.Join(strings.Fields(strings.Join(lines[start:i], "\n")), " ")
		}
	}
	if start < 0 {
		t.Fatalf("%s: no heading %q", path, prefix)
	}
	return strings.Join(strings.Fields(strings.Join(lines[start:], "\n")), " ")
}

// TestLightGreenIsNotAdminMergeable pins the consequence of a light pull
// request's green `test` and `race`: the heavy legs never ran, and an admin
// merge also gives up the queue's rebuild against the entries ahead of it, so
// it must not read that green as proof. Both aggregators say so in the notice
// of a forgiven skip, and each documented admin lane requires the `ci:full`
// run of the heavy legs, read as success and not skipped, before it merges.
func TestLightGreenIsNotAdminMergeable(t *testing.T) {
	var wf struct{ Jobs map[string]map[string]any }
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	aggregators := 0
	for name, job := range wf.Jobs {
		env, _ := job["env"].(map[string]any)
		if env[lightRunEnv] != lightRunValue {
			continue
		}
		aggregators++
		needs, _ := job["needs"].([]any)
		vars := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), lightRunEnv + "=true", prCIEnv + "=run"}
		for k, v := range env {
			for _, l := range needs {
				if v == fmt.Sprintf("${{ needs.%v.result }}", l) {
					result := "success"
					if heavyLegs[fmt.Sprint(l)] {
						result = "skipped"
					}
					vars = append(vars, k+"="+result)
				}
			}
		}
		steps, _ := job["steps"].([]any)
		var output strings.Builder
		for _, s := range steps {
			st, _ := s.(map[string]any)
			run, _ := st["run"].(string)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", run)
			cmd.Dir, cmd.Env = t.TempDir(), vars
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("aggregator %q is red on a light pull request with its heavy legs skipped: %v\n%s", name, err, out)
			}
			output.Write(out)
		}
		if !strings.Contains(output.String(), "not admin-mergeable") {
			t.Errorf("aggregator %q turns green on a light pull request without saying that green is not admin-mergeable:\n%s", name, output.String())
		}
	}
	if aggregators != 2 {
		t.Fatalf("found %d aggregators reading %s, want test and race", aggregators, lightRunEnv)
	}

	// The policy's two lanes state the rule in full; the agent page, held to
	// a byte budget, requires `ci:full` and links the parked gate for the rest.
	full := []string{"`ci:full`", "`race-shard (n)`", "`test-e2e (n)`", "not `skipped`"}
	lanes := []struct {
		path, heading string
		wants         []string
	}{
		{mergePolicyPath, "### A parked gate", full},
		{mergePolicyPath, "### The fast lane for an incident fix", full},
		{reviewAndMergePath, "## A parked gate is a rerouting signal", []string{"`ci:full`", "merge-policy.md#a-parked-gate-decided-2026-11-10"}},
	}
	for _, lane := range lanes {
		section := mdSection(t, lane.path, lane.heading)
		for _, want := range lane.wants {
			if !strings.Contains(section, want) {
				t.Errorf("%s %q admits an admin merge without requiring %s: a light pull request's green test/race never ran the heavy legs", lane.path, lane.heading, want)
			}
		}
		if strings.Contains(section, "nothing else") {
			t.Errorf("%s %q says the bypass gives up nothing else, but on a light pull request it also gives up the heavy legs", lane.path, lane.heading)
		}
	}
}

// TestCiFullGuidanceNamesEveryOptInJob requires the policy's `ci:full`
// guidance to name every job a light pull request skips, so an author whose
// change only one of them witnesses (cloud-e2e for charts and the cloud
// stack, mongo-conformance for the Mongo store) knows to opt in.
func TestCiFullGuidanceNamesEveryOptInJob(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatal(err)
	}
	section := mdSection(t, mergePolicyPath, "## Light runs, heavy main")
	var guidance string
	for _, sentence := range strings.Split(section, ". ") {
		if strings.Contains(sentence, "add the `ci:full` label") {
			guidance = sentence
		}
	}
	if guidance == "" {
		t.Fatalf("%s gives no `ci:full` guidance", mergePolicyPath)
	}
	optIn := 0
	for name, job := range wf.Jobs {
		if !strings.Contains(job.If, fullSuite) {
			continue
		}
		optIn++
		if !strings.Contains(guidance, "`"+name+"`") {
			t.Errorf("the `ci:full` guidance never names %q, which a light pull request skips:\n%s", name, guidance)
		}
	}
	if optIn < 4 {
		t.Fatalf("found %d jobs gated on ci:full, want race-shard, test-e2e, cloud-e2e and mongo-conformance", optIn)
	}
}

const pushToMainPath = "../../docs/agents/workflow/push-to-main.md"

// TestEveryAdminLandingOwesTheCIGateOnAnEmptyQueue holds each page that
// opens an admin landing to the operator's two rules of 2026-11-25 (#3686).
// A light pull request's green never ran race-shard or test-e2e, so every
// admin landing runs `task ci:gate`, whose members carry that work, rather
// than `task check`. And a push to main outside the queue rebuilds every
// group in flight, so every admin landing but an incident fix waits for an
// empty queue. The incident fast lane owes the gate and opens its pull
// request with `ci:full`, so its heavy legs run on the first push.
func TestEveryAdminLandingOwesTheCIGateOnAnEmptyQueue(t *testing.T) {
	const (
		gate      = "`task ci:gate`"
		emptyRule = "empty queue"
	)
	lanes := []struct {
		path, heading string
		wants         []string
	}{
		{mergePolicyPath, "### The extended train lane", []string{gate, "no entry in flight", "fixed forward", "`chore: release`", "`\"next\"` floor", "reading-golden lines"}},
		{mergePolicyPath, "### Queue hygiene", []string{emptyRule, "dispatch"}},
		{mergePolicyPath, "### A parked gate", []string{gate, "entry in flight"}},
		{mergePolicyPath, "### The fast lane for an incident fix", []string{gate, "opened with the `ci:full` label"}},
		{reviewAndMergePath, "## The operator direct-landing lane", []string{gate, emptyRule}},
		{reviewAndMergePath, "## A parked gate is a rerouting signal", []string{gate, emptyRule}},
		{pushToMainPath, "## Review, integrate, push, witness", []string{gate, emptyRule}},
	}
	for _, lane := range lanes {
		section := mdSection(t, lane.path, lane.heading)
		for _, want := range lane.wants {
			if !strings.Contains(section, want) {
				t.Errorf("%s %q does not say %q: an admin landing runs `task ci:gate` (a light pull request never ran the heavy legs) and, an incident fix aside, waits for an empty queue", lane.path, lane.heading, want)
			}
		}
	}
	// Three rules live in a paragraph rather than a section: push-to-main.md's
	// parked-gate opening, before its first `##`, review-and-merge.md's "One
	// lane per burst", and merge-policy.md's summary of the direct-landing
	// contract. Each is read up to its blank line, so the older `task ci:gate`
	// and "empty queue" mentions around them (the direct-landing paragraph,
	// the lane's own item 1, queue hygiene) cannot stand in for them. The
	// summary must not keep "warn sessions with queued PRs": queued PRs make
	// the push wait.
	paragraphs := []struct {
		path, opening string
		wants, bans   []string
	}{
		{pushToMainPath, "A **parked `revi/review`**", []string{gate, "`ci:full`", emptyRule}, nil},
		{reviewAndMergePath, "**One lane per burst**", []string{gate, emptyRule}, nil},
		{mergePolicyPath, "Operator-led chantiers may bypass the queue", []string{"wait for an empty queue"}, []string{"queued PRs"}},
	}
	for _, p := range paragraphs {
		_, rest, found := strings.Cut(string(mustRead(t, p.path)), "\n"+p.opening)
		if !found {
			t.Fatalf("%s has no paragraph opening with %q", p.path, p.opening)
		}
		paragraph, _, _ := strings.Cut(p.opening+rest, "\n\n")
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		for _, want := range p.wants {
			if !strings.Contains(paragraph, want) {
				t.Errorf("%s's paragraph %q does not say %q", p.path, p.opening, want)
			}
		}
		for _, ban := range p.bans {
			if strings.Contains(paragraph, ban) {
				t.Errorf("%s's paragraph %q still says %q: queued PRs make an admin landing wait, they are not a warning duty", p.path, p.opening, ban)
			}
		}
	}
}
