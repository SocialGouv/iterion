package docker

// The driver border of the plan-to-create window (#2069, H-10 round 2):
// the runtime verifies a bind source when the plan is built — before the
// pull, which is minutes — but the daemon resolves the source at CREATION.
// A sandboxed run alive through that window can swap the verified leaf for
// a symlink and choose where the mount lands. The spec names what the plan
// verified (Spec.BindSourceChecks); Start re-asks the same question at the
// last driver-controlled point, and a source that no longer verifies
// refuses the creation.
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
