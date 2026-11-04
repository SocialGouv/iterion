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
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// This file answers, mechanically, the question #2436 answered for two
// bundles and #2437 asked of the rest: of the bots that push under the
// managed forge credential, which can push a `.github/workflows/**` path,
// and what happens when one does?
//
// GitHub refuses an App installation token's push touching that directory
// outright (pkg/forge/github/app_client.go). The refusal arrives as a failed
// `git push` whose stderr says nothing about the grant, so the run dies
// untyped and an operator reads a network error. Three answers make that
// impossible, and a pusher owes exactly one of them:
//
//   - PROBE the delivery before pushing — ask the scoped preflight whether
//     THIS token may deliver THIS diff, and turn an answered denial into a
//     typed hold (bots/delivery_probe_drift_test.go pins the copies).
//   - OWN the paths delivered — declare the pathspec the push may carry and
//     refuse anything outside it, so a workflow file can never be in the
//     diff and there is nothing to ask.
//   - GATE the delivery — where the push is an AGENT's (it holds the
//     credential and the MR skill, so no declaration in a program can speak
//     for it), a deterministic node declaring the owned paths stands on
//     every route to that agent and ends the run TYPED when a path outside
//     them would be delivered. What makes it an answer and not a decoration
//     is dominance plus a typed refusal, both proved below: a gate the
//     delivery can be reached without, or one whose exit is not a declared
//     `fail`, buys nothing.
//
// The class is discovered rather than listed, because a list is what went
// stale: the ticket named six bundles and the measured class is eleven.

// deliveryOwnedPathsMarker is the declaration a pathspec-limited pusher — or
// the gate standing in front of an agent's delivery — carries in place of the
// probe. It is a NAME IN THE PROGRAM, not a comment: the staging and the
// refusal both read the tuple (a gate's refusal reads it alone, having
// nothing to stage), so narrowing it narrows the delivery and deleting it
// breaks the push rather than the prose.
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
	// gates are the deterministic nodes that declare the owned paths for a
	// delivery performed by somebody ELSE: they push nothing themselves, and
	// the graph turns their verdict into a typed refusal instead of a push.
	gates []deliveryPush
	// orphanGates declare the paths and no edge routes to them — a rule the
	// run never evaluates, which is the same as no rule.
	orphanGates []string
	// deliveries are the nodes whose declared job is to deliver the diff: a
	// program that pushes, and the agent that opens the pull request itself.
	deliveries []string
	// undominated are the deliveries still reachable from the entry once
	// every gate's verdict edge is cut: routes that deliver without the
	// gate's answer. Empty is what makes a gate worth the exemption.
	undominated []string
	// untypedRefusals are the gates whose own exit is not a declared `fail`
	// node with a code: the run ends, and the operator reads the engine's
	// generic FAIL_NODE instead of the paths that were refused.
	untypedRefusals []string
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

// ownedPrograms are the programs whose DECLARATION stands in for the probe —
// read by the two tests that judge a declaration: the pushes that declare
// the paths they themselves carry, and the gates that declare them for an
// agent's delivery.
func (p forgePusher) ownedPrograms() []deliveryPush {
	var out []deliveryPush
	for _, push := range p.pushes {
		if push.owns {
			out = append(out, push)
		}
	}
	return append(out, p.gates...)
}

// gateNodes returns the gate node ids.
func (p forgePusher) gateNodes() []string {
	var out []string
	for _, gate := range p.gates {
		out = append(out, gate.node)
	}
	return out
}

