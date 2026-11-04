//go:build live || clilive

package delegate

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// openCodeMajorVersion reads the CLI's own major version (`opencode v2.2.5`)
// under env, nil inheriting the test process's own.
//
// The 1.x and 2.x lines differ in what they parse, what they load and how
// they report a session, so a real-CLI proof has to know which one answered:
// measured, never assumed — an assertion that guessed its version would
// certify nothing.
//
// The environment is the caller's because opencode WRITES while answering
// (its log lands under the data home), so a probe carrying the operator's
// home touches the operator's tree.
func openCodeMajorVersion(t *testing.T, cli string, env []string) int {
	t.Helper()
	probe := exec.Command(cli, "--version") // #nosec G204 — the located CLI.
	probe.Env = env
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("opencode --version: %v: %s", err, out)
	}
	line := strings.TrimSpace(string(out))
	start := strings.IndexFunc(line, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		t.Fatalf("opencode --version printed %q, which carries no version", line)
	}
	major, _, _ := strings.Cut(line[start:], ".")
	parsed, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("opencode --version printed %q, whose major version does not parse: %v", line, err)
	}
	return parsed
}
