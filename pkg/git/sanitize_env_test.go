package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeEnv pins which variables are dropped and which survive. The two
// halves matter equally: dropping too little leaves a git command acting on a
// repository it did not name, and dropping too much breaks the sandbox path,
// which sets the identity deliberately so an in-container commit is possible
// at all.
func TestSanitizeEnv(t *testing.T) {
	in := []string{
		"GIT_DIR=/elsewhere/.git",
		"GIT_WORK_TREE=/elsewhere",
		"GIT_INDEX_FILE=/tmp/ambient-index",
		"GIT_OBJECT_DIRECTORY=/tmp/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/alt",
		"GIT_COMMON_DIR=/elsewhere/.git",
		"GIT_NAMESPACE=refs/ns",
		// Kept: the sandbox sets these on purpose.
		"GIT_AUTHOR_NAME=bot",
		"GIT_AUTHOR_EMAIL=bot@example.com",
		"GIT_COMMITTER_NAME=bot",
		"GIT_COMMITTER_EMAIL=bot@example.com",
		"GIT_SSH_COMMAND=ssh -i key",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"PATH=/usr/bin",
		// A value containing '=' must not confuse the key split.
		"GIT_ASKPASS=/tmp/helper=1",
	}
	got := strings.Join(SanitizeEnv(in), "\n")

	for _, dropped := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY=",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_COMMON_DIR=", "GIT_NAMESPACE="} {
		if strings.Contains(got, dropped) {
			t.Errorf("%s survived — a git call can still be redirected away from the directory it names", dropped)
		}
	}
	for _, kept := range []string{"GIT_AUTHOR_NAME=bot", "GIT_COMMITTER_EMAIL=bot@example.com",
		"GIT_SSH_COMMAND=ssh -i key", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null",
		"PATH=/usr/bin", "GIT_ASKPASS=/tmp/helper=1"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was dropped — the sandbox and transport paths set it deliberately", kept)
		}
	}
}

// TestSanitizeEnvIsWhatMakesDirAuthoritative demonstrates the property against
// real git rather than asserting on the slice: with GIT_DIR inherited, a
// command that names its own directory answers about another repository.
func TestSanitizeEnvIsWhatMakesDirAuthoritative(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	target := gitRepo(t)
	decoy := gitRepo(t)
	if err := os.WriteFile(filepath.Join(decoy, "only-in-decoy.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(env []string) string {
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = target
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "ERR: " + string(out)
		}
		return string(out)
	}

	polluted := append(os.Environ(),
		"GIT_DIR="+filepath.Join(decoy, ".git"),
		"GIT_WORK_TREE="+decoy,
	)
	if got := run(polluted); !strings.Contains(got, "only-in-decoy.txt") {
		t.Fatalf("the premise does not hold on this git: expected the decoy's state, got %q", got)
	}
	if got := run(SanitizeEnv(polluted)); strings.TrimSpace(got) != "" {
		t.Errorf("after SanitizeEnv, status still reports another repository: %q", got)
	}
}
