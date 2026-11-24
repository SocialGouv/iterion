package proctest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shape every caller of SplitOutput has: a subprocess that answers with
// a stream of JSON objects on stdout while the environment makes it talk on
// stderr. The decode must see the answer alone — read through the combined
// stream the first stderr byte ends it with "invalid character" — and the
// caller must still be able to report what the subprocess said, which is
// the only way a REAL toolchain failure stays visible once stderr no longer
// reaches the parser.
func TestSplitOutputKeepsAStderrLineOutOfTheParse(t *testing.T) {
	const script = `printf 'go: warning: the module cache is read-only\n' >&2
printf '{"ImportPath":"a"}\n{"ImportPath":"b"}\n'
printf 'go: warning: and one more after the answer\n' >&2`
	stdout, stderr, err := SplitOutput(exec.Command("sh", "-c", script))
	if err != nil {
		t.Fatalf("the probe did not answer: %v (stderr %q)", err, stderr)
	}

	var paths []string
	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	for {
		var pkg struct{ ImportPath string }
		if decErr := dec.Decode(&pkg); decErr != nil {
			if decErr == io.EOF {
				break
			}
			t.Fatalf("the %d bytes handed to the parser did not decode, so a stderr line reached it: %v\nstdout: %q\nstderr: %q",
				len(stdout), decErr, stdout, stderr)
		}
		paths = append(paths, pkg.ImportPath)
	}
	if want := []string{"a", "b"}; !slices.Equal(paths, want) {
		t.Fatalf("the parser read %v, want %v", paths, want)
	}

	// The diagnostic is not lost, only moved: a caller whose parse comes up
	// empty reports WHY from here.
	for _, fragment := range []string{"the module cache is read-only", "and one more after the answer"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr %q does not carry %q — a real toolchain failure would reach no failure message", stderr, fragment)
		}
	}
}

// The error path: a non-zero exit must reach the caller with the
// subprocess's own reason beside it, not swallowed with its stderr.
func TestSplitOutputReportsAFailingProbeWithItsStderr(t *testing.T) {
	stdout, stderr, err := SplitOutput(exec.Command("sh", "-c", `printf 'go: cannot load ./...\n' >&2; exit 2`))
	if err == nil {
		t.Fatalf("a probe that exited 2 was reported as an answer: stdout %q", stdout)
	}
	if !strings.Contains(stderr, "cannot load") {
		t.Errorf("stderr = %q, want the subprocess's reason", stderr)
	}
	if len(stdout) != 0 {
		t.Errorf("stdout = %q, want the empty answer the probe gave", stdout)
	}
}

// SplitOutput owns both streams, so a command that arrives with one of them
// already set is a programming mistake — and the one shape that would
// otherwise surface as a subprocess failure: [exec.Cmd.Output] answers a set
// Stdout with "exec: Stdout already set", which every caller here reports as
// "the tool did not answer". The refusal names the misuse instead, and the
// command does not run.
func TestSplitOutputRefusesACommandThatAlreadyOwnsAStream(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "the-probe-ran")
	cmd := exec.Command("sh", "-c", `printf ran > "$0"`, ran)
	var mine bytes.Buffer
	cmd.Stdout = &mine

	stdout, stderr, err := SplitOutput(cmd)
	if err == nil {
		t.Fatalf("SplitOutput ran a command whose Stdout was already set: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "cmd.Stdout set: true") {
		t.Fatalf("the refusal does not name the misuse, so a caller reports it as the subprocess failing: %v", err)
	}
	if _, statErr := os.Stat(ran); !os.IsNotExist(statErr) {
		t.Fatalf("the refused command still ran (%s exists: %v) — a guard that spawns the process guards nothing", ran, statErr)
	}
	// The caller's own destination is left as it chose it.
	if cmd.Stderr != nil {
		t.Errorf("the refusal replaced cmd.Stderr with %T", cmd.Stderr)
	}
}

// The same refusal for the stream SplitOutput writes into: overwriting it
// would silently discard the destination the caller chose.
func TestSplitOutputRefusesACommandThatAlreadyOwnsStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	var mine bytes.Buffer
	cmd.Stderr = &mine

	if _, _, err := SplitOutput(cmd); err == nil {
		t.Fatal("SplitOutput overwrote a caller's cmd.Stderr instead of refusing")
	} else if !strings.Contains(err.Error(), "cmd.Stderr set: true") {
		t.Fatalf("the refusal does not name the misuse: %v", err)
	}
	if cmd.Stderr != &mine {
		t.Errorf("cmd.Stderr = %T, want the caller's own buffer", cmd.Stderr)
	}
}
