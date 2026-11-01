package docker

// The container ID the driver keeps is what every later lifecycle command
// addresses — exec, stop, rm. `docker run --detach` prints it on stdout and
// keeps stderr for what the environment decided: "WARNING: Published ports
// are discarded when using host network mode" (measured on docker 29.9.2),
// the platform-mismatch notice for an image built for another architecture
// (the driver passes no --platform), podman's WARN[0000] lines. One such
// line read as part of the answer leaves the driver holding an ID no
// container has, while the container it created runs on to its own exit.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// containerIDWitnessID is the answer the shim runtime gives on stdout — the
// 64-hex shape of a real container ID.
const containerIDWitnessID = "9cafebd8561b38c93614c35680cf8293e59d2ca4e41ce1828ec5022b936ec1f2"

// chattyRuntime installs a container-runtime shim that answers `run` with
// one warning on stderr and the ID on stdout, and records every invocation's
// argv so the test can read which container the later commands addressed.
// Returns the runtime name and the path of the argv log.
func chattyRuntime(t *testing.T) (Runtime, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	rt := Runtime("iterion-test-chatty-runtime")
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in\n" +
		"run)\n" +
		"  printf 'WARNING: Published ports are discarded when using host network mode\\n' >&2\n" +
		"  printf '" + containerIDWitnessID + "\\n'\n" +
		"  ;;\n" +
		"inspect)\n" +
		"  printf 'private\\n'\n" +
		"  ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, string(rt)), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rt, log
}

func TestStart_ReadsTheContainerIDFromStdoutWhileTheRuntimeWarns(t *testing.T) {
	rt, argvLog := chattyRuntime(t)
	spec := sandbox.Spec{
		Mode:            sandbox.ModeInline,
		Image:           "iterion-container-id-witness:never-pulled",
		WorkspaceFolder: LegacyDefaultWorkspace,
	}
	d := &Driver{rt: rt, logger: iterlog.Nop()}
	prepared := preparedWithoutPrepare(t, spec)
	prepared.runtime = rt

	run, err := d.Start(context.Background(), prepared, sandbox.RunInfo{
		RunID:         "container-id-witness",
		WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = run.Cleanup(context.Background()) })

	dr, ok := run.(*Run)
	if !ok {
		t.Fatalf("Start returned %T, want *Run", run)
	}
	if dr.containerID != containerIDWitnessID {
		t.Fatalf("containerID = %q, want %q — the runtime's warning was read as part of the answer", dr.containerID, containerIDWitnessID)
	}

	// The effect, not the field: the commands that follow address the
	// container that was created.
	if err := run.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the shim recorded no invocation: %v", err)
	}
	want := "rm --force " + containerIDWitnessID
	if !strings.Contains(string(argv), want) {
		t.Fatalf("no lifecycle command addressed the created container (want a line carrying %q), so the removal named something else:\n%s", want, argv)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if strings.Contains(line, "WARNING") {
			t.Fatalf("a lifecycle command carries the runtime's warning as its container argument: %q", line)
		}
	}
}
