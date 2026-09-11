package cli

import (
	"path/filepath"
	"testing"
)

// TestRunDiagram_BareMainBotOfABundleSeesItsPrompts: `iterion diagram
// bots/x/main.bot` compiles the bundle as validate does — the multi-file
// shape declares no prompt in main.bot, so a bare compile fails C003 and a
// drawn diagram is the promotion itself.
func TestRunDiagram_BareMainBotOfABundleSeesItsPrompts(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	jp, out := jsonPrinter()
	if err := RunDiagram(DiagramOptions{File: filepath.Join("bots", "mf", "main.bot")}, jp); err != nil {
		t.Fatalf("diagram on the bare main.bot: %v\n%s", err, out.String())
	}
}
