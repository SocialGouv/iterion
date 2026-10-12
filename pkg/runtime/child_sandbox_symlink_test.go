package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The bytes every tree the run does not own holds, so a rewrite through a link
// is as visible as a deletion.
const outsideClaudeBody = "not the run's to touch\n"

// The bytes the parent's own `.claude` entries hold, written by the test and
// expected back from a backup: a backup still holding them can restore the
// parent, one holding anything else cannot.
const parentClaudeBody = "the parent's own\n"

// assertBackupKeptTheParentsEntry re-reads an entry inside the backup a
// refused restore names. Naming the backup is worth nothing unless the
// parent's BYTES are still in it: a refusal that removed the directory, swept
// the entries inside it or rewrote them would be the step that destroys what
// the guard exists to protect. Both refusals that keep a backup — the host
// restore and the in-sandbox one — are held to this.
func assertBackupKeptTheParentsEntry(t *testing.T, entry string) {
	t.Helper()
	body, err := os.ReadFile(entry)
	if err != nil || string(body) != parentClaudeBody {
		t.Errorf("the refused restore destroyed the parent's only copy at %s: %q, %v", entry, body, err)
	}
}

// testBackupName is a name no other run can hold: the snapshot creates
// /tmp/<name> inside the sandbox, and these tests assert on its absence. A
// REFUSED restore keeps its backup on purpose (the parent's resources are
// recoverable there), so the test removes the directory it had made.
func testBackupName(t *testing.T) string {
	t.Helper()
	name := "iterion-child-resources-test-" + rand.Text()
	t.Cleanup(func() { _ = os.RemoveAll(path.Join("/tmp", name)) })
	return name
}

// plantOutsideClaudeTree fills a tree the run does not own with the entries the
// shared-resource scripts act on. It is the test's own t.TempDir().
func plantOutsideClaudeTree(t *testing.T, outside string) {
	t.Helper()
	writeClaudeFile(t, outside, "settings.json", outsideClaudeBody)
	writeClaudeFile(t, outside, "skills/not-ours/SKILL.md", outsideClaudeBody)
	writeClaudeFile(t, outside, "commands/not-ours.md", outsideClaudeBody)
}

