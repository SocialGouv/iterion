package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// twoDockers plants two executables named docker, each announcing which
// directory it came from. Returns the two directories; the caller decides
// which one PATH names, and when.
func twoDockers(t *testing.T) (dirA, dirB string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("the stand-in runtime is a shell script")
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	dirA, dirB = t.TempDir(), t.TempDir()
	for dir, says := range map[string]string{dirA: "A", dirB: "B"} {
		// `echo` is a shell builtin: the child resolves no PATH of its own,
		// so what it prints names the binary that ran and nothing else.
		if err := os.WriteFile(filepath.Join(dir, string(RuntimeDocker)), []byte("#!/bin/sh\necho "+says+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dirA, dirB
}

// The binary the probe accepted is the binary that runs. [Detect] resolves
// the container CLI through the envtrust seam; the CLI it returns carries
// that ABSOLUTE path, and every spawn in this package — the management
// commands and the workload exec alike — runs it.
//
// The second lookup is what this witness denies. Detect used to return the
// runtime's NAME alone, so each spawn passed "docker" to exec.CommandContext
// and Go answered from whatever PATH said THEN: a different question from
// the one Detect answered. Here PATH moves between the probe and the spawn,
// and the spawn must still run what was probed.
//
// Red when CLI.command goes back to spawning string(c.rt): the impostor in
// dirB is then what runs, and the assertions report B.
func TestTheRuntimeDetectResolvedIsTheOneThatSpawns(t *testing.T) {
	dirA, dirB := twoDockers(t)
	t.Setenv("PATH", dirA)

	cli, err := Detect()
	if err != nil {
		t.Fatalf("Detect on a host with a docker on PATH: %v", err)
	}
	if cli.Runtime() != RuntimeDocker || cli.String() != "docker" {
		t.Fatalf("Detect = %q (runtime %q), want docker's identity", cli, cli.Runtime())
	}
	if want := filepath.Join(dirA, string(RuntimeDocker)); cli.path != want {
		t.Fatalf("Detect kept %q, want the absolute path it resolved (%q)", cli.path, want)
	}

	// The probe is over. PATH now names another docker entirely.
	t.Setenv("PATH", dirB)
	out, err := cli.cmdContext(context.Background(), "version").Output()
	if err != nil {
		t.Fatalf("spawning the runtime Detect resolved: %v", err)
	}
	if got := string(out); got != "A\n" {
		t.Errorf("a management command ran the docker that says %q, want the one Detect accepted in %s — a spawn that "+
			"re-asks the PATH does not run the binary the probe certified; PATH now names %s", got, dirA, dirB)
	}

	// The workload exec is the same question: it keeps the user's locale,
	// not the management scrub, but it runs the same resolved binary.
	r := &Run{
		driver:      &Driver{cli: cli, logger: iterlog.Nop()},
		containerID: "deadbeef",
		prepared:    &Prepared{cli: cli},
	}
	cmd := r.Command(context.Background(), []string{"true"}, sandbox.ExecOpts{})
	if want := filepath.Join(dirA, string(RuntimeDocker)); cmd.Path != want {
		t.Errorf("the workload exec runs %q, want %q", cmd.Path, want)
	}
}

// A CLI nobody resolved refuses rather than falling back to the runtime's
// bare name: the fallback would consult exactly the PATH this type exists
// to stop consulting, and silently. [Detect] is the only constructor.
func TestAZeroCLIRefusesInsteadOfSpawningABareName(t *testing.T) {
	cmd := CLI{rt: RuntimeDocker}.cmdContext(context.Background(), "ps")
	if cmd.Err == nil {
		t.Fatal("a CLI with no resolved path built a runnable command: the spawn would resolve one from the live PATH")
	}
	if err := cmd.Run(); err == nil {
		t.Error("running a command built from an unresolved CLI must fail, loudly")
	}
}
