package docker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestBuildImageRef_DeterministicAndDockerSafe(t *testing.T) {
	cases := []struct {
		name, runID, want string
	}{
		{"underscore-to-dash", "run_abc123", "iterion-sandbox-build:run-abc123"},
		{"uppercase-lowered", "RUN_XYZ", "iterion-sandbox-build:run-xyz"},
		{"long-trimmed", strings.Repeat("a", 200), "iterion-sandbox-build:" + strings.Repeat("a", 128)},
	}
	for _, tc := range cases {
		got := buildImageRef(tc.runID)
		if got != tc.want {
			t.Errorf("%s: buildImageRef(%q) = %q, want %q", tc.name, tc.runID, got, tc.want)
		}
	}
}

func TestBuild_NoOpWhenSpecHasNoBuild(t *testing.T) {
	d := &Driver{rt: "docker"}
	prepared := &Prepared{spec: sandbox.Spec{Image: "alpine:3.20"}, workspace: "/workspace", runtime: "docker"}
	got, err := d.Build(context.Background(), prepared,
		sandbox.RunInfo{RunID: "run_x", WorkspacePath: "/tmp"})
	if err != nil {
		t.Fatalf("Build with nil spec.Build must be a no-op: %v", err)
	}
	if got != prepared {
		t.Error("Build with nil spec.Build must return the input prepared unchanged")
	}
}

func TestBuild_RequiresWorkspacePath(t *testing.T) {
	d := &Driver{rt: "docker"}
	_, err := d.Build(context.Background(),
		&Prepared{
			spec:      sandbox.Spec{Build: &sandbox.Build{Dockerfile: "Dockerfile"}},
			workspace: "/workspace",
			runtime:   "docker",
		},
		sandbox.RunInfo{RunID: "run_x"}) // WorkspacePath empty
	if err == nil {
		t.Fatal("expected error when WorkspacePath is empty")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("error must mention workspace, got: %v", err)
	}
}

func TestBuild_ArgvShape(t *testing.T) {
	var captured []string
	old := runBuildx
	runBuildx = func(_ context.Context, _ Runtime, args []string, _ io.Writer) error {
		captured = args
		return nil
	}
	t.Cleanup(func() { runBuildx = old })

	d := &Driver{rt: "docker"}
	prepared := &Prepared{
		spec: sandbox.Spec{
			Build: &sandbox.Build{
				Dockerfile: "build/Dockerfile.dev",
				Context:    ".",
				Args:       map[string]string{"VERSION": "1.2.3", "ALPHA": "yes"},
			},
			User: "1000:1000",
		},
		workspace: "/workspace",
		runtime:   "docker",
	}
	out, err := d.Build(context.Background(), prepared,
		sandbox.RunInfo{RunID: "run_xyz", WorkspacePath: "/repo"})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	want := []string{
		"buildx", "build",
		"-f", "/repo/build/Dockerfile.dev",
		"-t", "iterion-sandbox-build:run-xyz",
		"--load",
		// Sorted: ALPHA before VERSION.
		"--build-arg", "ALPHA=yes",
		"--build-arg", "VERSION=1.2.3",
		"/repo",
	}
	if !sliceEqual(captured, want) {
		t.Errorf("argv mismatch.\n got=%q\nwant=%q", captured, want)
	}

	p, ok := out.(*Prepared)
	if !ok {
		t.Fatalf("expected *Prepared, got %T", out)
	}
	if p.spec.Image != "iterion-sandbox-build:run-xyz" {
		t.Errorf("returned spec.Image = %q, want freshly-built ref", p.spec.Image)
	}
	if p.spec.Build != nil {
		t.Error("returned spec.Build must be nil after Build consumes it")
	}
}

func TestBuild_SurfacesStderrTail(t *testing.T) {
	old := runBuildx
	runBuildx = func(_ context.Context, _ Runtime, _ []string, stderr io.Writer) error {
		_, _ = stderr.Write([]byte("ERROR: failed to solve: dockerfile parse error: unknown instruction WHAT"))
		return errors.New("exit status 1")
	}
	t.Cleanup(func() { runBuildx = old })

	d := &Driver{rt: "docker"}
	_, err := d.Build(context.Background(),
		&Prepared{
			spec:      sandbox.Spec{Build: &sandbox.Build{Dockerfile: "Dockerfile"}, User: "1000:1000"},
			workspace: "/workspace",
			runtime:   "docker",
		},
		sandbox.RunInfo{RunID: "run_q", WorkspacePath: "/tmp"})
	if err == nil {
		t.Fatal("expected error when buildx exits non-zero")
	}
	if !strings.Contains(err.Error(), "failed to solve") {
		t.Errorf("error must surface buildx stderr; got: %v", err)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
