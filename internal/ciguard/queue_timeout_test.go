package ciguard

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// The merge queue's patience, and the alert that is supposed to fire before it
// runs out.
//
// Why these are worth guards (#2377): a required job sat 70 minutes `queued`
// on `arc-runners` with no runner while its siblings ran, and the queue
// ejected the pull request — `checks_timed_out`, and every entry behind it
// with it. The pool had capacity; the Actions service was holding the job on a
// phantom runner. Nothing in this tree can read the ruleset, so the number the
// queue actually enforces lives here as prose in four places, and prose does
// not type-check: the guards below hold those places to one value, and hold
// the canary's own detection arithmetic to the ceiling its file claims.
const (
	mergePolicyPath    = "../../docs/merge-policy.md"
	workflowDir        = "../../.github/workflows"
	minTimeoutDeclared = 4 // tests.yml twice, merge-queue-gate.yml, docs/merge-policy.md
)

var (
	// `check_response_timeout_minutes` (60) — one spelling, so one regexp
	// harvests every site. The number may sit on the next line (a wrapped
	// Markdown or YAML comment), never further.
	queueTimeoutDecl = regexp.MustCompile("check_response_timeout_minutes`?[ \t]*\n?[ \t#>]*\\(([0-9]+)\\)")
	// The three numbers that decide how late the canary can page.
	canaryStallLine     = regexp.MustCompile(`(?m)^STALL_SECONDS=([0-9]+)\s*$`)
	canaryCronLine      = regexp.MustCompile(`(?m)^\s*- cron: "([0-9,]+) \* \* \* \*"\s*$`)
	canaryHeadStartLine = regexp.MustCompile(`(?m)^\s*sleep ([0-9]+)\s*$`)
	canaryClaimedCeil   = regexp.MustCompile(`Worst-case detection is therefore ~([0-9]+)-([0-9]+)\s*\n?#?\s*minutes`)
	// `**30 + 25 + 5 = 60 minutes**`, the arithmetic docs/merge-policy.md
	// spells out for a reader who will never run this test.
	policyArithmetic = regexp.MustCompile(`\*\*([0-9]+) \+ ([0-9]+) \+ ([0-9]+) = ([0-9]+) minutes\*\*`)
)

// timeoutDecl is one place this tree writes the queue's check-response
// timeout, kept with its position so a disagreement names the file to edit.
type timeoutDecl struct {
	where   string
	minutes int
}

// queueCheckTimeoutMinutes harvests every declaration and returns the value
// they all agree on. It fatals rather than guesses: an empty harvest (the
// field renamed, the spelling changed) would make every comparison below pass
// against nothing.
func queueCheckTimeoutMinutes(t *testing.T) int {
	t.Helper()
	decls := harvestQueueTimeoutDecls(t)
	if len(decls) < minTimeoutDeclared {
		t.Fatalf("harvested %d declaration(s) of check_response_timeout_minutes, expected at least %d — the field was renamed or the `name (number)` spelling changed; fix this guard's regexp rather than letting it compare nothing", len(decls), minTimeoutDeclared)
	}
	for _, d := range decls[1:] {
		if d.minutes != decls[0].minutes {
			t.Fatalf("%s says %d minutes and %s says %d: the queue enforces ONE number (ruleset 18857412), so every place that writes it must agree", decls[0].where, decls[0].minutes, d.where, d.minutes)
		}
	}
	return decls[0].minutes
}

func harvestQueueTimeoutDecls(t *testing.T) []timeoutDecl {
	t.Helper()
	paths := []string{mergePolicyPath}
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read the workflow directory: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yml" {
			paths = append(paths, filepath.Join(workflowDir, e.Name()))
		}
	}
	var decls []timeoutDecl
	for _, path := range paths {
		body := mustRead(t, path)
		for _, m := range queueTimeoutDecl.FindAllSubmatchIndex(body, -1) {
			minutes, err := strconv.Atoi(string(body[m[2]:m[3]]))
			if err != nil {
				t.Fatalf("%s: %q is not a number of minutes: %v", path, body[m[2]:m[3]], err)
			}
			line := 1 + bytes.Count(body[:m[0]], []byte("\n"))
			decls = append(decls, timeoutDecl{where: path + ":" + strconv.Itoa(line), minutes: minutes})
		}
	}
	return decls
}

