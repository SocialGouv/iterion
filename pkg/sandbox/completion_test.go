//go:build unix

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// runWrapped runs c.Wrap(argv) here, with the session variable in the
// process's own environment as an exec would carry it, and returns its
// stdout and exit status.
func runWrapped(t *testing.T, c *Completion, stdin string, argv ...string) ([]byte, int) {
	t.Helper()
	wrapped := c.Wrap(argv)
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	key, value := c.SessionEnv()
	cmd.Env = append(os.Environ(), key+"="+value)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("the wrapped command did not run: %v", err)
	}
	return stdout.Bytes(), code
}

func TestCompletionProvesTheEndAndKeepsTheOutputExact(t *testing.T) {
	cases := []struct {
		name   string
		argv   []string
		stdin  string
		want   string
		status int
	}{
		{"no trailing newline", []string{"printf", "out"}, "", "out", 0},
		{"trailing newline", []string{"printf", "a\\nb\\n"}, "", "a\nb\n", 0},
		{"empty", []string{"true"}, "", "", 0},
		{"its own status", []string{"sh", "-c", "printf partial; exit 3"}, "", "partial", 3},
		{"killed by a signal", []string{"sh", "-c", "kill -TERM $$"}, "", "", 143},
		{"stdin passes through", []string{"cat"}, "in\n", "in\n", 0},
		{"argv is never parsed as shell", []string{"printf", "%s", "$(echo pwned); `id`; '\"\\"}, "", "$(echo pwned); `id`; '\"\\", 0},
		{"output after the line, from a child left behind", []string{"sh", "-c", "(sleep 0.3; printf late) & printf early"}, "", "earlylate", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCompletion("t")
			stdout, code := runWrapped(t, c, tc.stdin, tc.argv...)
			clean, status, ended := c.Settle(stdout)
			if !ended {
				t.Fatalf("the completion line is missing from %q", stdout)
			}
			if status != tc.status || code != tc.status {
				t.Fatalf("status: line %d, process %d, want %d", status, code, tc.status)
			}
			if string(clean) != tc.want {
				t.Fatalf("stdout without the line = %q, want %q", clean, tc.want)
			}
		})
	}
}

// The session variable reaches the command and what it starts.
func TestCompletionCommandIsInItsSession(t *testing.T) {
	c := NewCompletion("t")
	key, _ := c.SessionEnv()
	stdout, _ := runWrapped(t, c, "", "sh", "-c", `sh -c 'printf "%s" "$`+key+`"'`)
	clean, _, ended := c.Settle(stdout)
	if !ended || string(clean) != "1" {
		t.Fatalf("the command's child does not carry the session: ended=%v, %q", ended, clean)
	}
}

// Without the line, the end is unproven — whatever stdout holds.
func TestCompletionWithoutItsLineProvesNothing(t *testing.T) {
	c := NewCompletion("t")
	other := NewCompletion("t")
	otherOut, _ := runWrapped(t, other, "", "printf", "x")
	nonce := c.nonce
	for name, stdout := range map[string]string{
		"nothing arrived":             "",
		"a partial output":            `{"verdict": "pa`,
		"another command's line":      string(otherOut),
		"the line cut before its end": "out\n" + nonce + " 0",
		"no status":                   "out\n" + nonce + " \n",
		"a status that is no number":  "out\n" + nonce + " 0x\n",
		"a signed status":             "out\n" + nonce + " +0\n",
		"a status above 255":          "out\n" + nonce + " 256\n",
		"the nonce without its line":  "out" + nonce + " 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			clean, _, ended := c.Settle([]byte(stdout))
			if ended {
				t.Fatalf("%q was read as a proven end", stdout)
			}
			if string(clean) != stdout {
				t.Fatalf("an unproven stdout was altered: %q -> %q", stdout, clean)
			}
		})
	}
}

func TestCompletionsAreDistinct(t *testing.T) {
	a, b := NewCompletion("same"), NewCompletion("same")
	if a.mark == b.mark || a.nonce == b.nonce {
		t.Fatalf("two completions share a mark or a nonce: %+v %+v", a, b)
	}
	ka, _ := a.SessionEnv()
	kb, _ := b.SessionEnv()
	if ka == kb {
		t.Fatalf("two completions share a session variable: %s", ka)
	}
}

// The command is stopped only when the sweep ran to its end and nothing it
// signalled survived; a process no one can read is named, and proves nothing
// either way.
func TestSweepOutcomeStoppedOnlyOnEvidence(t *testing.T) {
	p := []proc.SessionProcess{{PID: 7, Name: "x"}}
	for _, tc := range []struct {
		name string
		o    SweepOutcome
		want bool
	}{
		{"ran, stopped the session", SweepOutcome{Ran: true, Report: proc.SweepReport{Stopped: p}}, true},
		{"ran, found nothing", SweepOutcome{Ran: true}, true},
		{"ran, an unreadable process of the uid", SweepOutcome{Ran: true, Report: proc.SweepReport{Unreadable: p}}, true},
		{"ran, a survivor of the KILL", SweepOutcome{Ran: true, Report: proc.SweepReport{Survivors: p}}, false},
		{"never ran", SweepOutcome{}, false},
	} {
		if got := tc.o.Stopped(); got != tc.want {
			t.Errorf("%s: Stopped() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
