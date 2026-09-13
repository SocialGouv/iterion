package parser

import (
	"strings"
	"testing"
)

// A block header with nothing under it — `budget:`, `memory:`, `vars:` and
// the like — declares an EMPTY block, the way a bare `prompt p:` declares
// an empty prompt: at the end of its parent (a dedent or the end of the
// file), or followed by a blank line and a sibling. The studio's document
// carries such a block as `{}`, and a plain file whose only property is
// zero-valued reads back as one; both need a written form.
func TestEmptyBlocksParse(t *testing.T) {
	src := "vars:\n\npresets:\n\nattachments:\n\nsecrets:\n\n" +
		"mcp_server m:\n  transport: stdio\n  command: \"x\"\n  auth:\n\n" +
		"agent a:\n  model: \"m\"\n  mcp:\n\n  compaction:\n\n  memory:\n\n  cursors:\n\n  sandbox:\n\n" +
		"agent b:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    build:\n\n    network:\n\n" +
		"tool t:\n  command: \"true\"\n  recovery:\n\n" +
		"workflow w:\n  entry: a\n  vars:\n\n  attachments:\n\n  mcp:\n\n  budget:\n\n  resources:\n\n  compaction:\n\n  a -> done\n"
	pr := Parse("blocks.bot", src)
	for _, d := range pr.Diagnostics {
		t.Errorf("unexpected diagnostic: %s", d.Error())
	}
	f := pr.File
	if f.Vars == nil || f.Presets == nil || f.Attachments == nil || f.Secrets == nil {
		t.Errorf("an empty top-level block was dropped: vars=%v presets=%v attachments=%v secrets=%v", f.Vars != nil, f.Presets != nil, f.Attachments != nil, f.Secrets != nil)
	}
	if len(f.MCPServers) != 1 || f.MCPServers[0].Auth == nil {
		t.Errorf("an empty auth block was dropped: %+v", f.MCPServers)
	}
	if len(f.Agents) != 2 {
		t.Fatalf("want two agents, got %+v", f.Agents)
	}
	a := f.Agents[0]
	if a.MCP == nil || a.Compaction == nil || a.Memory == nil || a.Cursors == nil {
		t.Errorf("an empty agent block was dropped: mcp=%v compaction=%v memory=%v cursors=%v", a.MCP != nil, a.Compaction != nil, a.Memory != nil, a.Cursors != nil)
	}
	if a.Sandbox == nil || a.Sandbox.Mode != "inline" {
		t.Errorf("an empty sandbox block is the inline block form: %+v", a.Sandbox)
	}
	if sb := f.Agents[1].Sandbox; sb == nil || sb.Build == nil || sb.Network == nil {
		t.Errorf("an empty sandbox sub-block was dropped: %+v", sb)
	}
	if len(f.Tools) != 1 || f.Tools[0].Recovery == nil {
		t.Errorf("an empty recovery block was dropped: %+v", f.Tools)
	}
	if len(f.Workflows) != 1 {
		t.Fatalf("want one workflow, got %+v", f.Workflows)
	}
	wf := f.Workflows[0]
	if wf.Vars == nil || wf.Attachments == nil || wf.MCP == nil || wf.Budget == nil || wf.Resources == nil || wf.Compaction == nil {
		t.Errorf("an empty workflow block was dropped: vars=%v attachments=%v mcp=%v budget=%v resources=%v compaction=%v", wf.Vars != nil, wf.Attachments != nil, wf.MCP != nil, wf.Budget != nil, wf.Resources != nil, wf.Compaction != nil)
	}
	if wf.Entry != "a" || len(wf.Edges) != 1 {
		t.Errorf("the properties around the empty blocks were lost: entry=%q edges=%d", wf.Entry, len(wf.Edges))
	}
}

// An empty block at the end of its parent needs no blank line: a dedent, or
// the end of the file with or without a trailing newline, ends it.
func TestEmptyBlockAtADedentOrAtEOFNeedsNoBlankLine(t *testing.T) {
	for _, src := range []string{
		"agent a:\n  model: \"m\"\n  memory:\nworkflow w:\n  entry: a\n  a -> done\n",
		"agent a:\n  model: \"m\"\n  memory:\n",
		"agent a:\n  model: \"m\"\n  memory:",
	} {
		pr := Parse("dedent.bot", src)
		for _, d := range pr.Diagnostics {
			t.Errorf("%q: unexpected diagnostic: %s", src, d.Error())
		}
		if len(pr.File.Agents) != 1 || pr.File.Agents[0].Memory == nil {
			t.Errorf("%q: the empty memory block was dropped: %+v", src, pr.File.Agents)
		}
	}
}

// A `fallbacks:` block exists to hold routes; the AST has no way to carry
// an empty one (a nil list is "no fallbacks"), so a bare header is not the
// empty block — it is an error that says what is missing, instead of the
// E002 about indentation it used to be.
func TestBareFallbacksIsAnErrorNamingTheRoute(t *testing.T) {
	pr := Parse("fb.bot", "agent a:\n  model: \"m\"\n  fallbacks:\n\nworkflow w:\n  entry: a\n  a -> done\n")
	for _, d := range pr.Diagnostics {
		if strings.Contains(d.Message, "route") {
			return
		}
	}
	t.Fatalf("a bare fallbacks: header drew no error naming the missing route: %v", pr.Diagnostics)
}

// A block body at the wrong indentation, with no blank line between the
// header and it, is still the indentation error with its hint — not an
// empty block followed by a stray property of the parent.
func TestUnindentedBlockBodyIsStillTheIndentError(t *testing.T) {
	for _, src := range []string{
		"workflow w:\n  entry: a\n  budget:\n  max_cost_usd: 3\n  a -> done\n",
		"agent a:\n  memory:\n  model: \"m\"\n",
		"vars:\nx: string\n",
	} {
		if pr := Parse("bad.bot", src); !indentError(pr) {
			t.Errorf("%q: an unindented block body parsed without the indentation error: %v", src, pr.Diagnostics)
		}
	}
}