// TestEveryDeclarationOfTheQueuesCheckTimeoutAgrees keeps the four places that
// write the queue's patience on one number.
//
// The ruleset is the authority and lives outside the repository, so each of
// these is a copy: two comments in tests.yml (the CI_SELF_HOSTED escape hatch,
// and brand's timeout budget), one in merge-queue-gate.yml (the queue branch a
// review never reports on), and the dated reading in docs/merge-policy.md. An
// admin who moves the field and updates one of them leaves three lying, and a
// lying comment about a 60-minute dequeue is how #2377 was diagnosed twice.
//
// Mutation seen red: `60` → `75` at .github/workflows/tests.yml:130 — the test
// fails naming both sites and their numbers.
func TestEveryDeclarationOfTheQueuesCheckTimeoutAgrees(t *testing.T) {
	minutes := queueCheckTimeoutMinutes(t)
	if minutes <= 0 {
		t.Fatalf("the declared check-response timeout is %d minutes", minutes)
	}
	t.Logf("every site declares check_response_timeout_minutes = %d", minutes)
}

// TestTheCanaryStallThresholdFiresBeforeTheQueueGivesUp holds the canary's
// threshold strictly under the queue's patience.
//
// A watchdog that pages after the dequeue is not an alert, it is a post-mortem:
// the documented repair for a deaf scale set is to set `CI_SELF_HOSTED` to
// `off` BEFORE the queue ejects the pull requests, which nobody can do without
// being told in time.
//
// Mutation seen red: `STALL_SECONDS=1500` → `STALL_SECONDS=5400` in
// scripts/ci/arc-canary-verdict.sh — 90 minutes of patience for a 60-minute
// queue, and this assertion fires.
func TestTheCanaryStallThresholdFiresBeforeTheQueueGivesUp(t *testing.T) {
	stall := canaryStallThresholdSeconds(t)
	timeout := queueCheckTimeoutMinutes(t) * 60
	if stall >= timeout {
		t.Errorf("the canary pages after %d s and the queue gives up after %d s: the alert must fire strictly before the dequeue, or the operator learns of the outage from the ejected pull requests (#2377). Lower STALL_SECONDS in %s, or raise check_response_timeout_minutes on ruleset 18857412 and the four places that write it", stall, timeout, canaryVerdictScript)
	}
}

