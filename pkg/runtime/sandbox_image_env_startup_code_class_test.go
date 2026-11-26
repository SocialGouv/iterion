package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// The deny class answers TWO questions about a name, and the table in
// sandbox_tool_env_class_test.go holds only the first: is the name in the
// class at all. This file holds the second — what the runtime DOES when an
// IMAGE bakes it in — for the family where the answer was litigated for
// four rounds: a name that leads to CODE, or to a configuration a program
// EXECUTES, at the startup of a process iterion starts in the container.
//
// The answer is REPORTED, for every one of them (ARB-121, amending
// ARB-119). Not because the channels are harmless — every row below
// carries the measurement that it FIRES — but because the thing that
// carries them is the repository's own code: an image a repository chooses
// ships its binaries, its files and its `postCreateCommand`, and the ENV
// road is one of several into the same position. Emptying one name of a
// channel the image's own files re-open promises the operator something
// the artefact cannot owe, and a promise that cannot be kept is worse than
// a residue that is written down. The fork road into that position is
// refused at admission (admitSandboxImageBackends); what this screen
// closes is the DECLARATIVE redirection of the operator's credentials and
// model routes, which is the credential/routing sub-class and nothing
// more.
//
// So the rows are kept and their measurements with them, with the
// disposition inverted: this file is the inventory of what the screen
// SIGNALS and deliberately does not empty. The set is enumerated from the
// PROGRAMS, not from a list of names: the default sandbox image ships bash
// 5.2, dash as /bin/sh, node 24 with npm/npx, a bun-compiled claude CLI,
// python 3.13, perl, git 2.47, GNU tar 1.35, openssl 3.5 and curl — read
// with `command -v` in the image itself — and each row is one startup
// channel of one of them. The `measured` field is the verdict that was
// observed, so a row cannot be argued into the table.
type startupCodeChannel struct {
	program        string // the program whose startup reads it
	where          string // the site that starts that program
	env            string // the name an image can bake
	what           string // what the planted value runs
	wantDenied     bool
	wantNeutralize bool
	measured       string // what was observed, on a disposable image
}

