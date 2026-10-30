package docker

// The driver border of the plan-to-create window (#2069, H-10 round 2):
// the runtime verifies a bind source when the plan is built — before the
// pull, which is minutes — but the daemon resolves the source at CREATION.
// A sandboxed run alive through that window can swap the verified leaf for
// a symlink and choose where the mount lands. The spec names what the plan
// verified (Spec.BindSourceChecks); Start re-asks the same question at the
// last driver-controlled point, and a source that no longer verifies
// refuses the creation. The question (sandbox.AnswerBindSourceCheck)
// answers by the source's shape: a directory keeps the store ask, a file
// identity alone — never creation — and a source gone since the plan
// refuses whatever its shape.
//
// These witnesses run without a container runtime: the refusal fires before
// the create command is spawned, and the control reaches the create command
// (which fails on the missing image — the probe image is never pulled)
// without producing the bind-source refusal.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/safepath"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

const toctouRefusalPhrase = "swapped between plan and creation"

// preparedWithoutPrepare builds the handle Start consumes, skipping the
// image pull: the window under test is plan -> creation, and the pull is
// exactly the stretch of it the runtime cannot see.
func preparedWithoutPrepare(t *testing.T, spec sandbox.Spec) *Prepared {
	t.Helper()
	if err := spec.Validate(); err != nil {
		t.Fatalf("the witness spec must validate: %v", err)
	}
	return &Prepared{spec: spec, runtime: RuntimeDocker}
}

// The swap, played by a run alive through the window: the leaf was a real
// directory when the plan verified it, and a symlink by the time Start
// creates the container. The creation must be refused, naming the swap.
func TestABindSourceSwappedBetweenPlanAndCreationRefusesTheStart(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	leaf := filepath.Join(root, "store", "scratch")
	if err := safepath.EnsureRealBindSourceDir(root, leaf, 0o700); err != nil {
		t.Fatalf("the plan-time ask must pass on the real leaf: %v", err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		Mounts:           []string{fmt.Sprintf("source=%s,target=/tmp/iterion-scratch,type=bind", leaf)},
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: leaf}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	// The swap.
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, leaf); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "toctou-witness",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Start created a container whose verified bind source was swapped in the window")
	}
	if !strings.Contains(err.Error(), toctouRefusalPhrase) {
		t.Errorf("the refusal must name the swap it caught — an operator reading the run log must find the window, not a docker error: %v", err)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal must name where the swapped leaf now points: %v", err)
	}
}

// The control: the same spec with the leaf still real must pass the
// re-ask and reach the create command — which fails on the image this
// witness never pulls, NOT on the bind source. A control that refused
// everything would be a sandbox that can no longer start.
func TestABindSourceStillRealReachesTheCreateCommand(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "store", "scratch")
	if err := safepath.EnsureRealBindSourceDir(root, leaf, 0o700); err != nil {
		t.Fatalf("the plan-time ask must pass on the real leaf: %v", err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		Mounts:           []string{fmt.Sprintf("source=%s,target=/tmp/iterion-scratch,type=bind", leaf)},
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: leaf}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "toctou-control",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("the control expected the create command to fail on the never-pulled image")
	}
	if strings.Contains(err.Error(), toctouRefusalPhrase) {
		t.Errorf("the re-ask refused a source that is still the path it is named — the check has a false positive: %v", err)
	}
}

// The workspace is the one bind source the DRIVER derives from run metadata
// (RunInfo.WorkspacePath) instead of from the mount plan — read-write, and
// for a run on an engine worktree under the iterion home a living run holds
// read-write. Its shape at this border differs from the store-backed
// sources in one promise: the tree is what the ENGINE laid for this run, so
// a source swapped for a symlink refuses the creation, naming where the
// swap points.
func TestTheEngineLaidWorkspaceSwappedBetweenPlanAndCreationRefusesTheStart(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wt := filepath.Join(root, "projects", "k", "worktrees", "run-witness")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:            sandbox.ModeInline,
		Image:           "iterion-toctou-witness:never-pulled",
		WorkspaceFolder: LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{
			{Root: root, Path: wt, MustExist: true},
		},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	// The swap a run alive through the window plays on the next run's
	// worktree, through its own read-write home bind.
	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, wt); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "workspace-swap-witness",
		WorkspacePath: wt,
	})
	if err == nil {
		t.Fatal("Start created a container whose engine-laid workspace source was swapped in the window")
	}
	if !strings.Contains(err.Error(), toctouRefusalPhrase) {
		t.Errorf("the refusal must name the swap it caught — an operator reading the run log must find the window, not a docker error: %v", err)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal must name where the swapped workspace now points: %v", err)
	}
}

// The other half of the workspace's promise: nothing stands in for the tree
// the engine laid. A store-backed source that went missing is recreated
// cold (the scratch and the caches skip cold for the same absence); a
// MISSING WORKSPACE is not recreated — an empty stand-in directory would
// start the run on a tree the engine never laid, and the border refuses
// instead, without mkdir-ing the path back.
func TestTheWorkspaceGoneSinceThePlanRefusesInsteadOfRecreating(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "projects", "k", "worktrees", "run-gone")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:            sandbox.ModeInline,
		Image:           "iterion-toctou-witness:never-pulled",
		WorkspaceFolder: LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{
			{Root: root, Path: wt, MustExist: true},
		},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	// Gone since the plan verified it.
	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "workspace-gone-witness",
		WorkspacePath: wt,
	})
	if err == nil {
		t.Fatal("Start created a container whose workspace source went missing after the plan")
	}
	if !strings.Contains(err.Error(), "is gone") {
		t.Errorf("the refusal must name the disappearance, not a docker error: %v", err)
	}
	if _, statErr := os.Lstat(wt); !os.IsNotExist(statErr) {
		t.Errorf("the border recreated the missing workspace source (%v) — a cold recreation is the store-backed posture, never the engine-laid one", statErr)
	}
}

