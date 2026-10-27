package botregistry

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bot whose frontmatter the reader does not read is listed — a loose
// .bot under its file's name, a bundle with its manifest's triggers — and
// the catalogue says why, the remedy named (DiscoveryErrorIdentity): a
// block whose values pass the read budget (r3b F3), a main whose body does
// (r3a F7). Lowering the budget must never strip a bot's identity in
// silence. Each of the good bots beside them keeps its own.
func TestABotWhoseIdentityIsNotReadIsListedAndSaid(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "400")
	dir := t.TempDir()
	for _, n := range []string{"good1", "good2"} {
		writeFile(t, filepath.Join(dir, n+".bot"), "## ---\n## name: "+n+"\n## description: a good bot\n## triggers: [t1]\n## ---\nagent a:\n  model: \"m\"\n")
	}
	writeFile(t, filepath.Join(dir, "dense.bot"), "## ---\n## name: dense-identity\n## description: a bot whose block is dense\n## x: ["+strings.Repeat("a,", 300)+"a]\n## ---\nagent a:\n  model: \"m\"\n")
	writeFile(t, filepath.Join(dir, "body.bot"), "## ---\n## name: watcher\n## triggers: [issue_comment]\n## ---\nprompt p:\n"+strings.Repeat("  x\n", 500)+"\nagent a:\n  model: \"m\"\n  system: p\n")
	bundleDir := filepath.Join(dir, "bundled")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), "name: bundled\nversion: 1.0.0\ntriggers: [push]\n")
	writeFile(t, filepath.Join(bundleDir, "main.bot"), "## ---\n## triggers: [issue_comment]\n## ---\nprompt p:\n"+strings.Repeat("  x\n", 500)+"\nagent a:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n")
	entries, diags, err := ListWithDiagnostics(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	for _, n := range []string{"good1", "good2", "dense", "body", "bundled"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("%s is not listed: %v", n, entries)
		}
	}
	if e := byName["good1"]; e.Description != "a good bot" || len(e.Triggers) != 1 {
		t.Errorf("a good bot lost its identity beside the unread ones: %+v", e)
	}
	if e := byName["bundled"]; len(e.Triggers) != 1 || e.Triggers[0] != "push" {
		t.Errorf("the bundle's entry carries %v, want the manifest's triggers its unread frontmatter does not override", e.Triggers)
	}
	said := map[string]string{}
	for _, d := range diags {
		if d.Kind == DiscoveryErrorIdentity {
			said[filepath.Base(d.Path)] = d.Error
		}
	}
	for _, n := range []string{"dense.bot", "body.bot", "bundled"} {
		if why, ok := said[n]; !ok || !strings.Contains(why, parser.ReadBudgetEnv) {
			t.Errorf("%s: the catalogue says %q of its unread identity, want an identity diagnostic naming %s", n, why, parser.ReadBudgetEnv)
		}
	}
	if len(said) != 3 {
		t.Errorf("identity diagnostics %v, want the three unread bots' alone", said)
	}
}

// The workspace overlay is held to the size cap and its decode to the read
// budget before yaml.v2 builds a node of it (r3b F2): one whose values pass
// the budget is not decoded. Its refusal is the listing's error — the
// operator's visibility decisions are unread, and a listing that answered
// without them would route every bot the overlay disabled — unless the
// caller opts past it, and is then answered every bot disabled and the
// refusal said (DiscoveryErrorOverlay). It is not rewritten from what was not
// read: the toggle refuses, the file's bytes kept.
func TestTheWorkspaceOverlayIsHeldToTheReadBudget(t *testing.T) {
	work := t.TempDir()
	bots := filepath.Join(work, "bots")
	writeFile(t, filepath.Join(bots, "a.bot"), "agent a:\n  model: \"m\"\n")
	keys := make([]string, 301)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	overlay := "bots: {" + strings.Join(keys, ",") + "}\n"
	path := filepath.Join(work, ".iterion", "bot-overrides.yaml")
	writeFile(t, path, overlay)
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(200))
	var refused *OverlayRefusal
	if entries, _, err := ListWithDiagnostics(ListOptions{Paths: []string{bots}, Workdir: work}); !errors.As(err, &refused) || entries != nil {
		t.Errorf("a listing over an overlay past the budget answered %+v, %v — want no entry and the overlay's refusal", entries, err)
	} else if !strings.Contains(err.Error(), parser.ReadBudgetEnv) || strings.Contains(err.Error(), work) {
		t.Errorf("the refusal %q does not name its remedy, or names the workspace's root", err)
	}
	entries, diags, err := ListWithDiagnostics(ListOptions{Paths: []string{bots}, Workdir: work, ListPastAnUnreadOverlay: true})
	if err != nil {
		t.Fatalf("an opted-in listing past an overlay past the budget: %v", err)
	}
	if len(entries) != 1 || entries[0].Enabled {
		t.Errorf("entries %+v, want the bot listed disabled", entries)
	}
	if len(diags) != 1 || diags[0].Kind != DiscoveryErrorOverlay || !strings.Contains(diags[0].Error, parser.ReadBudgetEnv) || strings.Contains(diags[0].Error, work) {
		t.Errorf("diagnostics %+v, want the overlay's refusal, its remedy named, the workspace's root cut", diags)
	}
	before := sha256.Sum256([]byte(overlay))
	off := false
	err = SetOverlayEnabled(context.Background(), work, "a", &off)
	if !errors.As(err, &refused) {
		t.Errorf("a toggle over an overlay past the budget: %v, want its refusal", err)
	}
	if got, _ := os.ReadFile(path); sha256.Sum256(got) != before {
		t.Errorf("the overlay was rewritten from what was not read: %q", got)
	}
	// Within the budget it reads, and says nothing.
	t.Setenv(parser.ReadBudgetEnv, "0")
	if _, diags, err := ListWithDiagnostics(ListOptions{Paths: []string{bots}, Workdir: work}); err != nil || len(diags) != 0 {
		t.Errorf("the overlay read whole: %v %v", diags, err)
	}
}
