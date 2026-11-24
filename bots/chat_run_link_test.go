package bots

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// A bot that posts to chat (Mattermost/Slack incoming webhooks) names its
// run exactly like a forge-facing one: the channel is where its output is
// read, and a digest with no way back to the run is an assertion
// (docs/agents/bots/run-link.md, #2430). The link lives in the DELIVERY
// tool scripts — notify carries the AI provenance beside it,
// notify_silence the signature alone — and everything is read from the
// COMPILED workflow, never from the file's text: a `##` comment naming
// {{run.url}} is not a channel, and a guard that accepted one would be
// green over a bot that publishes nothing.
//
// feed-watch (Vigie) is the pinned class. prod-watch (Argus) posts the
// same lane and joins when its retrofit lands (#2430 stays open for it) —
// added here the day it carries the link, so it cannot drop out quietly.
var chatFacingExpected = []string{"feed-watch"}

func TestEveryChatFacingBotLinksItsRun(t *testing.T) {
	for _, dir := range chatFacingExpected {
		u := unittest.Whole(unit.LoadDir(filepath.Join(dir, "main.bot")))
		if u.Merged == nil {
			continue // the parse/compile test reports it
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		for _, id := range []string{"notify", "notify_silence"} {
			node, ok := cr.Workflow.Nodes[id]
			if !ok {
				t.Fatalf("%s is missing its %s node — the delivery node the run link lives in", dir, id)
			}
			tool, ok := node.(*ir.ToolNode)
			if !ok {
				t.Fatalf("%s's %s is no longer a ToolNode (%T) — the guard reads script refs", dir, id, node)
			}
			linked := false
			for _, r := range tool.ScriptRefs {
				if r != nil && r.Kind == ir.RefRun && len(r.Path) == 1 && r.Path[0] == "url" {
					linked = true
				}
			}
			if !linked {
				t.Errorf("%s's %s posts to chat without rendering {{run.url}} — the channel reader gets no way back to the run that wrote the message", dir, id)
			}
		}
		// The provenance half: the digest footer shows the model and
		// harness that served, Revi-style — pinned as a literal so the
		// block cannot quietly lose it and leave a bare link behind.
		node := cr.Workflow.Nodes["notify"]
		tool, ok := node.(*ir.ToolNode)
		if !ok {
			t.Fatalf("%s's notify is no longer a ToolNode (%T)", dir, node)
		}
		if !strings.Contains(tool.Script, "Détails du run IA · Iterion") {
			t.Errorf("%s's notify no longer builds the AI provenance block — the chat lane would publish the run link without the model/harness that served the digest", dir)
		}
	}
}
