package proc

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// workDirHelperEnv turns the test binary into the child half of the witness
// below. A compiled Go child is the point: a POSIX shell recomputes PWD from
// its real cwd at startup, so a shell fake reports the right directory with
// or without the gesture and cannot redden.
const workDirHelperEnv = "ITERION_PROC_WORKDIR_HELPER"

const workDirHelperPrefix = "childPWD="

// TestSetWorkDirHelperChild prints the PWD its parent handed it. It is not a
// test of its own: without the marker it does nothing.
func TestSetWorkDirHelperChild(t *testing.T) {
	if os.Getenv(workDirHelperEnv) == "" {
		return
	}
	// Written to stderr so the parent reads it whatever the framework puts
	// on stdout.
	if _, err := os.Stderr.WriteString("\n" + workDirHelperPrefix + os.Getenv("PWD") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestSetWorkDirReachesTheChildsEnvironment is the witness for the gesture
// every host spawn of an agent CLI crosses: the CHILD must observe PWD
// naming the directory the spawn was pointed at, not the engine's cwd.
func TestSetWorkDirReachesTheChildsEnvironment(t *testing.T) {
	dir := t.TempDir()

	t.Run("a non-shell child reads the spawn's directory", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSetWorkDirHelperChild$") // #nosec G204 — the test binary itself.
		cmd.Env = append(os.Environ(), workDirHelperEnv+"=1")
		SetWorkDir(cmd, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper child: %v: %s", err, out)
		}
		got := ""
		for _, line := range strings.Split(string(out), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), workDirHelperPrefix); ok {
				got = rest
			}
		}
		if got != dir {
			t.Fatalf("the child read PWD=%q, want the spawn's directory %q — a CLI that resolves its project from PWD would work on the engine's checkout", got, dir)
		}
	})

	t.Run("the real working directory is set too", func(t *testing.T) {
		cmd := exec.Command("true") // #nosec G204 — a constant.
		cmd.Env = []string{"A=1"}
		SetWorkDir(cmd, dir)
		if cmd.Dir != dir {
			t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, dir)
		}
		if last := cmd.Env[len(cmd.Env)-1]; last != "PWD="+dir {
			t.Fatalf("last env entry = %q, want PWD=%q — os/exec keeps the LAST value for a duplicate key", last, dir)
		}
	})

	// A nil Env means "inherit"; appending to it would hand the child an
	// environment of exactly one entry — no PATH, no HOME, no credentials.
	t.Run("a nil environment is materialised, not replaced", func(t *testing.T) {
		cmd := exec.Command("true") // #nosec G204 — a constant.
		SetWorkDir(cmd, dir)
		if len(cmd.Env) != len(os.Environ())+1 {
			t.Fatalf("Env has %d entries, want the inherited %d plus PWD", len(cmd.Env), len(os.Environ())+1)
		}
		if !hasPrefixEntry(cmd.Env, "PATH=") {
			t.Fatal("the child lost PATH: a nil Env was appended to instead of being materialised")
		}
	})

	t.Run("an empty directory leaves the spawn alone", func(t *testing.T) {
		cmd := exec.Command("true") // #nosec G204 — a constant.
		SetWorkDir(cmd, "")
		if cmd.Dir != "" || cmd.Env != nil {
			t.Fatalf("Dir = %q, Env = %v — an empty dir must change neither", cmd.Dir, cmd.Env)
		}
	})
}

func hasPrefixEntry(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
