package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// The deny class answers TWO questions about a name, and the table in
// sandbox_tool_env_class_test.go holds only the first: is the name in the
// class at all. This file holds the second — what the runtime DOES when an
// IMAGE bakes it in — for the sub-class where the answer is load-bearing:
// a name that leads to CODE, or to a configuration a program EXECUTES, at
// the startup of a process iterion starts in the container.
//
// It exists because a name of that sub-class left at the image's value
// re-plants every credential the screen empties: the measured case is
// LD_PRELOAD, whose constructor ran in pid 1 of a container started with
// `--env ANTHROPIC_BASE_URL= --env AWS_ACCESS_KEY_ID= …` and put the
// planter's route and keys back, for bash, for node and for an
// npm-started MCP server alike (#2317 round 3, ARB-119). Reporting such a
// name is reporting the hole.
//
// The set is enumerated from the PROGRAMS, not from a list of names: the
// default sandbox image ships bash 5.2, dash as /bin/sh, node 24 with
// npm/npx, a bun-compiled claude CLI, python 3.13, perl, git 2.47, GNU
// tar 1.35, openssl 3.5 and curl — read with `command -v` in the image
// itself — and each row is one startup channel of one of them. Three
// measurements decide a row, on a disposable image through that program's
// own startup path: the planted value RUNS, the empty value is inert, and
// no image iterion publishes bakes the name. The `measured` field is the
// verdict that was observed, so a row cannot be argued into the table.
//
// A row whose disposition the class does not give is a hole or a lie:
// either move the name, or write the measurement that says why it stays.
type startupCodeChannel struct {
	program        string // the program whose startup reads it
	where          string // the site that starts that program
	env            string // the name an image can bake
	what           string // what the planted value runs
	wantDenied     bool
	wantNeutralize bool
	measured       string // what was observed, on a disposable image
}

