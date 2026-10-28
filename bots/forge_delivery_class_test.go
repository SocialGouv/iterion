package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// This file answers, mechanically, the question #2436 answered for two
// bundles and #2437 asked of the rest: of the bots that push under the
// managed forge credential, which can push a `.github/workflows/**` path,
// and what happens when one does?
//
// GitHub refuses an App installation token's push touching that directory
// outright (pkg/forge/github/app_client.go). The refusal arrives as a failed
// `git push` whose stderr says nothing about the grant, so the run dies
// untyped and an operator reads a network error. Two answers make that
// impossible, and a pusher owes exactly one of them:
//
//   - PROBE the delivery before pushing — ask the scoped preflight whether
//     THIS token may deliver THIS diff, and turn an answered denial into a
//     typed hold (bots/delivery_probe_drift_test.go pins the copies).
//   - OWN the paths delivered — declare the pathspec the push may carry and
//     refuse anything outside it, so a workflow file can never be in the
//     diff and there is nothing to ask.
//
// The class is discovered rather than listed, because a list is what went
// stale: the ticket named six bundles and the measured class is eleven.

// deliveryOwnedPathsMarker is the declaration a pathspec-limited pusher
// carries in place of the probe. It is a NAME IN THE PROGRAM, not a comment:
// the staging and the refusal both read the tuple, so narrowing it narrows
// the delivery and deleting it breaks the push rather than the prose.
const deliveryOwnedPathsMarker = "DELIVERY_OWNED_PATHS"

// gitPushSpellings are the ways a tool node of ours delivers: argv from a
// python program, or the shell's own words from a `sh` script. BOTH are read,
// and the second over-matches on purpose — these bots name `git push` in their
// error messages too, so a program that only TALKS about pushing is read as a
// pusher and owes an answer it may not need. That asymmetry is deliberate: a
// pusher missed is the defect this guard exists for, and an extra answer costs
// one declaration.
var gitPushSpellings = []string{"'git', 'push'", "git push"}

// pushesIn reports whether a compiled tool body delivers to a forge.
func pushesIn(body string) bool {
	for _, spelling := range gitPushSpellings {
		if strings.Contains(body, spelling) {
			return true
		}
	}
	return false
}

// forgeMRSkill is the bundle file an agent-driven delivery ships: the skill
// that detects the forge, authenticates its CLI from the mounted token,
// pushes the branch and opens the pull/merge request. A bundle shipping it
// hands an agent the credential and a write over the whole tree — the exact
// delivery whose diff can carry a workflow file.
const forgeMRSkill = "skills/forge-mr-create.md"

// forgeTokenSecret is the managed credential: an org binds its forge
// credential to this secret name, and the runner mounts it read-only.
const forgeTokenSecret = "forge_token"

// deliveryPush is one node that pushes, with the program the run executes.
type deliveryPush struct {
	node    string
	program string
	// owns reports whether this program DECLARES the paths its push may
	// carry — the assignment, not a mention of it.
	owns bool
}

type forgePusher struct {
	bundle string   // "feed-watch"
	rel    string   // "feed-watch/main.bot"
	probed []string // nodes carrying the delivery probe AND routed to
	// orphanProbes carry the probe and nothing routes to them: a copy that
	// passes the drift guard, runs on no path, and proves nothing.
	orphanProbes []string
	pushes       []deliveryPush
	agentMR      bool // the bundle ships the forge MR skill
}

// ownedPushes returns the pushing nodes that declare the paths they own.
func (p forgePusher) ownedPushes() []string {
	var out []string
	for _, push := range p.pushes {
		if push.owns {
			out = append(out, push.node)
		}
	}
	return out
}

func (p forgePusher) pushNodes() []string {
	var out []string
	for _, push := range p.pushes {
		out = append(out, push.node)
	}
	return out
}

// answered reports whether this pusher owes nothing more: it probes its
// delivery, or EVERY push it performs is limited to paths it declares.
func (p forgePusher) answered() bool {
	if len(p.probed) > 0 {
		return true
	}
	// A declaration answers for a PROGRAM's push. An agent holding the
	// credential and the MR skill pushes through neither, so no declaration
	// in a program can speak for it.
	if len(p.pushes) == 0 || p.agentMR {
		return false
	}
	return len(p.ownedPushes()) == len(p.pushes)
}