// The control for both: the engine-laid tree still real and still present
// passes the re-ask and reaches the create command, which fails on the
// never-pulled image — never on the workspace source.
func TestTheEngineLaidWorkspaceStillRealReachesTheCreateCommand(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "projects", "k", "worktrees", "run-control")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:            sandbox.ModeInline,
		Image:           "iterion-toctou-witness:never-pulled",
		WorkspaceFolder: LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{
			{Root: root, Path: wt, MustExist: true},
		},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "workspace-control",
		WorkspacePath: wt,
	})
	if err == nil {
		t.Fatal("the control expected the create command to fail on the never-pulled image")
	}
	if strings.Contains(err.Error(), toctouRefusalPhrase) || strings.Contains(err.Error(), "is gone") {
		t.Errorf("the re-ask refused the real engine-laid workspace — the check has a false positive: %v", err)
	}
}

// The FILE class of verified source: a copy seed (plugins.yaml under the
// iterion home, a worktree's .git/config) or a host-bound state file rides
// the record next to the directories. The border used to run its
// create-on-missing ask on it — a MkdirAll: ENOTDIR on the honest file, so
// EVERY honest container was refused, and a stand-in DIRECTORY where the
// operator keeps a file when the file went missing. The border answers a
// file by identity alone, never creation.
func TestAVerifiedFileSourceReachesTheCreateCommand(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "plugins.yaml")
	if err := os.WriteFile(file, []byte("enable: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: file}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "file-control",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("the control expected the create command to fail on the never-pulled image")
	}
	if strings.Contains(err.Error(), toctouRefusalPhrase) || strings.Contains(err.Error(), "is gone") {
		t.Errorf("the border refused an honest verified FILE: %v", err)
	}
}

// The file keeps the swap promise the directories hold: a symlink swapped
// in for the verified file inside the window refuses the creation, naming
// where the swap points.
func TestAFileSourceSwappedForASymlinkRefusesTheStart(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(root, "plugins.yaml")
	if err := os.WriteFile(file, []byte("enable: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: file}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	// The swap.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "file-swap-witness",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Start created a container whose verified file source was swapped in the window")
	}
	if !strings.Contains(err.Error(), toctouRefusalPhrase) {
		t.Errorf("the refusal must name the swap it caught: %v", err)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal must name where the swapped file now points: %v", err)
	}
}

// The other half of the file's answer: gone since the plan, it refuses —
// and nothing stands in its place. The create-on-missing posture would
// mkdir a DIRECTORY over the operator's file path, corrupting the layout
// for every run after this one.
func TestAFileSourceGoneSinceThePlanRefusesInsteadOfRecreatingADirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "plugins.yaml")
	if err := os.WriteFile(file, []byte("enable: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: file}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "file-gone-witness",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Start created a container whose verified file source went missing after the plan")
	}
	if !strings.Contains(err.Error(), "is gone") {
		t.Errorf("the refusal must name the disappearance, not a docker error: %v", err)
	}
	if _, statErr := os.Lstat(file); !os.IsNotExist(statErr) {
		t.Errorf("the border recreated the missing file source as %v — a file's path is never a store to lay a directory over", statErr)
	}
}

// The same refusal for a store DIRECTORY gone since the plan: the plan
// lays every source it mounts, so an absence at this border is a window
// event, and the border recreates nothing the window removed. (Pinned so a
// future "helpful" recreate is a conscious red: recreation at the border
// would paper over exactly the change this field exists to catch.)
func TestADirectorySourceGoneSinceThePlanRefusesInsteadOfRecreating(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "store", "scratch")
	if err := safepath.EnsureRealBindSourceDir(root, leaf, 0o700); err != nil {
		t.Fatalf("the plan-time ask must pass on the real leaf: %v", err)
	}
	spec := sandbox.Spec{
		Mode:             sandbox.ModeInline,
		Image:            "iterion-toctou-witness:never-pulled",
		WorkspaceFolder:  LegacyDefaultWorkspace,
		BindSourceChecks: []sandbox.BindSourceCheck{{Root: root, Path: leaf}},
	}
	d := &Driver{rt: RuntimeDocker}
	prepared := preparedWithoutPrepare(t, spec)

	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}

	_, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "dir-gone-witness",
		WorkspacePath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Start created a container whose verified directory source went missing after the plan")
	}
	if !strings.Contains(err.Error(), "is gone") {
		t.Errorf("the refusal must name the disappearance, not a docker error: %v", err)
	}
	if _, statErr := os.Lstat(leaf); !os.IsNotExist(statErr) {
		t.Errorf("the border recreated the missing directory source (%v) — the border recreates nothing the window removed", statErr)
	}
}
