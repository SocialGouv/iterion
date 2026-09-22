package floorsalign

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// releaseItBin is the installed release-it, or "" when node_modules is not
// there. The `test` CI job installs the node dependencies (it builds the
// studio frontend) before it runs the Go tests, so this skip is a local
// convenience and not a hole in the gate.
func releaseItBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("..", "..", "node_modules", ".bin", "release-it"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("release-it is not installed (%v): run `corepack pnpm install` — the CI `test` job does", err)
	}
	return bin
}

// The hook slot the wiring names is one release-it actually fires, and what
// it writes lands INSIDE the release commit. Both are claims .release-it.mjs
// makes in prose, and neither is falsifiable by reading it: a hook key
// release-it does not know is not an error, it is a no-op — the release is
// cut, the pins keep their pending number, and the release test stays green
// because a pin left at the next minor is exactly what its ahead arm wants.
// The rot would surface one cut later, as a release that cannot be made.
//
// So the slot is exercised against the real release-it, on a repository
// small enough to cut in a test: a tracked file the hook rewrites must come
// back out of `git show HEAD:` rewritten.
func TestTheHookSlotFiresAndItsWriteLandsInTheReleaseCommit(t *testing.T) {
	bin := releaseItBin(t)
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name":"hook-probe","private":true,"version":"1.0.0"}`)
	write("pin.txt", "pending\n")
	write(".release-it.json", `{
  "git": {"requireUpstream": false, "push": false, "tag": false, "commitMessage": "chore: release v${version}"},
  "npm": {"publish": false},
  "github": {"release": false},
  "hooks": {"before:git:beforeRelease": "node -e \"require('fs').writeFileSync('pin.txt','cut\\n')\""}
}`)
	gittest.Run(t, root, "init", "-q")
	// release-it spawns its own git, which does not inherit gittest's
	// identity: the probe repository carries its own.
	gittest.Run(t, root, "config", "user.email", "probe@example.invalid")
	gittest.Run(t, root, "config", "user.name", "hook probe")
	gittest.Run(t, root, "config", "commit.gpgSign", "false")
	// release-it's git children are not gittest's, so the repository
	// carries the refusal itself: git >= 2.48 ends a commit by forking
	// `git maintenance run --auto --detach`, which outlives the process
	// and writes under .git while t.TempDir() removes it (#821, #828).
	gittest.Run(t, root, "config", "maintenance.auto", "false")
	gittest.Run(t, root, "config", "gc.auto", "0")
	gittest.Run(t, root, "add", ".")
	gittest.Run(t, root, "commit", "-q", "-m", "chore: seed the probe")

	cmd := exec.Command(bin, "patch", "--ci")
	cmd.Dir = root
	// The operator's global config must not decide this verdict: a
	// core.hooksPath, a signing key or an inherited GIT_DIR would redden a
	// probe that has nothing to do with them.
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release-it refused the probe cut: %v\n%s", err, out)
	}

	subject := lastLine(gittest.Run(t, root, "log", "-1", "--format=%s"))
	if subject != "chore: release v1.0.1" {
		t.Fatalf("HEAD is %q, want the release commit — the probe did not cut", subject)
	}
	committed := lastLine(gittest.Run(t, root, "show", "HEAD:pin.txt"))
	if committed != "cut" {
		t.Fatalf("pin.txt in the release commit is %q, want %q: release-it 21 no longer fires `before:git:beforeRelease` before the git plugin stages, so cmd/release-floors would realign nothing and no gate would say so\n%s", committed, "cut\n", out)
	}
}

// lastLine is git's answer without the diagnostics it may have written
// first: gittest reports the combined output, and a GIT_TRACE in the
// environment would otherwise decide the verdict.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
