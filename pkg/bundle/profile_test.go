package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The profile of a bundle is the highest its executable sources declare —
// main.bot and every subbot child reached from it, transitively — and
// nothing outside the bundle or unreachable from its main is read.
func TestMaxSyntaxProfileWalksTheSubbotChildren(t *testing.T) {
	files := map[string]string{
		"main.bot":              "subbot child:\n  source: \"children/a.bot\"\n\nworkflow w:\n  entry: child\n  child -> done\n",
		"children/a.bot":        "dsl: 2\n\nsubbot grand:\n  source: \"b.bot\"\n\nsubbot out:\n  source: \"../../outside.bot\"\n\nworkflow w:\n  entry: grand\n  grand -> done\n",
		"children/b.bot":        "dsl: 2\n\nsubbot back:\n  source: \"a.bot\"\n\nworkflow w:\n  entry: back\n  back -> done\n",
		"unreachable/three.bot": "dsl: 2\nagent a:\n  description: \"x\"\n",
		// Where `children/../../outside.bot` cleans to: reachable by path,
		// outside the bundle, never read.
		"../outside.bot": "dsl: 2\n",
	}
	profile, by, unread := MaxSyntaxProfile(files)
	if profile != 2 || !reflect.DeepEqual(by, []string{"children/a.bot", "children/b.bot"}) {
		t.Fatalf("profile %d by %v", profile, by)
	}
	// The child outside the bundle is named, not silently skipped.
	if !reflect.DeepEqual(unread, []string{"../outside.bot"}) {
		t.Fatalf("unread %v", unread)
	}
	// A bundle whose sources all read as profile 1 declares 1, by nobody.
	profile, by, _ = MaxSyntaxProfile(map[string]string{"main.bot": "agent a:\n  description: \"x\"\n"})
	if profile != 1 || len(by) != 0 {
		t.Fatalf("profile %d by %v", profile, by)
	}
	// The main itself may be the one that declares it.
	profile, by, _ = MaxSyntaxProfile(map[string]string{"main.bot": "dsl: 2\nagent a:\n  description: \"x\"\n"})
	if profile != 2 || !reflect.DeepEqual(by, []string{"main.bot"}) {
		t.Fatalf("profile %d by %v", profile, by)
	}
	// A missing child is not this walk's to report.
	profile, _, unread = MaxSyntaxProfile(map[string]string{"main.bot": "subbot c:\n  source: \"nope.bot\"\n"})
	if profile != 1 || len(unread) != 0 {
		t.Fatalf("missing child: profile %d unread %v", profile, unread)
	}
}

// The on-disk form reads the same files the map form would.
func TestMaxSyntaxProfileDirReadsTheBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "kids"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "main.bot"), []byte("subbot k:\n  source: \"kids/k.bot\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "kids", "k.bot"), []byte("dsl: 2\nagent a:\n  description: \"x\"\n"), 0o644)
	profile, by, _ := MaxSyntaxProfileDir(dir)
	if profile != 2 || !reflect.DeepEqual(by, []string{"kids/k.bot"}) {
		t.Fatalf("profile %d by %v", profile, by)
	}
}