// unprovenForgePushers are the pushers that still owe an answer, each with
// the reason it is not answered yet. Being listed here is the ONLY way a
// pusher passes without probing or owning its paths, and the list is read
// both ways: an entry that became answerable fails as loudly as a pusher that
// joined the class unlisted. Tracked by #2437.
var unprovenForgePushers = map[string]string{
	"app-dev":            "an agent writes the whole tree and opens the pull request itself, so the preflight has no pull request to be asked about yet — tracked by #2437",
	"docs-refresh":       "same shape as app-dev: the campaign agent writes the tree, finalize_mr opens or amends the PR — tracked by #2437",
	"feature-dev":        "same shape as app-dev — tracked by #2437",
	"instrument":         "holds the token and never calls git push itself: the MR is opened by an agent through the forge MR skill — tracked by #2437",
	"product-docs":       "same shape as app-dev: an agent writes the docs tree and opens the PR — tracked by #2437",
	"whole-improve-loop": "same shape as app-dev — tracked by #2437",
}

// discoverForgePushers compiles every bundle and returns the ones that can
// deliver a diff to a forge under the managed credential.
func discoverForgePushers(t *testing.T) []forgePusher {
	t.Helper()
	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	var found []forgePusher
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil || cr.Workflow.Secrets[forgeTokenSecret] == nil {
			continue
		}
		bundle := filepath.Dir(rel)
		p := forgePusher{bundle: bundle, rel: rel}
		if _, err := os.Stat(filepath.Join(bundle, forgeMRSkill)); err == nil {
			p.agentMR = true
		}
		routedTo := map[string]bool{}
		for _, e := range cr.Workflow.Edges {
			routedTo[e.To] = true
		}
		for node, raw := range cr.Workflow.Nodes {
			tn, ok := raw.(*ir.ToolNode)
			if !ok {
				continue
			}
			body := tn.Command + "\n" + tn.Script
			if strings.Contains(body, deliveryProbeMarker) {
				// Routed to, not merely present. A probe the graph never
				// enters is a 230-line copy the drift guard keeps honest and
				// no run ever executes — green here, and the push unproven.
				if routedTo[node] || node == cr.Workflow.Entry {
					p.probed = append(p.probed, node)
				} else {
					p.orphanProbes = append(p.orphanProbes, node)
				}
			}
			if pushesIn(body) {
				// The ASSIGNMENT, never a mention: a comment naming the
				// declaration, or the loop that reads it, says nothing about
				// a program whose declaration was deleted — and that is
				// exactly what a half-reverted edit leaves behind.
				p.pushes = append(p.pushes, deliveryPush{node: node, program: body, owns: len(ownedPathsDeclarations(body)) > 0})
			}
		}
		sort.Strings(p.probed)
		sort.Strings(p.orphanProbes)
		sort.Slice(p.pushes, func(i, j int) bool { return p.pushes[i].node < p.pushes[j].node })
		if len(p.probed) == 0 && len(p.orphanProbes) == 0 && len(p.pushes) == 0 && !p.agentMR {
			continue // holds the credential to READ a forge, never to deliver
		}
		found = append(found, p)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].bundle < found[j].bundle })
	// A floor, because the other failure mode of a discriminator is to match
	// nothing and leave a vacuous guard looking green. The two carriers of
	// #2436 plus the three state-dir watchers are the minimum this repository
	// has shipped since then.
	if len(found) < 5 {
		t.Fatalf("discovered %d forge pushers, want >= 5 — the discriminators are stale: a bundle declares the %q secret and delivers in a shape none of %q, %v or %q reads",
			len(found), forgeTokenSecret, deliveryProbeMarker, gitPushSpellings, forgeMRSkill)
	}
	return found
}

