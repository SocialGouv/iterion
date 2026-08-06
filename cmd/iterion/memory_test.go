package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// `iterion memory export|import|du` is the operator's only hands-on
// surface onto the workspace knowledge tree a run's memory_* tools
// write into: it is how a memory space is backed up, moved to another
// machine, or handed to a colleague. The FS store and the archive
// codec are unit-covered; the COMMANDS were not — so nothing asserted
// that the flags an operator types actually address the space their
// runs use, or that an exported archive imports back into a readable
// space.
//
// The tests drive the real `rootCmd` (the version_test.go idiom: a
// subcommand with a parent must be executed through root), against an
// ITERION_HOME pointed at a temp dir so the operator's own memory tree
// is never read or written.
//
// Mutation check: resolve the space from anything but the flags and the
// round-trip finds nothing to export; drop the archive write and import
// reports 0 documents; ignore --strategy and the skip/overwrite
// assertions diverge; stop counting bytes and `du` reports 0 for a
// space that holds a document.

const memoryDocBody = `---
title: Coverage ritual
description: how this repo proves a feature is covered
---

Write the test, see it pass, flip the row.
`

// resetMemoryFlags restores the package-level cobra flag targets to
// their declared defaults. Cobra parses into globals, so without this a
// value from one invocation leaks into the next and a later command
// silently addresses the earlier one's space.
func resetMemoryFlags() {
	memoryOpts.visibility = "bot"
	memoryOpts.name = ""
	memoryOpts.project = ""
	memoryOpts.user = ""
	memoryOpts.tenant = ""
	memoryOpts.bot = ""
	memoryOpts.out = ""
	memoryOpts.in = ""
	memoryOpts.strategy = "skip"
	rootCmd.SetArgs(nil)
	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)
	rootCmd.SetIn(nil)
}

// runMemoryCmd drives the real cobra tree and returns stdout+stderr.
func runMemoryCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetMemoryFlags()
	t.Cleanup(resetMemoryFlags)
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return buf.String(), err
}

// botSpace is the SpaceRef the `--visibility bot --bot <id> --project
// <dir> --name <n>` flag set addresses. Built here through the same
// resolver the command uses, so the assertions are about the space an
// operator's flags reach — not a path this test invented.
func botSpace(projectDir, bot, name string) knowledge.SpaceRef {
	return memory.ResolveSpaceRef(
		knowledge.VisibilityBot, name, bot, "",
		memory.SpaceRefInputs{ProjectID: memory.ProjectKey(projectDir), BotID: bot},
	)
}

func seedMemoryDoc(t *testing.T, ref knowledge.SpaceRef, path, content string) {
	t.Helper()
	if _, err := memory.DefaultFSStore().WriteDocument(context.Background(), ref, knowledge.DocumentInput{
		Path:      path,
		Content:   []byte(content),
		UpdatedBy: "e2e",
	}); err != nil {
		t.Fatalf("seed memory doc %s: %v", path, err)
	}
}

func readMemoryDoc(t *testing.T, ref knowledge.SpaceRef, path string) string {
	t.Helper()
	doc, err := memory.DefaultFSStore().ReadDocument(context.Background(), ref, path)
	if err != nil {
		t.Fatalf("read memory doc %s from %s: %v", path, ref.ID(), err)
	}
	return string(doc.Content)
}

func TestMemoryExportImportRoundTripsASpace(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	projectDir := t.TempDir()

	src := botSpace(projectDir, "endy", "notes")
	seedMemoryDoc(t, src, "rituals/coverage.md", memoryDocBody)
	seedMemoryDoc(t, src, "rituals/nested/deep.md", "# Deep\n\nstill archived\n")

	archive := filepath.Join(t.TempDir(), "notes.tar.gz")
	if _, err := runMemoryCmd(t, "memory", "export",
		"--visibility", "bot", "--bot", "endy", "--name", "notes",
		"--project", projectDir, "--out", archive,
	); err != nil {
		t.Fatalf("memory export: %v", err)
	}

	// Import into a DIFFERENT space: a round-trip back into the source
	// would pass even if the archive were empty and import a no-op.
	if _, err := runMemoryCmd(t, "memory", "import",
		"--visibility", "bot", "--bot", "endy", "--name", "restored",
		"--project", projectDir, "--in", archive,
	); err != nil {
		t.Fatalf("memory import: %v", err)
	}

	dst := botSpace(projectDir, "endy", "restored")
	if got := readMemoryDoc(t, dst, "rituals/coverage.md"); got != memoryDocBody {
		t.Errorf("restored document = %q, want the exported body", got)
	}
	if got := readMemoryDoc(t, dst, "rituals/nested/deep.md"); !strings.Contains(got, "still archived") {
		t.Errorf("nested document did not survive the round-trip: %q", got)
	}

	// The index the run-time memory tools build must see both documents
	// in the restored space — a file on disk that the index misses is
	// invisible to every bot.
	idx, err := memory.DefaultFSStore().BuildIndex(context.Background(), dst)
	if err != nil {
		t.Fatalf("build index of the restored space: %v", err)
	}
	if len(idx) != 2 {
		t.Errorf("restored index has %d entries, want 2: %+v", len(idx), idx)
	}
	var found bool
	for _, e := range idx {
		if e.Path == "rituals/coverage.md" {
			found = true
			if e.Title != "Coverage ritual" {
				t.Errorf("restored title = %q, want the frontmatter title", e.Title)
			}
		}
	}
	if !found {
		t.Errorf("restored index omits rituals/coverage.md: %+v", idx)
	}
}

