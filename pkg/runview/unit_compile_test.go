package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot in two files: the main imports the fragment that declares its only
// node, so a compile that reads the main alone has no `worker` to run.
const unitMain = "dsl: 2\nimport \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

// Pinned model and backend: the compile refuses C018 on a credential-less
// host, and these tests measure the unit.
const unitNodes = "schema out:\n  ok: bool\n\nprompt mission:\n  Do the thing.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"

func writeUnit(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAPathCompilesItsUnit: the compile behind every path-driven surface
// reads the file AND the fragments its imports reach, beside it — the
// fragment's node is in the workflow — and the identity it records covers
// the fragment: the same main over an edited fragment is another source,
// which is what makes a resume under a parked run refuse it.
func TestAPathCompilesItsUnit(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, map[string]string{"main.bot": unitMain, "lib/nodes.bot": unitNodes})
	mainBot := filepath.Join(root, "main.bot")

	wf, hash, b, err := CompileWorkflowPath(mainBot)
	if err != nil {
		t.Fatalf("CompileWorkflowPath: %v", err)
	}
	if b != nil {
		t.Fatalf("a loose file was promoted to a bundle: %+v", b)
	}
	if _, ok := wf.Nodes["worker"]; !ok {
		t.Fatalf("the fragment's node is not in the workflow (%d nodes)", len(wf.Nodes))
	}
	if hash == "" {
		t.Fatal("no identity recorded")
	}
	// What the compile read is reported file by file, for the run to record.
	_, cs, err := compileUnit(mainBot, "", true, nil)
	if err != nil {
		t.Fatalf("compileUnit: %v", err)
	}
	if cs.Hash != hash || cs.Main != "main.bot" || cs.Files["main.bot"] != unitMain || cs.Files["lib/nodes.bot"] != unitNodes || len(cs.Files) != 2 {
		t.Fatalf("compiled source: hash %s main %q files %v", cs.Hash, cs.Main, cs.Files)
	}

	writeUnit(t, root, map[string]string{"lib/nodes.bot": unitNodes + "\n## edited under the run\n"})
	_, edited, _, err := CompileWorkflowPath(mainBot)
	if err != nil {
		t.Fatalf("CompileWorkflowPath after the fragment edit: %v", err)
	}
	if edited == hash {
		t.Fatal("an edited fragment left the identity unchanged: a resume would run the new text as the old")
	}

	// A file beside the unit that nothing imports is not the unit's.
	writeUnit(t, root, map[string]string{"lib/unused.bot": "agent z:\n  description: \"z\"\n"})
	_, same, _, err := CompileWorkflowPath(mainBot)
	if err != nil {
		t.Fatalf("CompileWorkflowPath beside an unimported file: %v", err)
	}
	if same != edited {
		t.Fatal("a file nothing imports changed the identity")
	}
}

// TestTheIdentityOfASingleFileIsUnchanged pins the formula every run
// recorded so far: a loose file's identity is the digest of its bytes, a
// bundle's folds its prompts/*.md after them under the frame it always
// had. A unit with no fragment and no include changes nothing a recorded
// run compares against, so no resume asks for --force after the upgrade.
func TestTheIdentityOfASingleFileIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pause_demo.bot")
	if err := os.WriteFile(botPath, []byte(pausingBot), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	bare := sha256.Sum256([]byte(pausingBot))
	if hash != hex.EncodeToString(bare[:]) {
		t.Fatalf("a loose single file no longer hashes as its bytes: %s vs %s", hash, hex.EncodeToString(bare[:]))
	}

	bundleDir := promptedBundle(t)
	opened, err := bundle.OpenDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	_, promoted, err := CompileBundleWorkflow(opened.IterPath, opened)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}
	src, err := os.ReadFile(opened.IterPath)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(src)
	h.Write([]byte("\x00bundle.prompt:mission.md\x00Do the thing.\n"))
	if promoted != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("a bundle's identity no longer folds its prompts under the recorded frame: %s vs %s", promoted, hex.EncodeToString(h.Sum(nil)))
	}
}

// TestTheIdentityFollowsTheIncludeClosure: the files a prompt's
// {{include}} markers read — nested ones too — are part of what the
// agent reads, so they are part of the identity: an edit to one of them
// is a source change, a file nothing includes is not.
func TestTheIdentityFollowsTheIncludeClosure(t *testing.T) {
	root := t.TempDir()
	src := "schema out:\n  ok: bool\n\nprompt mission:\n  {{include \"notes/a.md\"}}\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	writeUnit(t, root, map[string]string{
		"main.bot":   src,
		"notes/a.md": "Alpha {{include \"b.md\"}}",
		"notes/b.md": "Beta",
	})
	mainBot := filepath.Join(root, "main.bot")

	_, cs, err := compileUnit(mainBot, "", true, nil)
	if err != nil {
		t.Fatalf("compileUnit: %v", err)
	}
	if !reflect.DeepEqual(cs.Included, []string{"notes/a.md", "notes/b.md"}) {
		t.Fatalf("included files %v, want the closure by path from the root", cs.Included)
	}
	bare := sha256.Sum256([]byte(src))
	if cs.Hash == hex.EncodeToString(bare[:]) {
		t.Fatal("the identity of a file with includes is the digest of its bytes alone: an edited include would resume as the old text")
	}

	writeUnit(t, root, map[string]string{"notes/b.md": "Beta, edited"})
	_, edited, err := compileUnit(mainBot, "", true, nil)
	if err != nil {
		t.Fatalf("compileUnit after the nested include edit: %v", err)
	}
	if edited.Hash == cs.Hash {
		t.Fatal("an edited nested include left the identity unchanged")
	}

	writeUnit(t, root, map[string]string{"notes/c.md": "nobody includes me"})
	_, same, err := compileUnit(mainBot, "", true, nil)
	if err != nil {
		t.Fatalf("compileUnit beside an unincluded file: %v", err)
	}
	if same.Hash != edited.Hash {
		t.Fatal("a file nothing includes changed the identity")
	}
}