// TestEveryForgePusherProvesItsDelivery is the class guard: every bundle that
// can push under the managed forge credential either probes its delivery
// permission or declares the paths its push owns. A bundle that joins the
// class answers the question or names itself in unprovenForgePushers — it
// cannot slip in silently, which is how the first six came to exist.
func TestEveryForgePusherProvesItsDelivery(t *testing.T) {
	for _, p := range discoverForgePushers(t) {
		t.Run(p.bundle, func(t *testing.T) {
			if len(p.orphanProbes) > 0 {
				t.Errorf("%s carries the delivery probe at %v and no edge routes to it — the copy passes the drift guard and no run executes it, so the push is as unproven as if the probe were absent", p.bundle, p.orphanProbes)
			}
			why, listed := unprovenForgePushers[p.bundle]
			switch {
			case p.answered() && listed:
				t.Errorf("%s answers for its delivery (probe: %v, owned paths: %v) and is still listed in unprovenForgePushers (%q) — a stale exemption tells the next reader a closed gap is open", p.bundle, p.probed, p.ownedPushes(), why)
			case p.answered():
			case listed:
				if strings.TrimSpace(why) == "" {
					t.Errorf("%s is exempt with no reason — an exemption whose reason nobody wrote is a defect nobody can close", p.bundle)
				}
			default:
				t.Errorf("%s pushes under the %q credential and proves nothing about it: probe nodes %v, pushing nodes %v declaring owned paths %v, forge MR skill %v.\nIts delivery owes one of two answers: carry the delivery probe (copy it byte for byte — see bots/delivery_probe_drift_test.go) so an answered denial becomes a typed hold, or declare the pathspec its push owns as %s and refuse everything outside it. Neither is optional: without one, a workflow-touching edit dies at the forge as a raw 403 whose cause the run never names.",
					p.bundle, forgeTokenSecret, p.probed, p.pushNodes(), p.ownedPushes(), p.agentMR, deliveryOwnedPathsMarker)
			}
		})
	}
}

// TestNoForgePusherExemptionIsPhantom closes the list's other end: a key
// naming a bundle that is not in the class — renamed, deleted, or mistyped —
// buys nothing and says a gap is open where there is no bundle at all. The
// stale-entry check in the guard above cannot see it, because it only ever
// reads the list from a bundle it discovered.
func TestNoForgePusherExemptionIsPhantom(t *testing.T) {
	inClass := map[string]bool{}
	for _, p := range discoverForgePushers(t) {
		inClass[p.bundle] = true
	}
	for bundle := range unprovenForgePushers {
		if !inClass[bundle] {
			t.Errorf("unprovenForgePushers names %q, which is not a forge pusher of this tree — drop the entry, or fix the name it was meant to spell", bundle)
		}
	}
}

// TestOwnedDeliveryPathsExcludeWorkflows reads the declaration for the two
// spellings that would void it outright — the repository root and `.github` —
// and names them where a reader can see them.
//
// It is a fast, legible early warning and NOT the proof: a tuple built from a
// variable, a join or a dirname escapes any list of spellings, and a list of
// spellings never converges. What a declaration is WORTH is proved by
// TestOwnedDeliveryPathsRefuseAPathOutsideThem, which runs the program against
// a real index holding a workflow file.
func TestOwnedDeliveryPathsExcludeWorkflows(t *testing.T) {
	for _, p := range discoverForgePushers(t) {
		for _, push := range p.pushes {
			if !push.owns {
				continue
			}
			// Read off the COMPILED program, which is what runs: a
			// declaration a fragment or a template shapes is judged here as
			// the push itself reads it.
			for _, decl := range ownedPathsDeclarations(push.program) {
				// `.github` needs no spelling of its own — it is a path under
				// the root, and the root is already refused.
				for _, bad := range []string{"'.'", "'/'", "''", "'.github'", "'.github/'"} {
					if strings.Contains(decl, bad) {
						t.Errorf("%s/%s declares %s containing %s — %s\nThe declaration stands in for the delivery probe ONLY because a .github/workflows/** path cannot be inside it; a tuple that can hold one buys the exemption and delivers the very diff the probe exists to refuse.",
							p.bundle, push.node, deliveryOwnedPathsMarker, bad, decl)
					}
				}
			}
		}
	}
}