// assertOutsideClaudeTreeUntouched re-reads that tree: every entry still there,
// byte for byte. A script that ran through the link deleted them (rm -rf) or
// replaced their bytes (cp -a).
func assertOutsideClaudeTreeUntouched(t *testing.T, outside string) {
	t.Helper()
	for _, rel := range []string{"settings.json", "skills/not-ours/SKILL.md", "commands/not-ours.md"} {
		body, err := os.ReadFile(filepath.Join(outside, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s is gone from the tree the link points at: %v", rel, err)
		}
		if string(body) != outsideClaudeBody {
			t.Fatalf("%s = %q, want %q — a script wrote through the link", rel, body, outsideClaudeBody)
		}
	}
}

// linkedClaudeSandbox is the shape the refusal exists for: a copy-based sandbox
// whose workspace `.claude` is a SYMLINK to a tree outside it. Both trees are
// the test's own temporary directories.
func linkedClaudeSandbox(t *testing.T) (e *Engine, workspace, outside string) {
	t.Helper()
	workspace, outside = t.TempDir(), t.TempDir()
	plantOutsideClaudeTree(t, outside)
	if err := os.Symlink(outside, filepath.Join(workspace, ".claude")); err != nil {
		t.Fatalf("symlink .claude: %v", err)
	}
	return k8sShapedEngine(t, workspace, copySandboxRun{root: workspace}), workspace, outside
}

// The in-sandbox snapshot reads `$1/<name>` for every borrowed entry. Through a
// `.claude` link it copies the link target's files into the backup — which the
// restore then writes into the workspace as the parent's own resources. It must
// refuse before the backup exists, with the typed refusal the host sites return.
//
// This script only READS through the root, so the backup is where this site's
// damage lands: its absence is the witness, and the outside tree the other two
// witnesses re-read cannot change here whatever the guard does.
func TestTheSharedSnapshotRefusesASymlinkedClaudeRoot(t *testing.T) {
	e, _, _ := linkedClaudeSandbox(t)
	name := testBackupName(t)

	_, err := e.snapshotSharedChildResources(context.Background(), name)

	if _, statErr := os.Stat(path.Join("/tmp", name)); !os.IsNotExist(statErr) {
		t.Errorf("the snapshot copied the link target into its backup before refusing (stat %v)", statErr)
	}
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("the in-sandbox snapshot through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
}

// The destructive half: the restore does `rm -rf "$1/<name>"` for every
// borrowed entry, then copies the backup back. Through a `.claude` link swapped
// in mid-run — the agent inside the sandbox is exactly who can plant it — that
// deletes and rewrites the link target. It must refuse, loudly and typed.
func TestTheSharedRestoreRefusesASymlinkedClaudeRoot(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	writeClaudeFile(t, workspace, ".claude/settings.json", parentClaudeBody)
	e := k8sShapedEngine(t, workspace, copySandboxRun{root: workspace})
	name := testBackupName(t)
	restore, err := e.snapshotSharedChildResources(context.Background(), name)
	if err != nil {
		t.Fatalf("snapshot of a real .claude directory: %v", err)
	}

	// The swap: `.claude` becomes a link to a tree the run does not own.
	plantOutsideClaudeTree(t, outside)
	if err := os.RemoveAll(filepath.Join(workspace, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".claude")); err != nil {
		t.Fatalf("symlink .claude: %v", err)
	}

	restoreErr := restore()

	assertOutsideClaudeTreeUntouched(t, outside)
	// The backup is the parent's only copy of its `.claude` entries inside the
	// sandbox: the host-side restore repairs the host tree, nothing else holds
	// this one.
	assertBackupKeptTheParentsEntry(t, path.Join("/tmp", name, "settings.json"))
	var linkErr *claudeSymlinkError
	if !errors.As(restoreErr, &linkErr) {
		t.Fatalf("the in-sandbox restore through a mid-run .claude symlink = %v, want the typed *claudeSymlinkError refusal", restoreErr)
	}
}

// The reset empties every borrowed entry before the child's own resources are
// written through. It is `rm -rf` and nothing else, so a `.claude` link makes it
// a recursive delete outside the workspace with no copy anywhere.
func TestTheBorrowedResourceResetRefusesASymlinkedClaudeRoot(t *testing.T) {
	e, workspace, outside := linkedClaudeSandbox(t)

	err := e.clearBorrowedSandboxResources(context.Background())

	assertOutsideClaudeTreeUntouched(t, outside)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("the in-sandbox reset through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	// Only the sandbox can read the link; the operator fixes it from this message.
	if !strings.Contains(err.Error(), filepath.Join(workspace, ".claude")) || !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal names neither the link nor its target: %v", err)
	}
	// The typed error carries the host sites' wording, which is about a mirror
	// writing through the link; the hazard here is `rm -rf` of the link's
	// target. The script's own sentence states it, and only the script's
	// stderr can carry it to the operator.
	if !strings.Contains(err.Error(), "would remove and rewrite through it, outside the workspace") {
		t.Errorf("the refusal drops the script's own account of what it refused: %v", err)
	}
}

// The false positive the refusal must not have: a workspace REACHED through a
// symlink, whose `.claude` is a real directory. Both sides of the workspace link
// are the same tree — the host guard examines the final component alone, and the
// in-sandbox one holds the same line. A guard resolving the whole path and
// comparing it to the workspace would refuse this, and every run under a
// symlinked checkout with it.
func TestASymlinkedWorkspaceKeepsItsRealClaudeDirectory(t *testing.T) {
	realWorkspace := t.TempDir()
	writeClaudeFile(t, realWorkspace, ".claude/settings.json", parentClaudeBody)
	writeClaudeFile(t, realWorkspace, ".claude/skills/kept/SKILL.md", parentClaudeBody)
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realWorkspace, link); err != nil {
		t.Fatalf("symlink the workspace: %v", err)
	}
	e := k8sShapedEngine(t, link, copySandboxRun{root: link})

	restore, err := e.snapshotSharedChildResources(context.Background(), testBackupName(t))
	if err != nil {
		t.Fatalf("the snapshot refused a real .claude directory reached through a workspace link: %v", err)
	}
	// A child replaces the parent's resources; the restore puts them back.
	if err := os.RemoveAll(filepath.Join(realWorkspace, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	writeClaudeFile(t, realWorkspace, ".claude/settings.json", "the child's\n")
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(realWorkspace, ".claude", "settings.json")); err != nil || string(body) != parentClaudeBody {
		t.Errorf("settings.json = %q (err %v), want the parent's bytes back", body, err)
	}
	if _, err := os.Stat(filepath.Join(realWorkspace, ".claude", "skills", "kept", "SKILL.md")); err != nil {
		t.Errorf("the parent's skill was not restored: %v", err)
	}

	if err := e.clearBorrowedSandboxResources(context.Background()); err != nil {
		t.Fatalf("the reset refused a real .claude directory reached through a workspace link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realWorkspace, ".claude", "skills")); !os.IsNotExist(err) {
		t.Errorf("the reset left the borrowed skills in place (err %v) — it acted somewhere else", err)
	}
}

// The two ends of the script-to-engine mapping, which no sandbox can produce on
// demand: a refusal whose link target the sandbox could not read (no `readlink`
// in the image) still refuses, and every OTHER failing exit keeps its own
// message instead of being reported as a symlink.
func TestTheSandboxResourceRefusalMapsOnlyItsOwnExit(t *testing.T) {
	root := "/workspace/.claude"

	unreadable := sharedResourceScriptError("reset borrowed sandbox resources", root,
		sandbox.ExecResult{ExitCode: sandboxClaudeSymlinkExit})
	var linkErr *claudeSymlinkError
	if !errors.As(unreadable, &linkErr) {
		t.Fatalf("a refusal with no target on stdout = %v, want the typed *claudeSymlinkError refusal", unreadable)
	}
	for _, want := range []string{"<unreadable>", root, "reset borrowed sandbox resources"} {
		if !strings.Contains(unreadable.Error(), want) {
			t.Errorf("the refusal does not carry %q, which docs/bundles.md tells the operator to look for: %v", want, unreadable)
		}
	}

	// Exit 4 is the script's contract for the refusal, not a status only the
	// guard can produce: a coreutil exiting exactly 4 lands in the same
	// branch. Its stderr is then the only trace of what really failed, and
	// the symlink the message names does not exist.
	coreutil := sharedResourceScriptError("shared child resources (backup /tmp/b)", root,
		sandbox.ExecResult{ExitCode: sandboxClaudeSymlinkExit,
			Stderr: []byte("cp: cannot create regular file '/workspace/.claude/settings.json': Read-only file system\n")})
	if !strings.Contains(coreutil.Error(), "Read-only file system") {
		t.Errorf("an exit %d that is not the guard lost the only evidence of its cause: %v", sandboxClaudeSymlinkExit, coreutil)
	}

	other := sharedResourceScriptError("reset borrowed sandbox resources", root,
		sandbox.ExecResult{ExitCode: 1, Stderr: []byte("cp: cannot stat")})
	if errors.As(other, &linkErr) {
		t.Errorf("an unrelated failure was reported as a .claude symlink: %v", other)
	}
	if !strings.Contains(other.Error(), "exit 1") || !strings.Contains(other.Error(), "cp: cannot stat") {
		t.Errorf("an unrelated failure lost its exit status or its stderr: %v", other)
	}

	if clean := sharedResourceScriptError("site", root, sandbox.ExecResult{}); clean != nil {
		t.Errorf("a clean exit produced an error: %v", clean)
	}
}

// refusalFirstLineBudget is what the live follow view of a failing run keeps of
// an event's error: the text up to the first newline, bounded to 300 runes
// (pkg/cli's firstLine, under printRemoteEvent). It is where a cloud run's
// failure is first read, so the refusal's first line has to carry the verdict.
const refusalFirstLineBudget = 300

func refusalFirstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	if runes := []rune(line); len(runes) > refusalFirstLineBudget {
		return string(runes[:refusalFirstLineBudget])
	}
	return line
}

// `kubectl exec` appends its own exit-code line to the stderr it captures
// (sandbox.KubectlRemoteExit1 is that line for exit 1), so on the production
// transport the script's account is never the whole first line. Which account
// leads is then what the operator reads: the typed refusal when the sandbox
// read the link's target — the one fact the host cannot read for itself — and
// the script's own account when it did not, that one being the whole account
// of an exit 4 that is not the guard's.
func TestTheSandboxRefusalLeadsWithTheMostSpecificFact(t *testing.T) {
	const root = "/workspace/.claude"
	// The fixture is the real producer's sentence, checked against the script
	// that writes it (with the shell's own positional back in place): a
	// witness fed a kinder stderr than the script's proves nothing.
	said := "shared .claude root " + root + " is a symlink — the snapshot, restore and reset would" +
		" remove and rewrite through it, outside the workspace; replace the link with a real directory"
	if !strings.Contains(assertSharedResourceRoots, strings.Replace(said, root, "$1", 1)) {
		t.Fatalf("the fixture is not the sentence the script writes: %q", said)
	}
	stderr := []byte(said + "\ncommand terminated with exit code " + strconv.Itoa(sandboxClaudeSymlinkExit) + "\n")

	const target = "/home/runner/shared-claude"
	named := sharedResourceScriptError("reset borrowed sandbox resources", root,
		sandbox.ExecResult{ExitCode: sandboxClaudeSymlinkExit, Stdout: []byte(target + "\n"), Stderr: stderr})
	for _, want := range []string{root, target} {
		if !strings.Contains(refusalFirstLine(named), want) {
			t.Errorf("the one-line view of the refusal does not name %q: %q", want, refusalFirstLine(named))
		}
	}
	if !strings.Contains(named.Error(), said) {
		t.Errorf("the refusal dropped the script's own account: %v", named)
	}

	// No `readlink` in the image, or an exit 4 that is not the guard at all:
	// the typed message names no target, so leading with it would push the
	// only real account of the failure out of that one-line view.
	unnamed := sharedResourceScriptError("shared child resources (backup /tmp/b)", root,
		sandbox.ExecResult{ExitCode: sandboxClaudeSymlinkExit,
			Stderr: []byte("cp: cannot create regular file '/workspace/.claude/settings.json': Read-only file system\n" +
				"command terminated with exit code " + strconv.Itoa(sandboxClaudeSymlinkExit) + "\n")})
	if !strings.Contains(refusalFirstLine(unnamed), "Read-only file system") {
		t.Errorf("the one-line view of an exit %d with no target read does not carry its cause: %q",
			sandboxClaudeSymlinkExit, refusalFirstLine(unnamed))
	}
	// Whichever account leads, one errors.As recognises the class.
	var linkErr *claudeSymlinkError
	if !errors.As(unnamed, &linkErr) || !errors.As(named, &linkErr) {
		t.Errorf("a refusal lost its type to the order of its accounts: %v / %v", unnamed, named)
	}
}
