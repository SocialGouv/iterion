package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The subbot node's landing intent must reach the child run doc on the CLI
// path too: `iterion run` and `iterion resume` share this runner, and the
// doc is what a resume replay re-arms the child engine with — a wiring that
// stops at the request leaves the replay reading "never lands". The child
// here owns a worktree of its own (isolated, git workspace, `worktree: auto`
// default), the only shape the intent can act on.
func TestSubbotRunnerForCLILandingIntentReachesTheChildRunDoc(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	// The child engine's own tree, a git repo with a HEAD so the child's
	// worktree has something to anchor on.
	childTree := t.TempDir()
	gittest.Run(t, childTree, "init", "-q", "-b", "main")
	gittest.Run(t, childTree, "commit", "--allow-empty", "-qm", "anchor")
	if err := os.WriteFile(filepath.Join(dir, "child.bot"), []byte(`tool noop:
  command: "echo landing"

workflow child:
  sandbox: none
  entry: noop
  noop -> done
`), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, logger, RunOptions{NoInteractive: true})
	if _, err := runner(context.Background(), runtime.SubbotRequest{
		Source:        "child.bot",
		ParentRunID:   "landing-parent",
		NodeID:        "lot",
		ReattachKey:   "lot",
		WorkDir:       childTree,
		Isolated:      true,
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
		t.Fatalf("child doc carries no auto_merge intent — `iterion run` would never land this child (doc %s)", docs[0])
	}
	if doc.MergeInto != "main" || doc.MergeStrategy != "merge" {
		t.Fatalf("child doc landing = (%q, %q), want (main, merge)", doc.MergeInto, doc.MergeStrategy)
	}
	if doc.ParentRunID != "landing-parent" {
		t.Fatalf("child doc parent linkage = %q, want landing-parent", doc.ParentRunID)
	}
}

// A child that declares `worktree: none` owns no finalization, and no bank
// ever writes a subbot child's branch — the armed intent would sit on the
// run doc unfired forever. The CLI launch refuses it BEFORE any child run
// exists: the refusal is the effect witnessed here, and the store staying
// empty proves no dead-intent doc was recorded.
func TestSubbotRunnerForCLILandingRefusedOnAWorktreeNoneChild(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	if err := os.WriteFile(filepath.Join(dir, "child.bot"), []byte(`tool noop:
  command: "echo landing"

workflow child:
  worktree: none
  sandbox: none
  entry: noop
  noop -> done
`), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, logger, RunOptions{NoInteractive: true})
	_, err = runner(context.Background(), runtime.SubbotRequest{
		Source:        "child.bot",
		ParentRunID:   "landing-parent",
		NodeID:        "lot",
		ReattachKey:   "lot",
		WorkDir:       t.TempDir(),
		Isolated:      true,
		AutoMerge:     true,
		MergeInto:     "main",
		MergeStrategy: "merge",
	})
	if err == nil {
		t.Fatal("subbot run = nil error, want a refusal: a worktree:none child cannot land the armed intent")
	}
	if !strings.Contains(err.Error(), "worktree: none") {
		t.Fatalf("refusal %q does not name the declaration that disarms the intent", err)
	}
	docs, err := filepath.Glob(filepath.Join(storeDir, "runs", "*", "run.json"))
	if err != nil {
		t.Fatalf("glob run docs: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("the refused launch left %d child run doc(s) behind (%v) — the dead intent is on the record", len(docs), docs)
	}
}