// TestEveryStartupCodeChannelIsToldAndNotEmptied holds the class at its
// disposition, row by row: in the class (so the operator is TOLD) and not
// neutralised (so the image keeps the value, and the page says why).
func TestEveryStartupCodeChannelIsToldAndNotEmptied(t *testing.T) {
	channels := []startupCodeChannel{
		// The loader: it runs the planter's native code in EVERY
		// dynamically linked process, which is every process in the
		// container.
		{"ld.so", "every process iterion starts there", "LD_PRELOAD",
			"a shared object whose constructor runs before main", true, false,
			"the constructor ran in pid 1 and re-exported ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, AWS_ACCESS_KEY_ID and CLAUDE_CODE_USE_BEDROCK to bash, node and an npm-started MCP server; emptying it was measured inert, and is still not done — the same image ships the .so it names"},
		{"ld.so", "every process iterion starts there", "LD_AUDIT",
			"the same object through the loader's audit interface", true, false,
			"la_version and the constructor both ran in pid 1, and the planted route came back; `--env LD_AUDIT=` inert"},
		{"ld.so", "every process iterion starts there", "LD_LIBRARY_PATH",
			"the directories the loader searches, hence which library is found", true, false,
			"an image baking LD_LIBRARY_PATH=/planted/lib with one shim in it (libdl.so.2, whose glibc original is an empty stub) had NODE read back the planter's route, token, access key and Bedrock switch, with every neutralisation of round 3 applied and over the operator's own token — and it cannot be emptied either way: it is how a CUDA-style image finds its own runtime (nvidia/cuda:12.6.2-base-ubuntu24.04 bakes it)"},
		// The shells iterion runs a tool node and an agent's Bash tool
		// with.
		{"bash", "a tool node's command, an agent's Bash tool", "BASH_ENV",
			"a startup file sourced by every NON-interactive bash", true, false,
			"the planted script re-exported the names the screen had emptied, in the very `bash -c` of a tool node; empty is bash's default, and the e2e arm reads the kept value in the container (TestLive_Feat_Sandbox_Image_Env)"},
		{"bash", "a tool node's command", "SHELLOPTS",
			"xtrace, which runs the command substitutions of PS4", true, false,
			"`SHELLOPTS=xtrace` with a planted PS4 ran the planter's program once per traced command in a `bash -c`"},
		{"bash", "a tool node's command", "PS4",
			"the substitution xtrace evaluates for every traced command", true, false,
			"same run as SHELLOPTS"},
		{"bash", "a tool node's command", "BASH_FUNC_git%%",
			"a function definition that shadows a command the script calls", true, false,
			"the planted function answered `git --version` in a `bash -c`"},
		// node, and the two npm roads into a node child.
		{"node", "the claude, pi and opencode CLIs, an MCP server", "NODE_OPTIONS",
			"--require=<module>, in the CLI's own process", true, false,
			"the planted module re-exported the emptied names inside node's own process; the e2e arm asks NODE what it reads, in the container (TestLive_Feat_Sandbox_Image_Env)"},
		{"node", "the claude, pi and opencode CLIs", "NODE_PATH",
			"a bare require resolved from the planter's directory", true, false,
			"empty is node's default (#2317, ARB-119); kept, per ARB-121"},
		{"npm", "an MCP server a workflow declares (`npx <server>`)", "NPM_CONFIG_NODE_OPTIONS",
			"the NODE_OPTIONS of every node child npm spawns", true, false,
			"`npm run` and `npm exec` children read NODE_OPTIONS=--require=/planted/replant.js and the planted module re-exported the credentials, with NODE_OPTIONS itself emptied — which is the measurement that killed emptying NODE_OPTIONS as a guarantee"},
		{"npm", "an MCP server a workflow declares", "NPM_CONFIG_SCRIPT_SHELL",
			"the shell every npm script runs under", true, false,
			"the planter's program became the shell of `npm run`, with the script on its argv"},
		{"claude", "every claude_code node's agent CLI", "BUN_OPTIONS",
			"--preload=<module>, inside the CLI's own process", true, false,
			"`claude --version` in the default image ran the planted preload; the CLI is a bun-compiled binary"},
		// glibc's and OpenSSL's module roads, which no PYTHON* or node
		// option covers.
		{"python3", "a tool node with `language: python3`, a repo build", "GCONV_PATH",
			"a gconv module dlopened by the charset conversion at interpreter startup", true, false,
			"`python3 -c` AND `python3 -I -c` — the argv iterion's own python node uses — both ran the planted module's constructor"},
		{"curl", "a tool node's command, the openssl CLI", "OPENSSL_CONF",
			"a configuration file that dlopens the provider module it names", true, false,
			"`curl https://…` and `openssl version` both ran the planted provider's constructor"},
		// git, by the roads that need no file and no HOME.
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_COUNT",
			"`git -c` from the environment: core.sshCommand and credential.helper are programs git RUNS", true, false,
			"`git ls-remote` executed the planted program with the operator's remote on its argv"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_KEY_0",
			"the key half of the same pair", true, false, "same run"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_VALUE_0",
			"the value half of the same pair", true, false, "same run"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_PARAMETERS",
			"git's SECOND config road from the environment: the serialised form `git -c` passes to its children, one variable, no file and no HOME", true, false,
			"a planted 'core.sshCommand=…' ran the planter's program on `git ls-remote` (uid 1000, default sandbox image), core.hooksPath ran it on the commit an agent makes, and credential.helper is #2316's mechanism by the same single variable"},
		// tar, which iterion itself starts.
		{"tar", "the scratch bank and its restore, kubernetes populate/export", "TAR_OPTIONS",
			"--checkpoint-action=exec=<program>, prepended to tar's own arguments", true, false,
			"the planter's program ran in a `tar -czf`"},
		// PATH is a startup channel of every one of them, and the one
		// row of this table no disposition could ever take.
		{"sh", "every Run.Exec of a generated script", "PATH",
			"the binaries the script's bare names resolve to", true, false,
			"not emptied, and it could not be: the container would resolve no command, and iterion's own `full` image bakes it"},
		{"sh", "every Run.Exec of a generated script", "ENV",
			"a startup file, in an interactive POSIX shell", true, false,
			"measured on both shapes: the default image's /bin/sh (dash) and an image whose /bin/sh is bash BOTH ignore it for a non-interactive `sh -c`, where BASH_ENV is the channel that fires"},
		{"nix", "the devbox install of the post-create prologue", "NIX_CONFIG",
			"plugin-files, dlopened on any nix invocation", true, false,
			"not emptied: an empty nix.conf breaks the provisioning step iterion itself runs"},
		{"kubectl", "a deploy-shaped workflow's step", "KUBECONFIG",
			"a kubeconfig whose exec plugin is a program", true, false,
			"not emptied: emptying sends the client to its own default, the operator's ~/.kube/config under host_state, which is worse than the planted path"},
		{"git", "the clone, commit and push of the run's work", "HOME",
			"the ~/.gitconfig every insteadOf and credential helper is read from", true, false,
			"not emptied, and assumed as such (question Jo #59): an empty HOME sends every configuration read to the DEFAULT, which under host_state is the operator's own home"},

		// OUTSIDE the class: the channel fires and no site of iterion's
		// starts the program, so the operator is not even told. The two
		// rows are the honest edge of the class, measured like the rest.
		{"python3", "a tool node with `language: python3`, a repo build", "PYTHONPATH",
			"a sitecustomize.py imported at interpreter startup", false, false,
			"measured both ways: the planted sitecustomize ran under a bare `python3 -c` and NOT under `python3 -I`, which is the argv iterion's own python node uses (isolated mode ignores the whole PYTHON* family) — what is left is the repository's own build, which already runs in that container"},
		{"perl", "no site: iterion starts no perl in the container", "PERL5OPT",
			"-M<module>, run at interpreter startup", false, false,
			"measured that the channel FIRES (the planted module ran under `perl -e`), and that no site of iterion's own starts perl there: the deny class takes the names that steer a process ITERION starts"},
	}

	// Anti-vacuity: the table is the claim, and an empty or shrunken one
	// would certify nothing. 24 rows at the time of writing, 22 of them
	// in the class.
	if len(channels) < 22 {
		t.Fatalf("the startup-code class holds %d rows: a table this small has stopped describing the programs the image ships", len(channels))
	}
	told := 0
	for _, c := range channels {
		if c.wantDenied {
			told++
		}
	}
	if told < 20 {
		t.Fatalf("only %d row(s) of the startup-code class are in the class at all: a name outside it is not even REPORTED, which is strictly less than this file's whole claim", told)
	}

	for _, c := range channels {
		t.Run(c.program+"/"+c.env, func(t *testing.T) {
			if c.measured == "" {
				t.Fatalf("%s has no measurement: a row of this table is an observation, not an argument", c.env)
			}
			denied, neutralize := devcontainer.ScreenImageEnvName(c.env)
			if denied != c.wantDenied {
				t.Errorf("ScreenImageEnvName(%q) says denied=%v, want %v — iterion runs %s (%s) in the container and %s gives it %s (%s)",
					c.env, denied, c.wantDenied, c.program, c.where, c.env, c.what, c.measured)
			}
			if neutralize != c.wantNeutralize {
				verb := "reports it and keeps the image's value"
				if neutralize {
					verb = "overrides it with an empty value"
				}
				t.Errorf("an image baking %s: the screen %s, and this row wants neutralize=%v. %s gives %s %s — %s",
					c.env, verb, c.wantNeutralize, c.env, c.program, c.what, c.measured)
			}
		})
	}
}