// TestTheImageScreenNeutralisesEveryStartupCodeChannel holds the class at
// its disposition, row by row.
func TestTheImageScreenNeutralisesEveryStartupCodeChannel(t *testing.T) {
	channels := []startupCodeChannel{
		// The loader: it runs the planter's native code in EVERY
		// dynamically linked process, which is every process in the
		// container.
		{"ld.so", "every process iterion starts there", "LD_PRELOAD",
			"a shared object whose constructor runs before main", true, true,
			"the constructor ran in pid 1 and re-exported ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, AWS_ACCESS_KEY_ID and CLAUDE_CODE_USE_BEDROCK to bash, node and an npm-started MCP server; with `--env LD_PRELOAD=` all four read empty and both runtimes ran clean"},
		{"ld.so", "every process iterion starts there", "LD_AUDIT",
			"the same object through the loader's audit interface", true, true,
			"la_version and the constructor both ran in pid 1, and the planted route came back; `--env LD_AUDIT=` inert"},
		// The shells iterion runs a tool node and an agent's Bash tool
		// with.
		{"bash", "a tool node's command, an agent's Bash tool", "BASH_ENV",
			"a startup file sourced by every NON-interactive bash", true, true,
			"the planted script re-exported the emptied names; empty is bash's default"},
		{"bash", "a tool node's command", "SHELLOPTS",
			"xtrace, which runs the command substitutions of PS4", true, true,
			"`SHELLOPTS=xtrace` with a planted PS4 ran the planter's program once per traced command in a `bash -c`; both emptied, no marker"},
		{"bash", "a tool node's command", "PS4",
			"the substitution xtrace evaluates for every traced command", true, true,
			"same run as SHELLOPTS"},
		{"bash", "a tool node's command", "BASH_FUNC_git%%",
			"a function definition that shadows a command the script calls", true, true,
			"the planted function answered `git --version` in a `bash -c`; `--env BASH_FUNC_git%%=` restored git's own"},
		// node, and the two npm roads into a node child.
		{"node", "the claude, pi and opencode CLIs, an MCP server", "NODE_OPTIONS",
			"--require=<module>, in the CLI's own process", true, true,
			"the planted module re-exported the emptied names; `node -e` runs clean with it empty"},
		{"node", "the claude, pi and opencode CLIs", "NODE_PATH",
			"a bare require resolved from the planter's directory", true, true,
			"empty is node's default (#2317, ARB-119)"},
		{"npm", "an MCP server a workflow declares (`npx <server>`)", "NPM_CONFIG_NODE_OPTIONS",
			"the NODE_OPTIONS of every node child npm spawns", true, true,
			"`npm run` and `npm exec` children read NODE_OPTIONS=--require=/planted/replant.js and the planted module re-exported the credentials, with NODE_OPTIONS itself emptied; `--env NPM_CONFIG_NODE_OPTIONS=` clean"},
		{"npm", "an MCP server a workflow declares", "NPM_CONFIG_SCRIPT_SHELL",
			"the shell every npm script runs under", true, true,
			"the planter's program became the shell of `npm run`, with the script on its argv"},
		{"claude", "every claude_code node's agent CLI", "BUN_OPTIONS",
			"--preload=<module>, inside the CLI's own process", true, true,
			"`claude --version` in the default image ran the planted preload; the CLI is a bun-compiled binary, and `--env BUN_OPTIONS=` left it answering its version"},
		// glibc's and OpenSSL's module roads, which no PYTHON* or node
		// option covers.
		{"python3", "a tool node with `language: python3`, a repo build", "GCONV_PATH",
			"a gconv module dlopened by the charset conversion at interpreter startup", true, true,
			"`python3 -c` AND `python3 -I -c` — the argv iterion's own python node uses — both ran the planted module's constructor; `--env GCONV_PATH=` left python, iconv and the rest working"},
		{"curl", "a tool node's command, the openssl CLI", "OPENSSL_CONF",
			"a configuration file that dlopens the provider module it names", true, true,
			"`curl https://…` and `openssl version` both ran the planted provider's constructor; with it emptied curl, openssl, python's ssl and node's crypto all worked"},
		// git, by the road that needs no file and no HOME.
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_COUNT",
			"`git -c` from the environment: core.sshCommand and credential.helper are programs git RUNS", true, true,
			"`git ls-remote` executed the planted program with the operator's remote on its argv; `--env GIT_CONFIG_COUNT=` left `git config --get core.sshCommand` answering nothing"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_KEY_0",
			"the key half of the same pair", true, true, "same run"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_VALUE_0",
			"the value half of the same pair", true, true, "same run"},
		// tar, which iterion itself starts.
		{"tar", "the scratch bank and its restore, kubernetes populate/export", "TAR_OPTIONS",
			"--checkpoint-action=exec=<program>, prepended to tar's own arguments", true, true,
			"the planter's program ran in a `tar -czf`; `--env TAR_OPTIONS=` left the same tar at rc=0"},

		// The RESIDUE: names of the same shape that stay reported, each
		// with the measurement that says why. A residue row is the honest
		// half of this table — the hole it leaves is stated rather than
		// absent.
		{"ld.so", "every process iterion starts there", "LD_LIBRARY_PATH",
			"the directories the loader searches, hence which library is found", true, false,
			"not emptied: it is how a CUDA-style image finds its own runtime, so an empty value breaks an image doing its job"},
		{"sh", "every Run.Exec of a generated script", "ENV",
			"a startup file, in an interactive POSIX shell", true, false,
			"measured on both shapes: the default image's /bin/sh (dash) and an image whose /bin/sh is bash BOTH ignore it for a non-interactive `sh -c`, where BASH_ENV is the channel that fires"},
		{"python3", "a tool node with `language: python3`, a repo build", "PYTHONPATH",
			"a sitecustomize.py imported at interpreter startup", false, false,
			"measured both ways: the planted sitecustomize ran under a bare `python3 -c` and NOT under `python3 -I`, which is the argv iterion's own python node uses (isolated mode ignores the whole PYTHON* family) — what is left is the repository's own build, which already runs in that container"},
		{"perl", "no site: iterion starts no perl in the container", "PERL5OPT",
			"-M<module>, run at interpreter startup", false, false,
			"measured that the channel FIRES (the planted module ran under `perl -e`), and that no site of iterion's own starts perl there: the deny class takes the names that steer a process ITERION starts"},
		{"git", "the clone, commit and push of the run's work", "HOME",
			"the ~/.gitconfig every insteadOf and credential helper is read from", true, false,
			"not emptied, and assumed as such (question Jo #59): an empty HOME sends every configuration read to the DEFAULT, which under host_state is the operator's own home"},
		{"kubectl", "a deploy-shaped workflow's step", "KUBECONFIG",
			"a kubeconfig whose exec plugin is a program", true, false,
			"not emptied: emptying sends the client to its own default, the operator's ~/.kube/config under host_state, which is worse than the planted path"},
		{"sh", "every Run.Exec of a generated script", "PATH",
			"the binaries the script's bare names resolve to", true, false,
			"not emptied: the container would resolve no command, and iterion's own `full` image bakes it"},
		{"nix", "the devbox install of the post-create prologue", "NIX_CONFIG",
			"plugin-files, dlopened on any nix invocation", true, false,
			"not emptied: an empty nix.conf breaks the provisioning step iterion itself runs"},
	}

	// Anti-vacuity: the table is the claim, and an empty or shrunken one
	// would certify nothing. 16 neutralised rows and 8 residue rows at the
	// time of writing.
	if len(channels) < 20 {
		t.Fatalf("the startup-code class holds %d rows: a table this small has stopped describing the programs the image ships", len(channels))
	}
	neutralised := 0
	for _, c := range channels {
		if c.wantNeutralize {
			neutralised++
		}
	}
	if neutralised < 14 {
		t.Fatalf("only %d row(s) of the startup-code class neutralise: the sub-class this file exists for has been emptied", neutralised)
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

// TestThePromiseAboutTheNeutralisedSubClassIsBoundedToTheEnvRoad is the
// npmrc measurement, kept.
//
// A FILE the image lays down re-plants a neutralised name with NO
// environment variable at all: an image writing
// `node-options=--require=/planted/replant.js` into npm's own global
// config — `/usr/etc/npmrc` in the default image, which
// `npm config get globalconfig` names and no variable points at — had
// `npm run` and `npm exec` children start with
// `NODE_OPTIONS=--require=…` and the planted module re-export the
// credentials, with every neutralisation of the table applied.
//
// So the unbounded sentence this guard refuses — "the repository's value
// never reaches a process" — was measurably false, and the page owes the
// bounded one plus the residue. The forbidden string is the sentence
// itself: a page that drifts back to it fails here, which is the only
// thing that makes a prose promise testable.
func TestThePromiseAboutTheNeutralisedSubClassIsBoundedToTheEnvRoad(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "sandbox.md"))
	if err != nil {
		t.Fatalf("read docs/sandbox.md: %v", err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")

	const unbounded = "for a name of that sub-class the repository's value never reaches a process —"
	if strings.Contains(page, unbounded) {
		t.Errorf("docs/sandbox.md promises %q: an image-laid npmrc re-plants NODE_OPTIONS with no variable at all (measured), so the promise holds for the ENV road and must say so", unbounded)
	}
	for _, required := range []string{
		"the repository's value never reaches a process *by way of the image's `ENV`*",
		"What the ENV road does not close, measured.",
		"still define the container\" auto mode's rule",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("docs/sandbox.md no longer states %q — the bounded promise and the residue beside it are what the npmrc measurement bought", required)
		}
	}
}
