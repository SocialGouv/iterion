package git

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// A git transport a project `.env` planted never reaches the git binary:
// gitEnv assembles the child environment and drops every name of the
// transport family the planted marker carries. The loaders refuse the
// family at the plant, so a planted value here is what an OLDER binary's
// loader left in a child (the marker travels) — the H-10 probe measured
// the planted GIT_SSH_COMMAND EXECUTED by git before it even dialled, so
// the belt sits at the one place every engine gesture assembles its env.
func TestGitEnvDropsAPlantedGitTransport(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "GIT_SSH_COMMAND,GIT_PROXY_COMMAND,GIT_CONFIG_COUNT")
	envtrust.ResetForTest()

	t.Setenv("GIT_SSH_COMMAND", "/repo-under-review/planted-ssh")
	t.Setenv("GIT_PROXY_COMMAND", "/repo-under-review/planted-proxy")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	// The operator's own transport — exported, never planted — survives.
	t.Setenv("GIT_SSH_VARIANT", "ssh")

	env := toMapForTest(gitEnv())
	if got, ok := env["GIT_SSH_COMMAND"]; ok {
		t.Errorf("GIT_SSH_COMMAND = %q reached the git binary: the reviewed repository chose the engine's transport program", got)
	}
	if _, ok := env["GIT_PROXY_COMMAND"]; ok {
		t.Error("GIT_PROXY_COMMAND reached the git binary")
	}
	if _, ok := env["GIT_CONFIG_COUNT"]; ok {
		t.Error("GIT_CONFIG_COUNT reached the git binary: injected config rides it")
	}
	if got, ok := env["GIT_SSH_VARIANT"]; !ok || got != "ssh" {
		t.Errorf("GIT_SSH_VARIANT = %q (present=%v): the operator's own transport must survive the belt", got, ok)
	}
}

// The control: without a planted marker, the same environment passes
// through — an operator-exported GIT_SSH_COMMAND is the operator's
// transport choice and reaches git intact.
func TestGitEnvKeepsAnExportedTransport(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()

	const operatorSSH = "ssh -o ControlMaster=no -o ControlPath=none"
	t.Setenv("GIT_SSH_COMMAND", operatorSSH)

	env := toMapForTest(gitEnv())
	if got, ok := env["GIT_SSH_COMMAND"]; !ok || got != operatorSSH {
		t.Errorf("GIT_SSH_COMMAND = %q (present=%v), want the operator's own transport kept", got, ok)
	}
}

func toMapForTest(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok {
			out[key] = value
		}
	}
	return out
}
