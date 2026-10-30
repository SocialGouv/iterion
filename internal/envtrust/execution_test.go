package envtrust

import (
	"strings"
	"testing"
)

// Every name the H-10 round-6 review measured planted on the defect
// (plant-refused=false, its probe pNames.log) — the execution family the
// engine's children execute: a tool-node bash SOURCED the planted
// BASH_ENV, every child PRELOADED the planted LD_PRELOAD. Refused now, on
// one predicate, in any case.
func TestEveryExecutionGradeNameTheReviewMeasuredIsRefused(t *testing.T) {
	family := []string{
		"BASH_ENV", "ENV", "PS4", "SHELLOPTS", "BASHOPTS",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "OPENSSL_CONF",
		"NODE_OPTIONS", "BUN_OPTIONS", "NPM_CONFIG_NODE_OPTIONS",
		"PATH", "IFS", "GCONV_PATH", "ZDOTDIR", "SHELL",
		"GOFLAGS", "SSH_ASKPASS", "EDITOR", "VISUAL",
		"XDG_CONFIG_HOME", "NIX_CONFIG", "GIT_ASKPASS",
	}
	for _, name := range family {
		if !DotenvPlantRefused(name) {
			t.Errorf("%s is not refused at the plant: the file a project holds chooses what iterion's children execute", name)
		}
		if !DotenvPlantRefused(strings.ToLower(name)) {
			t.Errorf("%s refused, its lowercase spelling is not: the question must not depend on the case", name)
		}
	}
	// The wider execution portion of the devcontainer table rides the same
	// class: the config homes, the trust roots, the path-spawned binaries,
	// git's program names.
	for _, name := range []string{
		"HOME", "KUBECONFIG", "DOCKER_CONFIG", "CLAUDE_CONFIG_DIR",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"NODE_TLS_REJECT_UNAUTHORIZED", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
		"GIT_EXEC_PATH", "VITALS_EMITTER_BIN", "SDK_NATIVE_BIN",
		"DS_CHROMIUM_PATH", "TAR_OPTIONS", "GOENV", "GOTOOLCHAIN",
	} {
		if !DotenvPlantRefused(name) {
			t.Errorf("%s is not refused at the plant", name)
		}
	}
}

// The opt-in is the execution class's escape, as it is the routing class's:
// the operator's export decides, the file's own line does not (a planted
// value reads as nothing said, whatever the file put beside it).
func TestTheOptInAnswersTheExecutionGrade(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(DotenvEndpointsOptIn, "")
	if !DotenvPlantRefused("BASH_ENV") {
		t.Fatal("BASH_ENV refused by nobody: the default must be the refusal")
	}
	t.Setenv(DotenvEndpointsOptIn, "allow")
	if DotenvPlantRefused("BASH_ENV") {
		t.Error("the operator's own opt-in does not answer the execution class")
	}
	if DotenvPlantRefused("XDG_CONFIG_HOME") {
		t.Error("the opt-in does not answer the XDG family")
	}
	// The grade names the opt-in never answers, control side.
	t.Setenv(DotenvEndpointsOptIn, "allow")
	if !DotenvPlantRefused("ITERION_DISABLE_AUTH") {
		t.Error("the opt-in widened the operator grade: the posture is never a file's to set")
	}
	if !DotenvPlantRefused("ITERION_DOTENV_ENDPOINTS") {
		t.Error("the opt-in name itself must stay refused at the plant")
	}
}

// The union with the routing class is ONE answer per name, the routing
// line first: a name both classes answer reads the routing reason and the
// routing tail, never two lines or a doubled explanation.
func TestTheUnionReadsOneAnswer(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(DotenvEndpointsOptIn, "")

	const name = "NIX_STORE_URL" // NIX_ execution prefix, _URL routing suffix
	if !RoutingShapedName(name) || !ExecutionGradeName(name) {
		t.Fatalf("the union witness lost a class: routing=%v execution=%v", RoutingShapedName(name), ExecutionGradeName(name))
	}
	line := RoutingRefusalLine(name, "the project `.env`", "/repo/.env")
	if !strings.Contains(line, "where a connection goes") || strings.Contains(line, "execute or trust") {
		t.Errorf("the overlap must read the routing line alone, got: %s", line)
	}
	if strings.Count(line, DotenvEndpointsOptIn) != 1 {
		t.Errorf("the opt-in named %d times in one refusal: %s", strings.Count(line, DotenvEndpointsOptIn), line)
	}
	// And an execution name outside the routing shapes reads the execution
	// reason, its own tail, the same opt-in.
	execLine := RoutingRefusalLine("LD_PRELOAD", "the project `.env`", "/repo/.env")
	if !strings.Contains(execLine, "execute or trust") || strings.Contains(execLine, "where a connection goes") {
		t.Errorf("the execution reason is not said: %s", execLine)
	}
}

// The belt closes the prompt expansion the round-6 probe measured —
// `KEY=${BAR@P}` executed the base environment's `BAR=$(touch …)` inside
// the expansion bash — and any `${name@op}` operator form beside it, in
// any case; the forms that expand to nothing dangerous stay out of the
// refusal.
func TestAPromptExpansionReferenceIsASubstitution(t *testing.T) {
	yes := []string{"${BAR@P}", "${bar@p}", "a${B@Q}c", "${X@E}", "$(touch x)", "`touch x`"}
	for _, v := range yes {
		if !HasCommandSubstitution(v) {
			t.Errorf("%q carries no trigger per the belt", v)
		}
	}
	no := []string{"${BAR}", "$BAR", "${BAR:-fallback}", "${BAR@P", "$$A", "a-z_9", ""}
	for _, v := range no {
		if HasCommandSubstitution(v) {
			t.Errorf("%q is refused: the belt widened past the operator forms", v)
		}
	}
}

// The parity the bot-var gate's comment proclaims holds on the credential
// half now: a name the bot gate refuses (CredentialShapedName) is a name
// no dotenv loader plants. These are the drifted names the H-10 review
// measured planted (test-only readers that day, structural until now).
func TestOperatorGradeCoversTheCredentialShapedNames(t *testing.T) {
	drifted := []string{
		"ITERION_GH_PAT", "ITERION_DB_PW", "ITERION_DB_PWD",
		"ITERION_SESSION_COOKIE", "ITERION_LOGIN_COOKIES",
		"ITERION_HOOK_HMAC", "ITERION_MFA_TOTP", "ITERION_TLS_PEMS",
		"ITERION_REVIEW_BEARER", "ITERION_PG_PASSFILE", "ITERION_X_USERPASS",
		"ITERION_SERVER_TOKEN", "ITERION_BOOTSTRAP_ADMIN_PASSWORD",
	}
	for _, name := range drifted {
		if !OperatorGradeRefused(name) {
			t.Errorf("%s is not refused at the plant: the drift the review measured stands", name)
		}
	}
	// Controls: the operator's non-credential knobs stay plantable, the
	// authorship names keep their AUTHOR exception (the GIT one is infra-
	// named and refused for that reason, not this one), the provider keys
	// outside ITERION_ were never the grade's question.
	for _, name := range []string{
		"ITERION_BIN_PACKING_SIBLING", "ITERION_AUTO_RESUME",
		"ITERION_SEC_PATCH_EFFORT_AUTHOR",
		"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY",
	} {
		if OperatorGradeRefused(name) {
			t.Errorf("%s is refused: the grade widened past the operator's posture", name)
		}
	}
	if !InfraGradeName("ITERION_GIT_AUTHOR_EMAIL") {
		t.Error("ITERION_GIT_AUTHOR_EMAIL lost its infra listing: the control above leans on it")
	}
}
