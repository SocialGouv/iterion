package envtrust

import (
	"strings"
	"testing"
)

// The operator grade's floor: the names the H-10 review measured planted in
// silence on every lane, one per family the grade answers for — the sandbox
// lifts, the server's auth/origin/header guards, the project-trust
// switches, the server's secrets (namespaced and not), the opt-in itself,
// and the prefix families' edges. A name here that OperatorGradeRefused
// lets through is a `.env` line answering for the operator again.
func TestOperatorGradeRefusedCoversTheLiftersAndTheServerSecrets(t *testing.T) {
	for _, name := range []string{
		// the H-10 measured chains
		"ITERION_SANDBOX_OVERRIDE", "ITERION_SANDBOX_DEFAULT",
		"ITERION_DISABLE_AUTH", "ITERION_REQUIRE_ORIGIN", "ITERION_ALLOWED_ORIGINS",
		"ITERION_SECURITY_HEADERS", "ITERION_STUDIO_INSECURE_NONLOOPBACK",
		"ITERION_PI_TRUST_PROJECT",
		"ITERION_BOOTSTRAP_ADMIN_PASSWORD", "ITERION_BOOTSTRAP_ADMIN_EMAIL",
		"ITERION_JWT_SECRET", "ITERION_SERVER_TOKEN",
		"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE", "ITERION_COMPLETION_WEBHOOK_SECRET",
		// the credential substring, mid-name, outside every namespace
		"ITERION_SOME_SERVICE_TOKEN", "ITERION_VAULT_PASSWORD", "ITERION_SHARD_SIGNING_SECRET",
		// the opt-in itself: a file line granting the file anything would be
		// the file voting for itself
		"ITERION_DOTENV_ENDPOINTS",
		// the provenance marker
		"ITERION_DOTENV_PLANTED",
		// prefix-family edges
		"ITERION_LOG_LEVEL", "ITERION_MODE", "ITERION_HOME", "ITERION_GATE_SWEEP_ANYTHING",
		"ITERION_RUN_SOMETHING", "ITERION_LEASE_WHATEVER",
	} {
		if !OperatorGradeRefused(name) {
			t.Errorf("%s is not operator-grade: a project `.env` may plant it", name)
		}
	}
}

// The grade does not reach past the engine's namespace: a provider
// credential is what a `.env` next to a project is FOR, and a non-ITERION
// knob is nobody's posture question. The routing shape answers those.
func TestOperatorGradeStaysInsideTheEngineNamespace(t *testing.T) {
	for _, name := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY",
		"VAULT_TOKEN", "MY_PASSWORD", "GITHUB_TOKEN", "HOME", "PATH",
	} {
		if OperatorGradeRefused(name) {
			t.Errorf("%s is refused as operator-grade: the provider credentials are what the file is for", name)
		}
	}
}

// The layering at the one entry point: a routing-shaped name answers to the
// opt-in (the operator's explicit grant, whatever namespace it lives in —
// the deployment wires keep working), and every OTHER grade name answers to
// nothing the file or its opt-in line can say.
func TestDotenvPlantRefusedLayersTheGradeUnderTheRoutingGrant(t *testing.T) {
	t.Run("a routing-shaped grade name answers the opt-in", func(t *testing.T) {
		// ITERION_WEBHOOK_FORGE_HOSTS: the trusted-host list (_HOSTS) the
		// bot-var gate also denies a stored record — with the operator's own
		// export granting the routes, it plants, marked.
		ResetForTest()
		t.Cleanup(ResetForTest)
		t.Setenv(EnvPlantedNames, "")
		t.Setenv(DotenvEndpointsOptIn, "")
		if !DotenvPlantRefused("ITERION_WEBHOOK_FORGE_HOSTS") {
			t.Error("without the opt-in, the trusted-host list is not refused")
		}
		t.Setenv(DotenvEndpointsOptIn, "allow")
		if DotenvPlantRefused("ITERION_WEBHOOK_FORGE_HOSTS") {
			t.Error("with the operator's own opt-in, the routing-shaped name is still refused: the deployment wires lost their grant")
		}
	})
	t.Run("a grade name outside the routing shapes answers nothing", func(t *testing.T) {
		ResetForTest()
		t.Cleanup(ResetForTest)
		t.Setenv(EnvPlantedNames, "")
		for _, grant := range []string{"", "allow"} {
			t.Setenv(DotenvEndpointsOptIn, grant)
			for _, name := range []string{
				"ITERION_SANDBOX_OVERRIDE", "ITERION_DISABLE_AUTH",
				"ITERION_JWT_SECRET", "ITERION_SERVER_TOKEN",
				"ITERION_DOTENV_ENDPOINTS", "ITERION_LOG_LEVEL",
			} {
				if !DotenvPlantRefused(name) {
					t.Errorf("%s with the opt-in %q: the operator's posture was plantable", name, grant)
				}
			}
		}
	})
}