func TestMemoryImportStrategyDecidesWhoWinsOnConflict(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	projectDir := t.TempDir()

	src := botSpace(projectDir, "endy", "src")
	seedMemoryDoc(t, src, "note.md", "# from the archive\n")

	archive := filepath.Join(t.TempDir(), "src.tar.gz")
	if _, err := runMemoryCmd(t, "memory", "export",
		"--visibility", "bot", "--bot", "endy", "--name", "src",
		"--project", projectDir, "--out", archive,
	); err != nil {
		t.Fatalf("memory export: %v", err)
	}

	dst := botSpace(projectDir, "endy", "dst")
	seedMemoryDoc(t, dst, "note.md", "# already here\n")

	// skip (the default) must not clobber the operator's own note.
	out, err := runMemoryCmd(t, "memory", "import",
		"--visibility", "bot", "--bot", "endy", "--name", "dst",
		"--project", projectDir, "--in", archive, "--strategy", "skip",
	)
	if err != nil {
		t.Fatalf("memory import --strategy skip: %v", err)
	}
	if !strings.Contains(out, "skipped=1") {
		t.Errorf("import --strategy skip reported %q, want skipped=1", strings.TrimSpace(out))
	}
	if got := readMemoryDoc(t, dst, "note.md"); got != "# already here\n" {
		t.Errorf("--strategy skip overwrote the existing document: %q", got)
	}

	// overwrite must.
	out, err = runMemoryCmd(t, "memory", "import",
		"--visibility", "bot", "--bot", "endy", "--name", "dst",
		"--project", projectDir, "--in", archive, "--strategy", "overwrite",
	)
	if err != nil {
		t.Fatalf("memory import --strategy overwrite: %v", err)
	}
	if !strings.Contains(out, "imported=1") {
		t.Errorf("import --strategy overwrite reported %q, want imported=1", strings.TrimSpace(out))
	}
	if got := readMemoryDoc(t, dst, "note.md"); got != "# from the archive\n" {
		t.Errorf("--strategy overwrite did not replace the document: %q", got)
	}
}

func TestMemoryDuReportsTheSpaceUsageAndQuota(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	projectDir := t.TempDir()

	ref := botSpace(projectDir, "endy", "sized")

	// An empty space reports zero usage — the baseline the next
	// assertion is measured against.
	empty, err := runMemoryCmd(t, "memory", "du",
		"--visibility", "bot", "--bot", "endy", "--name", "sized", "--project", projectDir,
	)
	if err != nil {
		t.Fatalf("memory du on an empty space: %v", err)
	}
	if !strings.Contains(empty, "used=0") {
		t.Errorf("du on an empty space reported %q, want used=0", strings.TrimSpace(empty))
	}

	seedMemoryDoc(t, ref, "big.md", strings.Repeat("x", 4096))

	out, err := runMemoryCmd(t, "memory", "du",
		"--visibility", "bot", "--bot", "endy", "--name", "sized", "--project", projectDir,
	)
	if err != nil {
		t.Fatalf("memory du: %v", err)
	}
	if !strings.Contains(out, ref.ID()) {
		t.Errorf("du output %q does not name the addressed space %q", strings.TrimSpace(out), ref.ID())
	}
	if strings.Contains(out, "used=0 ") {
		t.Errorf("du reported zero usage for a space holding 4KiB: %q", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "quota=") {
		t.Errorf("du output %q omits the quota an operator needs to act on", strings.TrimSpace(out))
	}
}

// A bot space without --bot has no identity, so it would silently
// resolve to some other space. The command must refuse rather than
// export the wrong tree.
func TestMemoryRefusesAnUnaddressableSpace(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	if _, err := runMemoryCmd(t, "memory", "du", "--visibility", "bot", "--name", "notes"); err == nil {
		t.Fatalf("memory du accepted --visibility bot with no --bot")
	}
	if _, err := runMemoryCmd(t, "memory", "du", "--visibility", "bot", "--bot", "endy"); err == nil {
		t.Fatalf("memory du accepted a space with no --name")
	}
}
