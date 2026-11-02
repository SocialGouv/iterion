package runview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The subbot node's landing intent must reach the child run doc: the doc
// is what the instance's outcome router lands from on the cloud plane and
// what a resume replay re-arms the child engine with — a wiring that
// stops at the request leaves both reading "never lands".

func TestSubbotLandingIntentReachesTheChildRunDoc(t *testing.T) {
	storeDir := t.TempDir()
	botDir := t.TempDir()
	// The child engine's own tree, distinct from the process cwd (the
	// mirror-cwd guard refuses a mirror into the package directory).
	childTree := t.TempDir()
	writeFile(t, botDir, "child.bot", `tool noop:
  command: "echo landing"

workflow child:
  worktree: none
  sandbox: none
  entry: noop
  noop -> done
`)

	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()), WithWorkDir(botDir), WithSandboxDefault("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)

	run := svc.subbotRunnerFor(filepath.Join(botDir, "parent.bot"), iterlog.Nop())
	if _, err := run(context.Background(), runtime.SubbotRequest{
		Source:        "child.bot",
		ParentRunID:   "landing-parent",
		NodeID:        "lot",
		ReattachKey:   "lot",
		WorkDir:       childTree,
		AutoMerge:     true,
		MergeInto:     "main",
		MergeStrategy: "merge",
	}); err != nil {
		t.Fatalf("subbot run: %v", err)
	}

	// The child doc is the artifact: exactly one run exists in this store,
	// and it must carry the intent verbatim.
	docs, err := filepath.Glob(filepath.Join(storeDir, "runs", "*", "run.json"))
	if err != nil {
		t.Fatalf("glob run docs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want exactly one child run doc, got %v", docs)
	}
	body, err := os.ReadFile(docs[0])
	if err != nil {
		t.Fatalf("read child doc: %v", err)
	}
	var doc struct {
		AutoMerge     bool   `json:"auto_merge"`
		MergeInto     string `json:"merge_into"`
		MergeStrategy string `json:"merge_strategy"`
		ParentRunID   string `json:"parent_run_id"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse child doc: %v", err)
	}
	if !doc.AutoMerge {
		t.Fatalf("child doc carries no auto_merge intent — the instance would never land this child (doc %s)", docs[0])
	}
	if doc.MergeInto != "main" || doc.MergeStrategy != "merge" {
		t.Fatalf("child doc landing = (%q, %q), want (main, merge)", doc.MergeInto, doc.MergeStrategy)
	}
	if doc.ParentRunID != "landing-parent" {
		t.Fatalf("child doc parent linkage = %q, want landing-parent", doc.ParentRunID)
	}
}