// TestTheScreenStillEmptiesTheCredentialRoutingSubClass is the
// counterweight the table above owes: "every code channel is only
// reported" must not be readable as "the screen empties nothing". The
// sub-class the screen DOES neutralise is named here, in the same file, so
// a change that emptied the whole class into reports reddens on the way
// out (ARB-121 moved one line, not the guard).
func TestTheScreenStillEmptiesTheCredentialRoutingSubClass(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY",
		"AZURE_OPENAI_API_KEY", "GITHUB_TOKEN", "IMDS_ENDPOINT", "OPENAI_API_BASE",
		"HTTPS_PROXY", "GOOGLE_APPLICATION_CREDENTIALS",
		// The engine's own namespace is mixed and answers by sub-class
		// (#2859): the switch that lifts the private-host guard on a model
		// endpoint and the one that chooses which credential a call
		// carries are of the sub-class this screen empties, where
		// ITERION_VERSION — baked by iterion's own image — is not.
		"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "ITERION_OPENAI_USE_OAUTH",
		// And the guard lifts no credential or host spelling leaks out of:
		// the opt-in that lets a project `.env` plant the routing class,
		// and the switch that makes an engine child read that file at all.
		envtrust.DotenvEndpointsOptIn, envtrust.EnvDotEnvForce,
	} {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied || !neutralize {
			t.Errorf("ScreenImageEnvName(%q) = (%v, %v), want (true, true): the DECLARATIVE redirection of a model call and of the credential it carries is what this screen closes, and it is the whole promise",
				name, denied, neutralize)
		}
	}
}