func canaryStallThresholdSeconds(t *testing.T) int {
	t.Helper()
	m := canaryStallLine.FindSubmatch(mustRead(t, canaryVerdictScript))
	if m == nil {
		t.Fatalf("%s has no `STALL_SECONDS=<n>` line — the threshold moved; keep this guard in step", canaryVerdictScript)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n <= 0 {
		t.Fatalf("%s declares STALL_SECONDS=%q", canaryVerdictScript, m[1])
	}
	return n
}

// TestTheCanarysWorstCaseDetectionIsTheOneItClaims recomputes, from the three
// committed numbers that decide it, how late the canary can page — and holds
// both prose ceilings to that sum.
//
// The numbers: the longest gap between two probes (its cron), the stall
// threshold (the verdict script), and the head start the watchdog gives the
// period it judges (its `sleep`). Dispatch can therefore stop being
// trustworthy that many minutes before anyone is paged. Both
// .github/workflows/arc-canary.yml and docs/merge-policy.md state that sum to
// a reader; neither can be re-derived by one, so moving a cron mark without
// moving the prose leaves two files claiming a detection time the schedule no
// longer gives — and #2377 turns on exactly this arithmetic (60 minutes of
// worst-case detection against a 60-minute dequeue: the page and the ejection
// race, which is why the ticket's remaining gesture is one field of the
// ruleset).
//
// Mutations seen red, each on its own assertion: the cron reduced to `"7 * * *
// *"` (hourly: the claimed ceiling no longer matches); the `sleep 300` head
// start deleted; the claimed `~60-65 minutes` rewritten to `~55-60 minutes`.
func TestTheCanarysWorstCaseDetectionIsTheOneItClaims(t *testing.T) {
	canary := mustRead(t, canaryWorkflowPath)
	gap := longestGapBetweenProbes(t, canary)
	stall := canaryStallThresholdSeconds(t)
	if stall%60 != 0 {
		t.Fatalf("STALL_SECONDS=%d is not a whole number of minutes — the ceilings below are written in minutes", stall)
	}
	head := canaryHeadStartLine.FindSubmatch(canary)
	if head == nil {
		t.Fatalf("%s has no `sleep <n>` in the watchdog — the head start it gives the period it judges is part of the detection ceiling; keep this guard in step", canaryWorkflowPath)
	}
	headSeconds, err := strconv.Atoi(string(head[1]))
	if err != nil || headSeconds%60 != 0 {
		t.Fatalf("%s: the watchdog sleeps %q seconds, not a whole number of minutes", canaryWorkflowPath, head[1])
	}
	ceiling := gap + stall/60 + headSeconds/60

	claimed := canaryClaimedCeil.FindSubmatch(canary)
	if claimed == nil {
		t.Fatalf("%s no longer states `Worst-case detection is therefore ~<low>-<high> minutes` — that sentence is the claim this guard checks", canaryWorkflowPath)
	}
	low, high := atoi(t, claimed[1]), atoi(t, claimed[2])
	if low != ceiling {
		t.Errorf("%s claims worst-case detection ~%d-%d minutes, and its own numbers give %d (%d min between probes + %d min threshold + %d min of watchdog head start): move the sentence with the schedule", canaryWorkflowPath, low, high, ceiling, gap, stall/60, headSeconds/60)
	}
	if high < low {
		t.Errorf("%s claims a range ~%d-%d minutes, which is empty — the upper bound covers the watchdog's own dispatch latency and cannot be below the arithmetic", canaryWorkflowPath, low, high)
	}

	policy := mustRead(t, mergePolicyPath)
	spelled := policyArithmetic.FindSubmatch(policy)
	if spelled == nil {
		t.Fatalf("%s no longer spells the detection arithmetic as `**<gap> + <threshold> + <head start> = <sum> minutes**` — that is where a reader of the merge policy learns how late the page can be", mergePolicyPath)
	}
	for i, want := range []int{gap, stall / 60, headSeconds / 60, ceiling} {
		if got := atoi(t, spelled[i+1]); got != want {
			t.Errorf("%s spells `%s`, term %d is %d and the committed files give %d", mergePolicyPath, spelled[0], i+1, got, want)
		}
	}
	t.Logf("worst-case detection %d min (gap %d + threshold %d + head start %d) against a %d-min dequeue", ceiling, gap, stall/60, headSeconds/60, queueCheckTimeoutMinutes(t))
}

// longestGapBetweenProbes reads the canary's cron minute marks and returns the
// longest wait between two probes, in minutes — the gap that decides how long
// a stall can hide before a probe meets it.
func longestGapBetweenProbes(t *testing.T, canary []byte) int {
	t.Helper()
	// Exactly one schedule: a second `- cron:` line would add probes this
	// arithmetic never sees, and reading the first alone would overstate the
	// gap — a ceiling computed from half the schedule is not a ceiling.
	all := canaryCronLine.FindAllSubmatch(canary, -1)
	if len(all) != 1 {
		t.Fatalf("%s carries %d hourly `- cron: \"<minutes> * * * *\"` schedule(s), this guard reads exactly one — keep it in step with the file", canaryWorkflowPath, len(all))
	}
	m := all[0]
	var marks []int
	for _, field := range bytes.Split(m[1], []byte(",")) {
		marks = append(marks, atoi(t, field))
	}
	// The gap below walks the marks in order; cron does not require them to be
	// written that way.
	sort.Ints(marks)
	worst := 0
	for i, mark := range marks {
		next := 60 + marks[0]
		if i+1 < len(marks) {
			next = marks[i+1]
		}
		if gap := next - mark; gap > worst {
			worst = gap
		}
	}
	if worst <= 0 {
		t.Fatalf("%s: cron %q gives a non-positive probe interval", canaryWorkflowPath, m[1])
	}
	return worst
}

func atoi(t *testing.T, raw []byte) int {
	t.Helper()
	n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatalf("%q is not a number: %v", raw, err)
	}
	return n
}
