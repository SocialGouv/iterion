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

// TestSelfHostedRoutingExpressionsAgree pins the six copies of one security
// decision against each other.
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
