package commands_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/commands"
)

// The public surface is the whole point of the package: an embedder reaches
// `.claude/commands/` only through here. A swapped return, a wrong argument
// order or a dropped re-export compiles fine and breaks every consumer, so
// one round trip goes through the exported names — never the internal ones.
func TestPublicSurfaceRoundTrip(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".claude", "commands", "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested.md"),
		[]byte("---\ndescription: the nested one\n---\nARGS=[$ARGUMENTS] ONE=[$1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if commands.CommandsDir(ws) != filepath.Join(ws, ".claude", "commands") {
		t.Errorf("CommandsDir = %q", commands.CommandsDir(ws))
	}
	if !commands.HasWorkspaceCommands(ws) {
		t.Fatal("HasWorkspaceCommands = false")
	}
	if commands.HasWorkspaceCommands(t.TempDir()) {
		t.Error("HasWorkspaceCommands on a bare dir = true")
	}

	name, args, ok := commands.ParseInvocation("/sub:nested alpha beta")
	if !ok || name != "sub:nested" || args != "alpha beta" {
		t.Fatalf("ParseInvocation = (%q, %q, %v)", name, args, ok)
	}

	cmd, found, err := commands.LookupWorkspace(ws, name, 0)
	if err != nil || !found {
		t.Fatalf("LookupWorkspace = (%v, %v)", found, err)
	}
	if cmd.Description != "the nested one" {
		t.Errorf("Description = %q", cmd.Description)
	}
	got, consumed, err := commands.Expand(cmd, args, 0)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "ARGS=[alpha beta] ONE=[alpha]" {
		t.Errorf("Expand = %q", got)
	}
	if !consumed {
		t.Error("Expand reported consumed=false for a body full of placeholders")
	}
	if forms := commands.DynamicBodyForms(cmd.Body, args); len(forms) != 1 || forms[0] != "$N" {
		t.Errorf("DynamicBodyForms = %v, want [$N]", forms)
	}
}