// TestAnInlineSourceThatImportsIsRefused: text uploaded without the
// directory it came from names fragments that did not travel with it. The
// compile refuses it with a typed error, on both inline entry points, instead
// of compiling a program with pieces missing.
func TestAnInlineSourceThatImportsIsRefused(t *testing.T) {
	label := filepath.Join(t.TempDir(), "a1b2c3-main.bot")
	if _, _, err := CompileWorkflowFromSource(label, unitMain); !errors.Is(err, ErrInlineImport) {
		t.Fatalf("CompileWorkflowFromSource: err = %v, want ErrInlineImport", err)
	}
	if _, _, _, err := compileForLaunch("", unitMain, ""); !errors.Is(err, ErrInlineImport) {
		t.Fatalf("compileForLaunch without a bundle: err = %v, want ErrInlineImport", err)
	}
	// The same text, from its directory, is a program.
	root := t.TempDir()
	writeUnit(t, root, map[string]string{"main.bot": unitMain, "lib/nodes.bot": unitNodes})
	if _, _, _, err := compileForLaunch(filepath.Join(root, "main.bot"), "", ""); err != nil {
		t.Fatalf("the same unit from its directory: %v", err)
	}
}

// TestAnInlineDocumentWithItsBundleReadsTheFragmentsBesideTheMain: the
// studio's launch sends the document's text with the bundle it belongs to.
// The unit is then the bundle's, with the document as its main — the
// fragments read beside the bundle's main — and the identity is the one a
// launch of the file itself records, so a run launched on one surface
// resumes on the other without --force.
func TestAnInlineDocumentWithItsBundleReadsTheFragmentsBesideTheMain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bot")
	writeUnit(t, root, map[string]string{"main.bot": unitMain, "lib/nodes.bot": unitNodes, "skills/x.md": "# x\n"})
	mainBot := filepath.Join(root, "main.bot")
	copyPath := filepath.Join(t.TempDir(), "a1b2c3-main.bot")

	wf, cs, b, err := compileForLaunch(copyPath, unitMain, root)
	if err != nil {
		t.Fatalf("compileForLaunch(document, bundle): %v", err)
	}
	if b == nil || filepath.Clean(b.IterPath) != filepath.Clean(mainBot) {
		t.Fatalf("bundle %+v, want the one at %s", b, root)
	}
	if _, ok := wf.Nodes["worker"]; !ok {
		t.Fatalf("the fragment's node is not in the workflow (%d nodes)", len(wf.Nodes))
	}
	_, fromPath, _, err := CompileWorkflowPath(mainBot)
	if err != nil {
		t.Fatalf("CompileWorkflowPath: %v", err)
	}
	if cs.Hash != fromPath {
		t.Fatalf("the document's identity %s differs from the file's %s: a run launched from the studio would refuse a CLI resume", cs.Hash, fromPath)
	}
	// A document that differs from the file on disk is its own source.
	_, doc, _, err := compileForLaunch(copyPath, unitMain+"\n## edited in the editor\n", root)
	if err != nil {
		t.Fatalf("compileForLaunch(edited document, bundle): %v", err)
	}
	if doc.Hash == fromPath {
		t.Fatal("an edited document has the identity of the file on disk")
	}
}

// TestResume_RefusesAnEditedFragment: the gate `iterion resume` puts on a
// changed source sees a fragment edited under a parked run — the run's
// identity covers every file of its unit — and --force still opens it.
func TestResume_RefusesAnEditedFragment(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, map[string]string{
		"main.bot":     "import \"lib/gate.bot\"\n\nworkflow pause_demo:\n  entry: gate\n  gate -> done when approve\n  gate -> fail when not approve\n",
		"lib/gate.bot": "schema gate_out:\n  approve: bool\n\nprompt gate_prompt:\n  Approve?\n\nhuman gate:\n  instructions: gate_prompt\n  output: gate_out\n  interaction: human\n",
	})
	mainBot := filepath.Join(root, "main.bot")
	// The run gets a repository the test OWNS (#870).
	svc, err := NewService(root, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launched, err := svc.Launch(context.Background(), LaunchSpec{FilePath: mainBot})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-launched.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run did not reach its human pause")
	}
	before, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status before resume = %q, want paused_waiting_human", before.Status)
	}

	writeUnit(t, root, map[string]string{"lib/gate.bot": "## the gate, edited while the run was parked\n" + "schema gate_out:\n  approve: bool\n\nprompt gate_prompt:\n  Approve?\n\nhuman gate:\n  instructions: gate_prompt\n  output: gate_out\n  interaction: human\n"})
	answers := map[string]any{"approve": true}
	_, err = svc.Resume(context.Background(), ResumeSpec{RunID: launched.RunID, FilePath: mainBot, Answers: answers})
	if err == nil || !strings.Contains(err.Error(), "workflow source has changed") {
		t.Fatalf("Resume over an edited fragment: err = %v, want the source-changed refusal", err)
	}
	forced, err := svc.Resume(context.Background(), ResumeSpec{RunID: launched.RunID, FilePath: mainBot, Answers: answers, Force: true})
	if err != nil {
		t.Fatalf("forced Resume: %v", err)
	}
	select {
	case <-forced.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("forced resume did not finish")
	}
	finished, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != store.RunStatusFinished {
		t.Fatalf("status after forced resume = %q (error %q), want finished", finished.Status, finished.Error)
	}
}