// TestTheCommandValuedGitFamilyIsToldAndKept holds the `GIT_*` names
// whose value IS a command git runs: all of them read the same
// disposition, so a page that describes them as one group is describing
// the screen and not one example of it.
//
// It carries no claim about what an empty value would do, and that is the
// correction round 5 bought: the page used to state, of all seven, that
// `--env NAME=` does not close them because git runs the empty string as
// a command. Measured on git 2.47.3 in the default image, that is true of
// four (`GIT_SSH_COMMAND`, `GIT_SSH`, `GIT_EDITOR`, `GIT_EXTERNAL_DIFF` —
// `cannot run :`, rc 128) and FALSE of three: an empty `GIT_ASKPASS`
// leaves stderr byte-identical to the name being absent, an empty
// `GIT_PROXY_COMMAND` makes git go direct, and an empty
// `GIT_TEMPLATE_DIR` installs no hook and leaves init/add/commit/log/
// clone at rc 0. Under ARB-121 the question no longer decides anything —
// no code channel of an image is emptied, whether or not the empty value
// would be inert — so the reason this family keeps its value is the
// doctrine, and the measurement that remains is that the channel FIRES
// (the planter's program ran once per file of a plain `git diff`, on a
// fetch, and on the commit an agent makes).
func TestTheCommandValuedGitFamilyIsToldAndKept(t *testing.T) {
	family := []string{
		"GIT_SSH_COMMAND", "GIT_SSH", "GIT_PROXY_COMMAND", "GIT_ASKPASS",
		"GIT_EDITOR", "GIT_EXTERNAL_DIFF", "GIT_TEMPLATE_DIR",
	}
	for _, name := range family {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			t.Errorf("ScreenImageEnvName(%q) says the name is OUTSIDE the class: it is a program git runs, so the operator must at least be told", name)
		}
		if neutralize {
			t.Errorf("ScreenImageEnvName(%q) says NEUTRALISE: the image that bakes it ships the program it names, so docs/sandbox.md states this family as part of the residue — and a page and a screen that disagree is the defect", name)
		}
	}
}

// TestTheEngineGuardLiftRowsNameTheirOwnConstants pins the engine rows of
// the guard-lift sub-class to the constants that OWN the names
// (pkg/sandbox/devcontainer's engineEnvNeutralisedNames). The rows cannot
// all be keyed on their constant where they live — pkg/sandbox/devcontainer
// must not depend on pkg/connection, on pkg/platformcfg, on pkg/backend/mcp
// or on this package — so the spelling is held HERE instead, from the one
// package that imports every owner.
//
// A renamed variable therefore reddens on the row that stopped answering
// for it, instead of leaving a literal classifying a name nothing reads any
// more.
func TestTheEngineGuardLiftRowsNameTheirOwnConstants(t *testing.T) {
	for name, owner := range map[string]string{
		envtrust.DotenvEndpointsOptIn:      "envtrust.DotenvEndpointsOptIn (a project `.env` may plant the routing class)",
		envtrust.EnvDotEnvForce:            "envtrust.EnvDotEnvForce (an engine child reads the workspace `.env`)",
		mcp.EnvExpandUntrustedEnv:          "mcp.EnvExpandUntrustedEnv (a workflow-controlled config expands against the launcher's environment)",
		mcp.EnvServerEnvPassthrough:        "mcp.EnvServerEnvPassthrough (a stdio MCP server child receives the launcher's variables)",
		platformcfg.EnvCloudEnvPassthrough: "platformcfg.EnvCloudEnvPassthrough (workflow text reads the platform's credential-shaped variables)",
		connection.ProjectCatalogEnv:       "connection.ProjectCatalogEnv (a repository-shipped connector package serves a consented connection)",
		EnvSandboxImageEnvAllow:            "runtime.EnvSandboxImageEnvAllow (this screen's own escape hatch)",
	} {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied || !neutralize {
			t.Errorf("ScreenImageEnvName(%q) = (denied %v, neutralize %v), want (true, true): %s lifts a guard on a route or on a credential, and an image's ENV is an inherited grant",
				name, denied, neutralize, owner)
		}
		if reason := strings.TrimSpace(devcontainer.EngineImageEnvReason(name)); reason == "" {
			t.Errorf("EngineImageEnvReason(%q) is empty: %s is answered by the shape or by a family rather than by the row that names the guard it lifts — the sweep would print no reason for it",
				name, owner)
		}
	}
}
