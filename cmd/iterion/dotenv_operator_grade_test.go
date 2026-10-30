package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// gradeWitnessNames is the operator-grade witness table: the names the H-10
// review measured planted in silence, plus one of each family the grade
// answers for — the sandbox lifts, the server's auth/origin/header guards,
// the project-trust switches, the server's own secrets, the opt-in itself,
// and a member of every infra namespace's edge (ITERION_LOG_LEVEL riding
// its prefix). The rule they carry is the tree's own bot-var rule
// (pkg/platformcfg): a stored record must never set them, and the H-10
// finding was the project's file doing exactly that with MORE authority
// than a bot. One inventory serves both gates (envtrust's operator.go).
// The names that are ALSO routing-shaped (ITERION_WEBHOOK_FORGE_HOSTS and
// its kin) belong to the routing witness table, where the operator's
// opt-in answers.
var gradeWitnessNames = []string{
	"ITERION_SANDBOX_OVERRIDE",
	"ITERION_SANDBOX_DEFAULT",
	"ITERION_DISABLE_AUTH",
	"ITERION_REQUIRE_ORIGIN",
	"ITERION_ALLOWED_ORIGINS",
	"ITERION_REQUIRE_WS_ORIGIN",
	"ITERION_SECURITY_HEADERS",
	"ITERION_STUDIO_INSECURE_NONLOOPBACK",
	"ITERION_PI_TRUST_PROJECT",
	"ITERION_JWT_SECRET",
	"ITERION_SECRETS_KEY",
	"ITERION_BOOTSTRAP_ADMIN_PASSWORD",
	"ITERION_SERVER_TOKEN",
	"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE",
	"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE",
	"ITERION_DOTENV_ENDPOINTS",
	"ITERION_LOG_LEVEL",
	"ITERION_MODE",
	"ITERION_HOME",
}

// gitTransportWitnessNames is the git transport family: the programs and
// config the git BINARY reads (no Go reader exists — the H-10 probe
// measured a planted GIT_SSH_COMMAND EXECUTED by the git subprocess before
// it dialled). The lowercase spelling is a distinct name on Linux and
// travels on its own line, like the proxy witnesses.
var gitTransportWitnessNames = []string{
	"GIT_SSH",
	"GIT_SSH_COMMAND",
	"GIT_PROXY_COMMAND",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_KEY_1",
	"GIT_CONFIG_VALUE_1",
	"git_ssh_command",
}

// A project `.env` plants no operator-grade name: after the load, none of
// the guard-lifters, server secrets or infra names exists in the process —
// not even marked planted — and every refusal names the file and the key,
// with NO opt-in half: ITERION_DOTENV_ENDPOINTS widens routes, never the
// operator's posture. On the defect this closes, the same file planted
// ITERION_SANDBOX_OVERRIDE=none (the reviewed repository disarming the
// engine that reviews it) and ITERION_BOOTSTRAP_ADMIN_PASSWORD (the
// bootstrap taking the file's password for the super-admin) without a
// word.
func TestAProjectDotenvLeavesNoOperatorGradeNameInTheEnvironment(t *testing.T) {
	isolateRoutingEnv(t)

	lines := make([]string, 0, len(gradeWitnessNames))
	for _, name := range gradeWitnessNames {
		lines = append(lines, name+"=planted-by-the-repo-under-review")
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	envPath := filepath.Join(repo, ".env")
	for _, name := range gradeWitnessNames {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q: the repository under review answered for the operator", name, got)
		}
		if envtrust.Planted(name) {
			t.Errorf("%s reads as planted: a refused name does not exist, it is not planted-but-distrusted", name)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — it said: %s", name, said)
		}
		if strings.Contains(said, envtrust.DotenvEndpointsOptIn+"=allow to let this file plant its routes") {
			t.Fatalf("the operator-grade refusal offers the opt-in — it said: %s", said)
		}
	}
}

// The git transport family is refused like the routes: the file must not
// choose the PROGRAM that carries the engine's own clones, fetches and
// pushes, nor inject git config for them. The reader is the git binary
// itself, which no Go-side grep sees — the refusal at the plant is the
// only guard there is, beside pkg/git's own drop of a planted name.
func TestAProjectDotenvLeavesNoGitTransportNameInTheEnvironment(t *testing.T) {
	isolateRoutingEnv(t)
	// The sandbox itself may carry a real transport (the loader fills only
	// names the environment left unset): each witness is cleared, absent is
	// the state the file fills.
	for _, name := range gitTransportWitnessNames {
		clearEnv(t, name)
	}

	lines := make([]string, 0, len(gitTransportWitnessNames))
	for _, name := range gitTransportWitnessNames {
		lines = append(lines, name+"=sh -c 'echo transported'")
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	envPath := filepath.Join(repo, ".env")
	for _, name := range gitTransportWitnessNames {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q: the repository under review chose the engine's git transport", name, got)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, envPath) {
			t.Errorf("the refusal does not name %s and the file — it said: %s", name, said)
		}
	}
}

// A value spelling command substitution is refused, and nothing runs: the
// marker file the substitutions name is never created, the names do not
// exist in the environment, and the refusals name the file — where the
// sourcing dialects EXECUTED `$(touch …)` and backticks on the host
// (measured by the H-10 probe on the studio lane).
func TestAProjectDotenvRunsNothingACommandSubstitutionSpells(t *testing.T) {
	isolateRoutingEnv(t)

	marker := filepath.Join(t.TempDir(), "substituted-ran")
	repo := writeDotEnv(t,
		"ITERION_LOG_LEVEL=error",
		"FOO=$(touch "+marker+")",
		"BAR=`touch "+marker+"`",
		"QUOTED=\"$(touch "+marker+")\"",
		"LITERAL_SINGLE='$(touch "+marker+")'",
	)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the substitution marker exists: the file's command RAN on the host (stat err %v)", err)
	}
	for _, name := range []string{"FOO", "BAR", "QUOTED", "LITERAL_SINGLE"} {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s reached the environment: a refused value was applied", name)
		}
		if !strings.Contains(said, name) || !strings.Contains(said, filepath.Join(repo, ".env")) {
			t.Errorf("the substitution refusal does not name %s and the file — it said: %s", name, said)
		}
	}
}
