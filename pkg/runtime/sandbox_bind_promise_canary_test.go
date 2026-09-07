package runtime

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #834, the canary. On a driver with no host filesystem the runtime
// DECLARES host bind mounts, makes promises on their behalf, and then
// drops the binds — leaving the promises. Measured twice: the run-files
// variable naming a directory the pod never had (#815, four lots read
// "oracle RED" out of "Directory nonexistent"), and the bundle bind whose
// devbox provisioning bakes `cp /run/iterion/bundle/devbox.json …` into
// post_create and fails soft, so a bot's declared toolchain silently
// provisions nothing on cloud.
//
// The rule this pins: every path the runtime NAMES to the container — in
// `spec.Env` and in `spec.PostCreate` — must be served by a mount the
// driver actually keeps. It walks the REAL wiring twice, once on a driver
// that honours host binds and once on one that does not, so the dropped
// targets are measured rather than listed.

// specCapturingDriver records the spec handed to Prepare and then refuses
// to Start: the canary reads what the runtime BUILT, and nothing must run.
type specCapturingDriver struct {
	hostBinds bool
	captured  sandbox.Spec
}

var errCanaryStop = errors.New("canary: not starting")

func (d *specCapturingDriver) Name() string { return "docker" }

func (d *specCapturingDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsMounts:         true,
		SupportsHostBindMounts: d.hostBinds,
		SupportsPostCreate:     true,
		SupportsRemoteUser:     true,
	}
}

func (d *specCapturingDriver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	d.captured = spec
	return nil, errCanaryStop
}

func (d *specCapturingDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, errCanaryStop
}

// buildSandboxSpecFor runs the production mount wiring against a driver
// with the given host-bind capability and returns the spec it produced.
func buildSandboxSpecFor(t *testing.T, hostBinds bool, p SandboxParams) sandbox.Spec {
	t.Helper()
	t.Setenv("ITERION_MODE", "local") // pin the factory preference to docker,podman,noop
	d := &specCapturingDriver{hostBinds: hostBinds}
	p.Drivers = map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return d, nil },
	}
	p.Logger = iterlog.Nop()
	p.EmitEvent = func(store.EventType, map[string]any) error { return nil }
	if _, err := resolveAndStartSandbox(context.Background(), p); !errors.Is(err, errCanaryStop) {
		t.Fatalf("resolveAndStartSandbox = %v, want the canary driver's refusal — the wiring did not reach Prepare, so this test proves nothing", err)
	}
	return d.captured
}

// canaryParams is a run carrying every optional host bind the runtime
// declares: a bundle (with a devbox.json, so the provisioning fires),
// attachments, and a run-files directory.
func canaryParams(t *testing.T) SandboxParams {
	t.Helper()
	bundleDir := t.TempDir()
	writeDevboxConfig(t, bundleDir, "go-containerregistry@latest")
	return SandboxParams{
		Workflow: &ir.Workflow{
			Name:    "wf",
			Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/sbx:test", HostState: "none"},
		},
		RunID:                    "run-canary",
		WorkspacePath:            t.TempDir(),
		AttachmentsHostDir:       t.TempDir(),
		AttachmentsContainerPath: attachmentsContainerPath,
		RunFilesHostDir:          t.TempDir(),
		RunFilesContainerPath:    "/iterion/artifact-files",
		BundleHostDir:            bundleDir,
		BundleContainerPath:      "/run/iterion/bundle",
	}
}

func TestSandboxSpec_NoPromiseSurvivesTheBindThatServedIt(t *testing.T) {
	p := canaryParams(t)
	withBinds := buildSandboxSpecFor(t, true, p)
	poddish := buildSandboxSpecFor(t, false, p)

	dropped := droppedMountTargets(withBinds.Mounts, poddish.Mounts)
	if len(dropped) == 0 {
		t.Fatal("no host bind was dropped — the fixture declares none, so the canary would pass vacuously")
	}
	// The two halves of the walk must each have something to find, or a
	// green run proves nothing: a variable the runtime sets on a bind
	// (#815's own), and a snippet it bakes on one (the devbox prologue).
	if withBinds.Env[runFilesEnvVar] == "" {
		t.Fatal("the bind-capable spec sets no run-files variable — the Env half of the walk is vacuous")
	}
	if !strings.Contains(withBinds.PostCreate, "devbox install") {
		t.Fatalf("the bind-capable spec bakes no devbox prologue — the post_create half of the walk is vacuous:\n%s", withBinds.PostCreate)
	}

	for key, value := range poddish.Env {
		if target := underAny(value, dropped); target != "" {
			t.Errorf("spec.Env[%s] = %s, under the dropped bind %s — the runtime promises a directory the container never had", key, value, target)
		}
	}
	// A promise is not only a variable. The devbox provisioning bakes
	// `cp <bundle>/devbox.json …` into post_create, and that path is
	// exactly a dropped bind's target.
	for _, ref := range absolutePathsIn(poddish.PostCreate) {
		if target := underAny(ref, dropped); target != "" {
			t.Errorf("post_create names %s, under the dropped bind %s — the snippet runs against a path the container never had (it fails soft, so a bot's devbox.json silently provisions nothing)", ref, target)
		}
	}
}

// droppedMountTargets returns the mount targets present in `all` and gone
// from `kept`, cleaned.
func droppedMountTargets(all, kept []string) []string {
	keptTargets := map[string]bool{}
	for _, m := range kept {
		if tgt := mountTarget(m); tgt != "" {
			keptTargets[path.Clean(tgt)] = true
		}
	}
	var out []string
	for _, m := range all {
		tgt := mountTarget(m)
		if tgt == "" {
			continue
		}
		if c := path.Clean(tgt); !keptTargets[c] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// underAny returns the dropped target `value` sits under, or "".
func underAny(value string, dropped []string) string {
	v := path.Clean(strings.TrimSpace(value))
	if v == "" || v == "." || !path.IsAbs(v) {
		return ""
	}
	for _, d := range dropped {
		if v == d || strings.HasPrefix(v, d+"/") {
			return d
		}
	}
	return ""
}

// absolutePathsIn pulls every absolute-looking path out of a shell
// snippet. Deliberately coarse: a promise the runtime bakes into
// post_create is a literal path, and a false positive here is a mount the
// driver kept, which the caller filters on.
func absolutePathsIn(snippet string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(snippet, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '"' || r == '\'' || r == ';' || r == '{' || r == '}' || r == '&' || r == '|'
	}) {
		if strings.HasPrefix(field, "/") {
			out = append(out, field)
		}
	}
	return out
}