// TestOwnedDeliveryPathsRefuseAPathOutsideThem exercises the REFUSAL rather
// than the declaration. The bot's own delivery function is extracted from the
// compiled program and run against a real checkout whose index holds a
// `.github/workflows` file: it must name that path and refuse, before a
// commit exists and long before a push.
//
// A declaration nothing enforces is a comment with a capital letter. This is
// the half that cannot be satisfied by renaming a variable.
func TestOwnedDeliveryPathsRefuseAPathOutsideThem(t *testing.T) {
	for _, p := range discoverForgePushers(t) {
		for _, push := range p.pushes {
			if !push.owns {
				continue
			}
			t.Run(p.bundle+"/"+push.node, func(t *testing.T) {
				harness, call := deliveryRefusalHarness(t, p.bundle, push)
				repo := t.TempDir()
				gittest.Run(t, repo, "init", "-q")
				if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("probe\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, repo, "add", "README.md")
				gittest.Run(t, repo, "commit", "-q", "-m", "base")
				const stateDir = ".delivery-state"
				if err := os.MkdirAll(filepath.Join(repo, stateDir), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, stateDir, "state.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				const intruder = ".github/workflows/ci.yml"
				if err := os.MkdirAll(filepath.Join(repo, ".github", "workflows"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, intruder), []byte("name: ci\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Staged by someone else — a bug in the bot, an injection
				// through its config, an operator's own index. The delivery
				// function is the thing that must not carry it.
				gittest.Run(t, repo, "add", intruder)

				program := harness + "\n" + call + "\n"
				cmd := exec.Command("python3", "-I", "-c", program) // #nosec G204 -- the program is this repository's own bot, read from the compiled bundle
				cmd.Dir = repo
				cmd.Env = gittest.Env()
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s/%s: running the extracted delivery function failed: %v\n%s\n--- program ---\n%s", p.bundle, push.node, err, out, program)
				}
				var verdict struct {
					Refused  bool     `json:"refused"`
					Said     string   `json:"said"`
					Returned []string `json:"returned"`
				}
				line := lastDeliveryVerdictLine(string(out))
				if err := json.Unmarshal([]byte(line), &verdict); err != nil {
					t.Fatalf("%s/%s: could not read the function's verdict from %q: %v", p.bundle, push.node, line, err)
				}
				switch {
				case verdict.Refused && strings.Contains(verdict.Said, intruder):
				case !verdict.Refused && containsPath(verdict.Returned, intruder):
					// The function REPORTS the offending path and its caller
					// refuses on a non-empty answer — asserted next.
					assertEveryCallerRefuses(t, p.bundle, push, callName(t, p.bundle, push))
				default:
					t.Errorf("%s/%s carried %s: the delivery function neither refused by name nor reported the path (refused=%v, said=%q, returned=%v).\nA %s declaration replaces the delivery probe only because the program refuses what the tuple does not cover; a declaration nothing enforces leaves the push to die at the forge with a raw 403.",
						p.bundle, push.node, intruder, verdict.Refused, verdict.Said, verdict.Returned, deliveryOwnedPathsMarker)
				}
			})
		}
	}
}

// pyDef matches a python function definition and captures its indentation,
// name and parameter list.
var pyDef = regexp.MustCompile(`(?m)^( *)def ([A-Za-z_][A-Za-z0-9_]*)\(([^)]*)\):`)

// deliveryRefusalHarness extracts, from a pushing program, the function that
// declares the owned paths plus the functions it calls, dedented and prefixed
// with the imports they need. It returns that source and the call that
// exercises it. A function the extraction cannot run — one carrying an
// unrendered template — ends the test rather than being skipped: a delivery
// whose refusal cannot be exercised is the gap this guard exists to close.
func deliveryRefusalHarness(t *testing.T, bundle string, push deliveryPush) (string, string) {
	t.Helper()
	blocks := pyDefBlocks(push.program)
	target := ""
	for _, name := range sortedDefNames(blocks) {
		if len(ownedPathsDeclarations(blocks[name])) > 0 {
			target = name
			break
		}
	}
	if target == "" {
		t.Fatalf("%s/%s declares %s outside every function of its program — the extraction has no delivery function to exercise", bundle, push.node, deliveryOwnedPathsMarker)
	}
	// The target plus whatever of its siblings it calls, two passes: one
	// level of helper is all these programs use, and a second pass proves it.
	needed := map[string]bool{target: true}
	for range 2 {
		for name := range blocks {
			if needed[name] {
				continue
			}
			for want := range needed {
				if strings.Contains(blocks[want], name+"(") {
					needed[name] = true
					break
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString("import json, os, re, signal, subprocess, sys, time\n")
	for _, name := range sortedDefNames(blocks) {
		if !needed[name] {
			continue
		}
		dedented := dedent(blocks[name])
		if strings.Contains(dedented, "{{") {
			t.Fatalf("%s/%s: %s carries an unrendered template, so its refusal cannot be exercised here — render it or move the template out of the delivery function", bundle, push.node, name)
		}
		b.WriteString(dedented)
		b.WriteString("\n")
	}
	return b.String(), deliveryRefusalCall(t, bundle, push, target, blocks)
}

// deliveryRefusalCall builds the call that exercises the delivery function:
// the state directory for its first parameter — the path the declaration is
// built from — and a placeholder for the rest.
func deliveryRefusalCall(t *testing.T, bundle string, push deliveryPush, target string, blocks map[string]string) string {
	t.Helper()
	m := pyDef.FindStringSubmatch(blocks[target])
	if m == nil {
		t.Fatalf("%s/%s: %s is not a function definition the extraction can read", bundle, push.node, target)
	}
	var args []string
	for i, param := range strings.Split(m[3], ",") {
		if strings.TrimSpace(param) == "" {
			continue
		}
		if i == 0 {
			args = append(args, "'.delivery-state'")
			continue
		}
		args = append(args, "'probe'")
	}
	return `
try:
    returned = ` + target + `(` + strings.Join(args, ", ") + `)
except SystemExit as exc:
    print(json.dumps({'refused': True, 'said': str(exc)}))
    raise SystemExit(0)
print(json.dumps({'refused': False, 'returned': returned if isinstance(returned, list) else [repr(returned)]}))
`
}

// assertEveryCallerRefuses closes the other half for a delivery function that
// REPORTS the paths outside the declaration instead of refusing: every call
// site in the program must raise on a non-empty answer. One that reads the
// answer and carries on is a guard that reports a breach and commits it.
func assertEveryCallerRefuses(t *testing.T, bundle string, push deliveryPush, fn string) {
	t.Helper()
	lines := strings.Split(push.program, "\n")
	sites := 0
	for i, line := range lines {
		if !strings.Contains(line, fn+"(") || strings.Contains(line, "def "+fn+"(") {
			continue
		}
		sites++
		// The caller's shape, read on the three lines a python `if` needs:
		// the answer is bound, tested, and the test raises.
		window := strings.Join(lines[i:min(i+4, len(lines))], "\n")
		if !strings.Contains(window, "raise SystemExit") {
			t.Errorf("%s/%s: %s reports the paths outside %s at line %d and this caller does not refuse on them:\n%s\nA breach reported and then committed is worse than one never looked for: the push carries the path and the log says it was checked.",
				bundle, push.node, fn, deliveryOwnedPathsMarker, i+1, window)
		}
	}
	if sites == 0 {
		t.Errorf("%s/%s: %s is never called, so nothing in this program enforces %s", bundle, push.node, fn, deliveryOwnedPathsMarker)
	}
}

// callName returns the name of the function the harness exercised.
func callName(t *testing.T, bundle string, push deliveryPush) string {
	t.Helper()
	blocks := pyDefBlocks(push.program)
	for _, name := range sortedDefNames(blocks) {
		if len(ownedPathsDeclarations(blocks[name])) > 0 {
			return name
		}
	}
	t.Fatalf("%s/%s: no function of the program declares %s", bundle, push.node, deliveryOwnedPathsMarker)
	return ""
}

// pyDefBlocks returns every python function of a program by name, each block
// running from its `def` line to the next line indented no deeper.
func pyDefBlocks(program string) map[string]string {
	lines := strings.Split(program, "\n")
	out := map[string]string{}
	for i, line := range lines {
		m := pyDef.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1])
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if len(lines[j])-len(strings.TrimLeft(lines[j], " ")) <= indent {
				end = j
				break
			}
		}
		out[m[2]] = strings.Join(lines[i:end], "\n")
	}
	return out
}

// dedent removes the common leading indentation of a block, so a nested
// function becomes a top-level one.
func dedent(block string) string {
	lines := strings.Split(block, "\n")
	indent := len(lines[0]) - len(strings.TrimLeft(lines[0], " "))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) >= indent {
			out = append(out, line[indent:])
			continue
		}
		out = append(out, strings.TrimLeft(line, " "))
	}
	return strings.Join(out, "\n")
}

// ownedPathsDeclarations returns every `DELIVERY_OWNED_PATHS = …` assignment
// in a program, one line each. The assignment, never a mention of the name.
func ownedPathsDeclarations(program string) []string {
	var out []string
	for _, line := range strings.Split(program, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, deliveryOwnedPathsMarker) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, deliveryOwnedPathsMarker))
		// `=` binds the tuple and `+=` widens it: both decide what the push
		// may carry, and reading only the first would miss the one line that
		// adds `.github` to an otherwise honest declaration. `==` is a test,
		// not a declaration.
		if (strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==")) || strings.HasPrefix(rest, "+=") {
			out = append(out, trimmed)
		}
	}
	return out
}

func sortedDefNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// lastDeliveryVerdictLine returns the last line of output that looks like a JSON object:
// the extracted function may print its own diagnostics first.
func lastDeliveryVerdictLine(out string) string {
	last := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			last = trimmed
		}
	}
	return last
}
