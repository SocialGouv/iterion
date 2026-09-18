package ciguard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// workflowPath is read once, from the file the workflow actually runs from.
const workflowPath = "../../.github/workflows/tests.yml"

// runsOnLine captures the value of every job-level `runs-on:` in the file.
var runsOnLine = regexp.MustCompile(`(?m)^\s{4}runs-on:\s*(.+)$`)

// TestSelfHostedRoutingExpressionsAgree pins every copy of one security
// decision against each other. The count is deliberately NOT asserted: a job
// added without the expression is the guard's blind spot by design — what it
// pins is that no copy DIFFERS, not how many exist.
//
// `runs-on` cannot read a workflow-level `env`, and routing the choice
// through a `needs:` job would put every job behind a single GitHub-hosted
// slot — the queue the self-hosted routing exists to escape. So the
// expression is repeated per job, and repetition on a security decision is
// how one copy quietly stops matching the others: a fork guard removed from
// five jobs is loud, removed from one it is invisible.
//
// The assertion is deliberately byte equality between the copies, not a
// parse of what each means. Any difference at all — a clause dropped, a bot
// login changed, an operator lever added to five of six — is a difference
// the reviewer must see.
func TestSelfHostedRoutingExpressionsAgree(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read the workflow the jobs run from: %v", err)
	}

	seen := map[string][]string{}
	for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
		value := strings.TrimSpace(m[1])
		if !strings.Contains(value, "arc-runners") {
			continue // GitHub-hosted jobs carry a plain label, by design.
		}
		seen[value] = append(seen[value], value)
	}

	switch len(seen) {
	case 0:
		t.Fatal("no job routes to arc-runners — either the self-hosted routing was " +
			"removed (then delete this guard in the same change) or the expression " +
			"no longer names the scale set this reads for.")
	case 1:
		// One distinct expression: every copy agrees.
	default:
		t.Errorf("the %d jobs routed to arc-runners do NOT all carry the same expression, "+
			"so one security decision now has %d different answers. The fork guard and the "+
			"dependency-bot guard are only worth what their LEAST protected copy is worth.",
			total(seen), len(seen))
		for value, occurrences := range seen {
			t.Errorf("  %d copy/copies: %s", len(occurrences), value)
		}
	}
}

// TestSelfHostedRoutingKeepsItsThreeClauses asserts the expression still
// carries each guard the comment above it promises. Byte equality across
// copies (above) proves they AGREE; it cannot notice all six losing the
// same clause at once, which is exactly what a tidy-up does.
func TestSelfHostedRoutingKeepsItsThreeClauses(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read the workflow the jobs run from: %v", err)
	}

	var expression string
	for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
		if value := strings.TrimSpace(m[1]); strings.Contains(value, "arc-runners") {
			expression = value
			break
		}
	}
	if expression == "" {
		t.Skip("no self-hosted routing to check; the guard above reports it")
	}

	for _, clause := range []struct{ needle, why string }{
		{"vars.CI_SELF_HOSTED", "the operator's lever — three REQUIRED checks route here, and " +
			"repairing by merging does not work when merging is what is broken"},
		{"head.repo.full_name != github.repository", "the fork guard — a public repository, and " +
			"these runners sit inside the cluster"},
		{"renovate[bot]", "the dependency-bot guard — its author has write access, its CONTENT " +
			"is arbitrary upstream code that `pnpm install` executes"},
	} {
		if !strings.Contains(expression, clause.needle) {
			t.Errorf("the self-hosted routing no longer carries %q.\nThat clause is %s.\n"+
				"If removing it is deliberate, delete this row in the same change and say why.",
				clause.needle, clause.why)
		}
	}
}

func total(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

// requiredChecks mirrors ruleset 18857412's required_status_checks for the
// jobs THIS workflow defines. It is a literal because the ruleset lives
// outside the repository and nothing here can read it — which is exactly why
// the invariant below is worth pinning: the list drifts silently otherwise.
//
// `revi/review` is required too but is posted by a bot, not by this file.
var requiredChecks = map[string]bool{
	"test":              true,
	"race":              true,
	"vendor-check":      true,
	"mongo-conformance": true,
	"golangci":          true,
	"brand":             true,
}

// mergeGroupSkip matches the job-level `if:` that keeps an advisory job out of
// the merge queue — the STRUCTURE, not the word. A bare /merge_group/ also
// matches the prose of a comment explaining that a job deliberately has no
// skip, which is how the first version of this test accused the one job whose
// comment says the most about it.
var mergeGroupSkip = regexp.MustCompile(`(?m)^\s{4}if:.*merge_group`)

// TestEveryJobPicksASideOfTheMergeQueue holds the workflow header's
// load-bearing claim — "the jobs carrying that skip are exactly the complement
// of the ruleset's required checks" — to a test, because it was a comment and
// a comment does not hold.
//
// Both directions are a real defect, and each has happened:
//
//   - REQUIRED with a skip: a job skipped by a job-level `if:` reports
//     SUCCESS, so a required check that never ran satisfies the gate on every
//     queue entry. Silent, and it merges things nothing verified.
//   - ADVISORY without one: the job holds a slot in a queue that shares 20
//     concurrent runners across the organisation, to produce a verdict nobody
//     can act on. Measured: the `brand` job shipped that way and had to be
//     corrected in the round that followed.
func TestEveryJobPicksASideOfTheMergeQueue(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	jobs := splitJobs(string(src))
	if len(jobs) < 5 {
		t.Fatalf("parsed %d jobs from %s — the splitter no longer matches the file", len(jobs), workflowPath)
	}
	for name, body := range jobs {
		skips := mergeGroupSkip.MatchString(body)
		switch {
		case requiredChecks[name] && skips:
			t.Errorf("job %q is a REQUIRED check and skips merge_group — a skipped job reports SUCCESS, so the gate would be satisfied by a check that never ran", name)
		case !requiredChecks[name] && !skips:
			t.Errorf("job %q is advisory and does NOT skip merge_group — it holds a queue slot for a verdict nobody can act on; add `if: github.event_name != 'merge_group'`, or add it to the ruleset and to requiredChecks here", name)
		}
	}
	for name := range requiredChecks {
		if _, ok := jobs[name]; !ok {
			t.Errorf("requiredChecks names %q, which this workflow does not define — the ruleset would wait for a check that never reports", name)
		}
	}
}

// splitJobs carves the `jobs:` mapping into one body per job, keyed by name.
// Two-space indent introduces a job; anything deeper belongs to it.
func splitJobs(src string) map[string]string {
	header := regexp.MustCompile(`(?m)^  ([a-z0-9][a-z0-9-]*):\s*$`)
	idx := header.FindAllStringSubmatchIndex(src, -1)
	jobsAt := strings.Index(src, "\njobs:\n")
	out := make(map[string]string, len(idx))
	for i, m := range idx {
		if jobsAt >= 0 && m[0] < jobsAt {
			continue // a key above `jobs:` (concurrency, permissions, …)
		}
		end := len(src)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[src[m[2]:m[3]]] = src[m[0]:end]
	}
	return out
}
