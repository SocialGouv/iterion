package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SubbotChildBotIdentity is the one reading every subbot lane (cloud
// runner, in-process, CLI, dispatcher) makes of a child: its memory key
// stays ResolveBotID over the manifest name, and its routing fold reads
// the slug and manifest name a top-level launch of its bundle carries.
func TestSubbotChildBotIdentity(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const bot = "workflow w:\n  entry: done\n"
	write("lib/manifest.yaml", "name: libbot\n")
	write("lib/main.bot", bot)
	nested := write("lib/sub/x.bot", bot)
	write("kit/manifest.yaml", "name: kitbot\n")
	write("kit/main.bot", bot)
	export := write("kit/workflows/x.bot", bot)
	write("sib/manifest.yaml", "name: sibbot\n")
	write("sib/main.bot", bot)
	sibling := write("sib/extend.bot", bot)
	write("cdir/manifest.yaml", "name: childbot\n")
	renamed := write("cdir/main.bot", bot)
	plain := write("plain/main.bot", bot)
	loose := write("loose/child.bot", bot)

	compiled := func(path string) *bundle.Bundle {
		t.Helper()
		_, _, b, err := CompileWorkflowPath(path, "", store.RunTrustDefault)
		if err != nil {
			t.Fatalf("compile %s: %v", path, err)
		}
		return b
	}
	for _, tc := range []struct {
		name     string
		path     string
		bundle   *bundle.Bundle
		memoryID string
		fold     llmroute.BotIdentity
	}{
		{"nested workflow", nested, compiled(nested), "libbot", llmroute.BotIdentity{ID: "lib", Name: "libbot"}},
		{"workflows export", export, compiled(export), "kitbot", llmroute.BotIdentity{ID: "kit", Name: "kitbot"}},
		{"sibling entry", sibling, compiled(sibling), "sibbot", llmroute.BotIdentity{ID: "sib", Name: "sibbot"}},
		{"main.bot its manifest renames", renamed, compiled(renamed), "childbot", llmroute.BotIdentity{ID: "cdir", Name: "childbot"}},
		// A bot:// child: the bundle the resolver handed names the bot,
		// whatever bundle the path's own directory names.
		{"a resolved bundle over the path", renamed, &bundle.Bundle{Dir: filepath.Join(dir, ".botz", "realbot"), Manifest: &bundle.Manifest{Name: "realbot"}}, "realbot", llmroute.BotIdentity{ID: "realbot", Name: "realbot"}},
		{"a bundle without a manifest", plain, &bundle.Bundle{Dir: filepath.Join(dir, "plain")}, "plain", llmroute.BotIdentity{ID: "plain"}},
		{"a loose file", loose, nil, "", llmroute.BotIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memoryID, fold := SubbotChildBotIdentity(tc.path, tc.bundle)
			if memoryID != tc.memoryID || fold != tc.fold {
				t.Fatalf("identity = (%q, %+v), want (%q, %+v)", memoryID, fold, tc.memoryID, tc.fold)
			}
		})
	}
}