// The same inventory serves the bot-var gate: a name the bot-var gate
// refuses (its infra lists) is InfraGradeName, and the grade predicate
// answers it too. A name the platformcfg tests hold (their own witness
// tables) cannot drift away from the dotenv side without this going red.
func TestInfraGradeNameIsTheBotVarGateInventory(t *testing.T) {
	for _, name := range []string{
		"ITERION_PI_TRUST_PROJECT", "ITERION_DISABLE_AUTH", "ITERION_PUBLIC_URL",
		"ITERION_MONGO_URI", "ITERION_NATS_URL", "ITERION_SANDBOX_PROXY_ENDPOINT",
		"ITERION_SCHEDULE_PATH_MAX_BYTES", "ITERION_BOT_VARS_ENV_PINS",
	} {
		if !InfraGradeName(name) {
			t.Errorf("%s is not in the operator infra inventory: the bot-var gate and the dotenv grade have drifted", name)
		}
	}
	if InfraGradeName("OPENAI_BASE_URL") {
		t.Error("a provider endpoint is not the engine's infra")
	}
}

// The command-substitution scan: `$( `, backticks, and the quoted spellings
// of both, on the raw bytes — the studio lane used to RUN them by sourcing
// the file.
func TestHasCommandSubstitutionSeesTheSpellings(t *testing.T) {
	for _, value := range []string{
		"$(touch x)", "pre$(x)post", "`touch x`", "pre`x`post",
		`"$(touch x)"`, "${X:-$(touch x)}", "$((1+1))",
	} {
		if !HasCommandSubstitution(value) {
			t.Errorf("%q is not seen as command substitution", value)
		}
	}
	for _, value := range []string{
		"", "plain", "$HOME/bin", "${HOME}/bin", "$UNSET_VAR",
		"https://host.example/path?q=1", "a single $ alone",
	} {
		if HasCommandSubstitution(value) {
			t.Errorf("%q is refused as command substitution: the ordinary expansions must survive", value)
		}
	}
}

// The refusal lines: the grade-only names get the operator's channel and NO
// opt-in half; the routing names keep theirs; the substitution line offers
// no grant at all.
func TestRefusalLinesSayTheRightWayBack(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")
	t.Setenv(DotenvEndpointsOptIn, "")

	grade := RoutingRefusalLine("ITERION_SANDBOX_OVERRIDE", "the project `.env`", "/repo/.env")
	for _, want := range []string{"ITERION_SANDBOX_OVERRIDE", "/repo/.env", "does not speak for the operator"} {
		if !strings.Contains(grade, want) {
			t.Errorf("the grade refusal does not say %q: %s", want, grade)
		}
	}
	if strings.Contains(grade, DotenvEndpointsOptIn+"=allow") {
		t.Errorf("the grade refusal offers the opt-in: %s", grade)
	}

	route := RoutingRefusalLine("OPENAI_BASE_URL", "the project `.env`", "/repo/.env")
	for _, want := range []string{"OPENAI_BASE_URL", DotenvEndpointsOptIn + "=allow to let this file plant its routes"} {
		if !strings.Contains(route, want) {
			t.Errorf("the routing refusal does not say %q: %s", want, route)
		}
	}

	subst := CommandSubstitutionRefusalLine("FOO", "the studio snapshot", "/repo/.env")
	for _, want := range []string{"FOO", "the studio snapshot", "/repo/.env", "does not run commands"} {
		if !strings.Contains(subst, want) {
			t.Errorf("the substitution refusal does not say %q: %s", want, subst)
		}
	}
	if strings.Contains(subst, DotenvEndpointsOptIn) {
		t.Errorf("the substitution refusal offers a grant: %s", subst)
	}
}

// The git transport family: the programs and config the git BINARY reads —
// no Go reader exists, which is why the class carries them as prefixes.
func TestGitTransportNameSeesTheFamilyTheBinaryReads(t *testing.T) {
	for _, name := range []string{
		"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "git_ssh_command",
		"GIT_PROXY_COMMAND",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_PARAMETERS",
	} {
		if !GitTransportName(name) {
			t.Errorf("%s is not seen as git transport", name)
		}
		if !RoutingShapedName(name) {
			t.Errorf("%s is not in the routing class: a project `.env` may plant the engine's git transport", name)
		}
	}
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_AUTHOR_NAME", "GIT_TERMINAL_PROMPT"} {
		if GitTransportName(name) {
			t.Errorf("%s is wrongly in the transport family: the redirections are SanitizeEnv's own, the identity is set on purpose", name)
		}
	}
}
