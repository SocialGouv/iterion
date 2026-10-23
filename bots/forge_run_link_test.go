package bots

import (
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// A bot that writes on a forge publishes the address of the run that wrote
// it — the reader's way to the steps, the models and the cost behind the
// text, and the way into the platform for someone meeting it on their own
// repository (AGENTS.md, docs/agents/bots/run-link.md).
//
// Two channels satisfy it, and both are read from the COMPILED workflow,
// never from the file's text: a `##` comment naming `{{run.url}}` is not a
// channel, and a guard that accepted one would be green over a bot that
// publishes nothing.
//
//   - `{{run.url}}` — the run-namespace member the engine resolves from the
//     deployment's public origin (pkg/runtime, run.url). The form every new
//     bot uses.
//   - the publish endpoint's own origin — review-pr and gitops-warden
//     rebuild `/studio/runs/<id>` from `forge_publish_url`, which predates
//     the member and stays truthful (same deployment, same page). Read from
//     the text on purpose: it is a string those bots assemble in python.
const (
	runLinkStudioChannel = "/studio/runs/"
	// The footer every forge-facing bot ends its body with. Pinned as one
	// string so the fleet publishes one shape.
	runLinkFooterShape = "🤖 [iterion](https://iterion.cloud) · run [`<run id>`](<run url>)"
	// The two halves that keep it honest: no address → no link, and a
	// caller-chosen run id never lands raw in the label.
	runLinkNoHostRule = "Never substitute a host"
	runLinkIDRule     = "CALLER-CHOSEN at launch"
)

// forgeFacingBots are the bundles whose run posts text on a forge: the
// server publishes for them (their `.bot` names forge_publish_url), or they
// post through the forge API themselves (they ship a `skills/forge-*.md`).
// The expected membership is pinned below, so a bundle that LEAVES the class
// — a renamed marker, a deleted skill — reddens instead of quietly dropping
// out of every check.
var forgeFacingExpected = []string{
	"app-dev", "branch-improve-loop", "dep-update-guard", "docs-refresh",
	"feature-dev", "gitops-warden", "instrument", "product-docs",
	"revi-converse", "review-pr", "supply-shield", "supply-shield-cve",
	"whole-improve-loop",
}

// postingSkills returns the forge-posting skills a bundle ships.
func postingSkills(t *testing.T, dir string) []string {
	t.Helper()
	skills, err := filepath.Glob(filepath.Join(dir, "skills", "forge-*.md"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(skills)
	return skills
}

func forgeFacingBots(t *testing.T) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	mains, err := filepath.Glob("*/main.bot")
	if err != nil || len(mains) == 0 {
		t.Fatalf("bot bundle discovery broke: %v (%d bundles)", err, len(mains))
	}
	for _, main := range mains {
		dir := filepath.Dir(main)
		var sources []string
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".bot") {
				sources = append(sources, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
		sort.Strings(sources)
		facing := len(postingSkills(t, dir)) > 0
		for _, src := range sources {
			if strings.Contains(string(readFileOrFail(t, src)), "forge_publish_url") {
				facing = true
			}
		}
		if facing {
			found[dir] = sources
		}
	}
	var got []string
	for dir := range found {
		got = append(got, dir)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(forgeFacingExpected, ",") {
		t.Fatalf("the forge-facing class is now %v, pinned as %v — a bundle that joined must carry the run link and be added here; one that left must have stopped posting on a forge", got, forgeFacingExpected)
	}
	return found
}

func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// usedPrompts are the prompts some NODE references (ir.NodePromptRefs is the
// IR's own enumeration: an LLM node's system/user, an interaction prompt, a
// router's, a human node's). A prompt no node names is dead text the engine
// never renders, so it cannot be a bot's channel — a guard counting it would
// be green over a bundle that publishes nothing.
func usedPrompts(wf *ir.Workflow) map[string]bool {
	used := map[string]bool{}
	for _, n := range wf.Nodes {
		for _, name := range ir.NodePromptRefs(n) {
			used[name] = true
		}
	}
	return used
}

// rendersRunURL reports whether a compiled workflow hands `{{run.url}}` to
// something that EXECUTES or is sent to a model: a prompt a node uses, a tool
// command / script / postcondition, or an action param. Everything is read
// from the COMPILED form — what the engine renders — so a reference in a `##`
// comment (which the parser drops) or in a prompt nobody uses cannot satisfy
// it. An unreachable tool node is refused by the compiler itself (C016), so
// the node walk needs no reachability of its own.
func rendersRunURL(wf *ir.Workflow) bool {
	hit := func(refs []*ir.Ref) bool {
		for _, r := range refs {
			if r != nil && r.Kind == ir.RefRun && len(r.Path) == 1 && r.Path[0] == "url" {
				return true
			}
		}
		return false
	}
	used := usedPrompts(wf)
	for name, p := range wf.Prompts {
		if used[name] && hit(p.TemplateRefs) {
			return true
		}
	}
	for _, n := range wf.Nodes {
		x, ok := n.(*ir.ToolNode)
		if !ok {
			continue
		}
		if hit(x.CommandRefs) || hit(x.ScriptRefs) || hit(x.PostcondRefs) {
			return true
		}
		for _, prm := range x.Params {
			if hit(prm.Refs) {
				return true
			}
		}
	}
	return false
}

// assemblesStudioPath reports whether a workflow BUILDS the studio run path
// itself — review-pr and gitops-warden derive it from the publish endpoint's
// origin inside a tool body. Read from the compiled tool bodies, not the
// file's text: those two have no posting skill, so this is their ONLY
// coverage, and a `##` comment satisfying it would leave them green and
// blind — which is exactly what the first version of this guard did.
func assemblesStudioPath(wf *ir.Workflow) bool {
	for _, n := range wf.Nodes {
		x, ok := n.(*ir.ToolNode)
		if !ok {
			continue
		}
		if strings.Contains(x.Command, runLinkStudioChannel) ||
			strings.Contains(x.Script, runLinkStudioChannel) ||
			strings.Contains(x.Postcondition, runLinkStudioChannel) {
			return true
		}
	}
	return false
}

// TestEveryForgeFacingBotLinksItsRun is the class guard: a bot that posts on
// a forge without naming its run leaves the reader with a verdict and no
// evidence, and leaves the platform unreachable from the one place its
// output is read.
func TestEveryForgeFacingBotLinksItsRun(t *testing.T) {
	for dir := range forgeFacingBots(t) {
		u := unittest.Whole(unit.LoadDir(filepath.Join(dir, "main.bot")))
		if u.Merged == nil {
			continue // the parse/compile test reports it
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		if !rendersRunURL(cr.Workflow) && !assemblesStudioPath(cr.Workflow) {
			t.Errorf("%s posts on a forge but renders no run link: pass {{run.url}} into the prompt or the tool body that publishes (the engine resolves it from the deployment's public origin, and it is empty — id alone, no link — where none is configured)", dir)
		}
	}
}

// TestEveryForgePostingSkillMandatesTheRunFooter covers the self-publishing
// half: there the body is written by an agent, so the rule lives in the
// skill it reads — and the VALUE must reach that agent, or the rule is
// unfulfillable prose. The two halves are checked together because each is
// useless alone.
func TestEveryForgePostingSkillMandatesTheRunFooter(t *testing.T) {
	seen := 0
	for dir := range forgeFacingBots(t) {
		skills := postingSkills(t, dir)
		for _, skill := range skills {
			seen++
			// Whitespace-collapsed: the rule is prose, so a reflow of the
			// paragraph must not redden a guard that exists to pin the
			// STATEMENT. The footer shape is matched the same way, which also
			// makes it indentation-independent inside its fence.
			body := strings.Join(strings.Fields(string(readFileOrFail(t, skill))), " ")
			for _, want := range []string{runLinkFooterShape, runLinkNoHostRule, runLinkIDRule} {
				if !strings.Contains(body, strings.Join(strings.Fields(want), " ")) {
					t.Errorf("%s no longer states the run footer rule (%q) — what this bot posts would carry no way back to the run, or would paste a caller-chosen id raw into a markdown label", skill, want)
				}
			}
		}
		if len(skills) == 0 {
			continue
		}
		u := unittest.Whole(unit.LoadDir(filepath.Join(dir, "main.bot")))
		if u.Merged == nil {
			continue
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		if !rendersRunURL(cr.Workflow) {
			t.Errorf("%s mandates the run footer in %v, but no prompt or tool body of %s/main.bot renders {{run.url}} — the agent is told to publish an address it is never given", dir, skills, dir)
		}
	}
	if seen != 10 {
		t.Errorf("%d forge-posting skills walked, want 10 — the skills/forge-*.md discovery no longer finds the class, so this test is green and blind", seen)
	}
}