// gatesAnswer reports whether the bundle's gates answer for its delivery: at
// least one gate, a delivery for it to stand in front of, every route to
// that delivery crossing a gate's verdict, and a typed refusal on the other
// side. Any one of those missing leaves the question open.
func (p forgePusher) gatesAnswer() bool {
	return len(p.gates) > 0 && len(p.deliveries) > 0 &&
		len(p.orphanGates) == 0 && len(p.undominated) == 0 && len(p.untypedRefusals) == 0
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
	// A gate speaks for a delivery its own program does not perform — the
	// agent case — but only while it dominates that delivery and refuses
	// typed: the two conditions gatesAnswer reads.
	if p.gatesAnswer() {
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
// pusher passes without probing, owning its paths or gating its delivery,
// and the list is read both ways: an entry that became answerable fails as
// loudly as a pusher that joined the class unlisted.
//
// The four that remain are the ones whose delivery CANNOT be bounded by a
// pathspec: they write code across the tree on purpose, which is their job.
// Their answer is the preflight widened to a (repo, credential) pair — asked
// before any pull request exists — under epic #1409. docs-refresh and
// product-docs left this list the day their delivery gates landed (#2437):
// a documentation campaign owns a pathspec, so it can declare one.
var unprovenForgePushers = map[string]string{
	"app-dev":            "an agent writes the whole tree and opens the pull request itself, so the preflight has no pull request to be asked about yet; its delivery is code across the tree, which no pathspec bounds — epic #1409",
	"feature-dev":        "same shape as app-dev — epic #1409",
	"instrument":         "holds the token and never calls git push itself: the MR is opened by an agent through the forge MR skill, and its delivery legitimately carries CI instrumentation, so no narrow owned set exists — epic #1409",
	"whole-improve-loop": "same shape as app-dev — epic #1409",
}

// pullRequestContract is the output contract of the node whose declared job
// is to deliver: whether it opened a pull request, its address, and the
// branch it pushed. The discriminator is this contract and not the prompt,
// because every one of these bundles NAMES the forge MR skill in its
// campaign prompt as well — to forbid the campaign from pushing — so prose
// reads the loop body as a delivery, and a gate in the PR tail could never
// be proven to dominate a node inside the loop it follows.
var pullRequestContract = []string{"opened", "url", "branch"}

// opensAPullRequest reports whether a node's output schema answers the
// pull-request contract.
func opensAPullRequest(wf *ir.Workflow, schema string) bool {
	s := wf.Schemas[schema]
	if s == nil {
		return false
	}
	have := map[string]bool{}
	for _, f := range s.Fields {
		have[f.Name] = true
	}
	for _, want := range pullRequestContract {
		if !have[want] {
			return false
		}
	}
	return true
}

// gateVerdict judges a bundle's delivery gates on the two properties that
// make one an answer rather than a decoration.
//
// DOMINANCE, read on the REFUSAL and never on the node. "The gate runs
// before the delivery" is not the question — in both documentary bundles the
// writeable-set check already ran before the PR tail and the delivery
// shipped anyway, so a node-reachability check passes with no fix at all.
// The question is whether every route to the delivery crosses the gate's
// VERDICT: every out-edge of a gate that carries a condition is CUT, and the
// delivery must then be unreachable from the entry. A gate that accepts
// unconditionally, a gate the graph can bypass, a second edge into the
// delivery — each leaves it reachable, and each is named here.
//
// A TYPED REFUSAL, because the other side of the verdict is all the operator
// gets: the gate's unconditional (and `else`) exits must land on a declared
// `fail` node carrying a code. The bare `fail` target, `done`, or anything
// onward into the tail ends the run with the engine's generic FAIL_NODE — or
// with no refusal at all.
func gateVerdict(wf *ir.Workflow, gates, deliveries []string) (undominated, untyped []string) {
	if len(gates) == 0 {
		return nil, nil
	}
	isGate := map[string]bool{}
	for _, g := range gates {
		isGate[g] = true
	}
	refusals := map[string]int{}
	var kept []*ir.Edge
	for _, e := range wf.Edges {
		if isGate[e.From] && (e.Condition != "" || e.Expression != nil) {
			continue // the verdict edge: cut, so what survives is every route that does NOT read it
		}
		kept = append(kept, e)
		if !isGate[e.From] {
			continue
		}
		refusals[e.From]++
		if fail, ok := wf.Nodes[e.To].(*ir.FailNode); !ok || strings.TrimSpace(fail.Code) == "" {
			untyped = append(untyped, e.From+" -> "+e.To)
		}
	}
	for _, g := range gates {
		if refusals[g] == 0 {
			untyped = append(untyped, g+" -> (nothing: the node has no unconditional exit, so it declares no refusal)")
		}
	}
	reached := map[string]bool{wf.Entry: true}
	for queue := []string{wf.Entry}; len(queue) > 0; {
		from := queue[0]
		queue = queue[1:]
		for _, e := range kept {
			if e.From != from || reached[e.To] {
				continue
			}
			reached[e.To] = true
			queue = append(queue, e.To)
		}
	}
	for _, d := range deliveries {
		if reached[d] {
			undominated = append(undominated, d)
		}
	}
	sort.Strings(undominated)
	sort.Strings(untyped)
	return undominated, untyped
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
			if an, ok := raw.(*ir.AgentNode); ok {
				// The agent whose CONTRACT is to open the pull request. Read
				// off the output schema, never off the prompt: a campaign
				// prompt names the forge MR skill too — to forbid it — so
				// prose would read the loop body as a delivery and no gate
				// could ever dominate it.
				if opensAPullRequest(cr.Workflow, an.OutputSchema) {
					p.deliveries = append(p.deliveries, node)
				}
				continue
			}
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
			// The ASSIGNMENT, never a mention: a comment naming the
			// declaration, or the loop that reads it, says nothing about
			// a program whose declaration was deleted — and that is
			// exactly what a half-reverted edit leaves behind.
			owns := len(ownedPathsDeclarations(body)) > 0
			switch {
			case pushesIn(body):
				p.pushes = append(p.pushes, deliveryPush{node: node, program: body, owns: owns})
				p.deliveries = append(p.deliveries, node)
			case owns:
				// A declaration in a program that does NOT push is a GATE:
				// it bounds a delivery performed further down the graph.
				p.gates = append(p.gates, deliveryPush{node: node, program: body, owns: true})
				if !routedTo[node] && node != cr.Workflow.Entry {
					p.orphanGates = append(p.orphanGates, node)
				}
			}
		}
		sort.Strings(p.probed)
		sort.Strings(p.orphanProbes)
		sort.Strings(p.orphanGates)
		sort.Strings(p.deliveries)
		sort.Slice(p.pushes, func(i, j int) bool { return p.pushes[i].node < p.pushes[j].node })
		sort.Slice(p.gates, func(i, j int) bool { return p.gates[i].node < p.gates[j].node })
		p.undominated, p.untypedRefusals = gateVerdict(cr.Workflow, p.gateNodes(), p.deliveries)
		if len(p.probed) == 0 && len(p.orphanProbes) == 0 && len(p.pushes) == 0 && len(p.gates) == 0 && !p.agentMR {
			continue // holds the credential to READ a forge, never to deliver
		}
		// A bundle that hands an agent the MR skill HAS a delivery node, and
		// the dominance question is asked about it. None found means the
		// contract above no longer reads the node that opens the pull
		// request — and a dominance check with nothing to dominate is the
		// vacuous green this file exists to refuse.
		if p.agentMR && len(p.deliveries) == 0 {
			t.Fatalf("%s ships %s and no node of it answers the pull-request contract %v — the delivery node is undiscoverable, so no gate can be proven to dominate it",
				bundle, forgeMRSkill, pullRequestContract)
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
// permission, declares the paths its push owns, or gates the delivery an
// agent performs for it. A bundle that joins the class answers the question
// or names itself in unprovenForgePushers — it cannot slip in silently,
// which is how the first six came to exist.
func TestEveryForgePusherProvesItsDelivery(t *testing.T) {
	for _, p := range discoverForgePushers(t) {
		t.Run(p.bundle, func(t *testing.T) {
			if len(p.orphanGates) > 0 {
				t.Errorf("%s declares %s at %v and no edge routes to it — a rule the run never evaluates is no rule, and the delivery is as unbounded as if the gate were absent", p.bundle, deliveryOwnedPathsMarker, p.orphanGates)
			}
			if len(p.orphanProbes) > 0 {
				t.Errorf("%s carries the delivery probe at %v and no edge routes to it — the copy passes the drift guard and no run executes it, so the push is as unproven as if the probe were absent", p.bundle, p.orphanProbes)
			}
			why, listed := unprovenForgePushers[p.bundle]
			switch {
			case p.answered() && listed:
				t.Errorf("%s answers for its delivery (probe: %v, owned paths: %v, gates: %v) and is still listed in unprovenForgePushers (%q) — a stale exemption tells the next reader a closed gap is open", p.bundle, p.probed, p.ownedPushes(), p.gateNodes(), why)
			case p.answered():
			case listed:
				if strings.TrimSpace(why) == "" {
					t.Errorf("%s is exempt with no reason — an exemption whose reason nobody wrote is a defect nobody can close", p.bundle)
				}
			default:
				t.Errorf("%s pushes under the %q credential and proves nothing about it: probe nodes %v, pushing nodes %v declaring owned paths %v, delivery gates %v (undominated deliveries %v, untyped refusals %v), forge MR skill %v.\nIts delivery owes one of three answers: carry the delivery probe (copy it byte for byte — see bots/delivery_probe_drift_test.go) so an answered denial becomes a typed hold; declare the pathspec its push owns as %s and refuse everything outside it; or, where an agent performs the delivery, gate that delivery with a deterministic node declaring %s whose refusal is a typed `fail` and which dominates every route to the delivery. None is optional: without one, a workflow-touching edit dies at the forge as a raw 403 whose cause the run never names — and any other out-of-scope path is delivered without a word.",
					p.bundle, forgeTokenSecret, p.probed, p.pushNodes(), p.ownedPushes(), p.gateNodes(), p.undominated, p.untypedRefusals, p.agentMR, deliveryOwnedPathsMarker, deliveryOwnedPathsMarker)
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

// TestDeliveryGatesDominateTheDelivery is the half of the gate answer that a
// declaration cannot buy: a rule the delivery can be reached without, or one
// whose refusal is untyped, exempts a bundle while delivering exactly the
// diff the probe exists to refuse.
//
// Both properties are read in gateVerdict, on the graph the compiler
// produced. Mutations that reddens it: put back a direct
// `forge_auth_probe -> finalize_mr when available` edge (the delivery becomes
// reachable without the verdict); make the accept edge unconditional
// (nothing is cut, so the delivery is reachable, and the edge is read as an
// exit landing on a node that is not a `fail`); point the gate's bare exit at
// `done` or at the bare `fail` (the refusal loses its code).
func TestDeliveryGatesDominateTheDelivery(t *testing.T) {
	gated := 0
	for _, p := range discoverForgePushers(t) {
		if len(p.gates) == 0 {
			continue
		}
		gated++
		t.Run(p.bundle, func(t *testing.T) {
			if len(p.deliveries) == 0 {
				t.Fatalf("%s carries delivery gates %v and no delivery node was discovered — a gate in front of nothing is not an answer", p.bundle, p.gateNodes())
			}
			if len(p.undominated) > 0 {
				t.Errorf("%s (%s): cutting the verdict edges of %v still leaves %v reachable from the workflow entry — the delivery can happen without the gate's answer, so the declaration bounds nothing.\nEvery route into the delivery must cross the gate: put the gate on the ONLY in-edge of the delivery node, and guard the edge that proceeds on the gate's own field.",
					p.bundle, p.rel, p.gateNodes(), p.undominated)
			}
			if len(p.untypedRefusals) > 0 {
				t.Errorf("%s: the gate's refusal is not typed: %v.\nThe gate's unconditional exit must land on a declared `fail <name>:` node carrying a `code:` — the bare `fail` target ends the run as the engine's generic FAIL_NODE, and `done` ends it as a success. What an operator reads of a refused delivery is that code and the message beside it.",
					p.bundle, p.untypedRefusals)
			}
		})
	}
	// A floor: the two documentary campaigns are the gated bundles this
	// repository ships. Zero means the gate discriminator (a declaration in a
	// program that does not itself push) no longer reads them, and every
	// assertion above would be vacuously green.
	if gated < 2 {
		t.Fatalf("discovered %d gated deliveries, want >= 2 — a tool node declaring %s without pushing is no longer read as a gate", gated, deliveryOwnedPathsMarker)
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
		for _, push := range p.ownedPrograms() {
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
		for _, push := range p.ownedPrograms() {
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

				// A declaration built from the environment is EMPTY without
				// it, and an empty owned set refuses every path: the
				// intruder would then be refused by VACUITY rather than by
				// the rule, and no mutation of the rule could redden this.
				assertDeliveryEnvIsComplete(t, p.bundle, push.node, harness)
				program := harness + "\n" + call + "\n"
				cmd := exec.Command("python3", "-I", "-c", program) // #nosec G204 -- the program is this repository's own bot, read from the compiled bundle
				cmd.Dir = repo
				cmd.Env = append(gittest.Env(), deliveryRefusalEnvPairs()...)
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

// deliveryRefusalEnv is the environment the extracted delivery functions are
// built from. A declaration read out of the environment is EMPTY without it
// (product-docs' owned set is its validated `product_dir`), and an empty
// owned set reports every changed path outside — so the fixture's intruder
// would be refused by VACUITY, on a branch no mutation of the rule can
// redden. The prefix rule ITSELF is proved where the whole gate runs against
// a real product tree (bots/delivery_gate_test.go, "another product's
// tree"); what this file owes is that the class question is not asked of a
// rule switched off.
//
// The tree-noise channel is the value the ENGINE provisions on every tool
// process, for the same reason and against the ambient one gittest.Env()
// inherits.
var deliveryRefusalEnv = map[string]string{
	"PRODUCT_DIR":             "docs/produit-x",
	"EDITORIAL_DIR":           ".product-docs",
	treenoise.TreeNoiseEnvVar: treenoise.EnvValue(),
}

// deliveryRefusalEnvPairs renders deliveryRefusalEnv as exec's env, sorted so
// the command a failure prints is reproducible.
func deliveryRefusalEnvPairs() []string {
	keys := make([]string, 0, len(deliveryRefusalEnv))
	for k := range deliveryRefusalEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+deliveryRefusalEnv[k])
	}
	return pairs
}

// pyEnvRead matches a python read of the process environment by name.
var pyEnvRead = regexp.MustCompile(`os\.environ(?:\.get)?[(\[]['"]([A-Z_][A-Z0-9_]*)['"]`)

// assertDeliveryEnvIsComplete ends the test when the extracted source reads a
// variable deliveryRefusalEnv gives no value: the next bundle whose
// declaration is built from a new one cannot land a subtest that is green
// because its owned set was empty.
func assertDeliveryEnvIsComplete(t *testing.T, bundle, node, source string) {
	t.Helper()
	for _, m := range pyEnvRead.FindAllStringSubmatch(source, -1) {
		if _, ok := deliveryRefusalEnv[m[1]]; !ok {
			t.Fatalf("%s/%s: the extracted delivery function reads $%s and deliveryRefusalEnv gives it no value — the declaration it builds would be empty here, so the intruder is refused by vacuity and no mutation of the rule can redden this subtest. Give the variable the value a run gives it.", bundle, node, m[1])
		}
	}
}

// deliveryRefusalBaseParams are the first-parameter names that mean "the
// commit this delivery is measured against" rather than "the directory the
// declaration is built from" — the ONE convention a gate adds to the call
// below. A gate reads the paths a push WOULD carry, so its parameter is a
// git base; `HEAD` makes the committed half empty and leaves the index and
// the untracked tree, which is where the fixture plants the intruder.
//
// One convention, not an extraction that hoists module globals: everything
// else the function needs it reads from its own environment, which is the
// shape prod-watch's `_staged_outside` already has (one parameter,
// self-sufficient, returning the offending paths).
var deliveryRefusalBaseParams = map[string]bool{"base": true, "base_sha": true}

// deliveryRefusalCall builds the call that exercises the delivery function:
// the state directory for its first parameter — the path the declaration is
// built from, or a git base where the parameter names one — and a
// placeholder for the rest.
func deliveryRefusalCall(t *testing.T, bundle string, push deliveryPush, target string, blocks map[string]string) string {
	t.Helper()
	m := pyDef.FindStringSubmatch(blocks[target])
	if m == nil {
		t.Fatalf("%s/%s: %s is not a function definition the extraction can read", bundle, push.node, target)
	}
	var args []string
	for i, param := range strings.Split(m[3], ",") {
		name := strings.TrimSpace(param)
		if name == "" {
			continue
		}
		if i == 0 {
			if deliveryRefusalBaseParams[name] {
				args = append(args, "'HEAD'")
			} else {
				args = append(args, "'.delivery-state'")
			}
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
