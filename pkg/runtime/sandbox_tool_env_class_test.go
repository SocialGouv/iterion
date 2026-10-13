package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// Every program iterion starts INSIDE the container runs under that
// container's environment, which a found devcontainer.json seeds from the
// reviewed repository (#2316). Several of those programs take options, a
// configuration file or a helper program from the environment — and an
// option is enough: GNU tar's --checkpoint-action=exec=, go's -toolexec
// through the file GOENV names, a kubeconfig's exec credential plugin all
// run a program the repository chose, inside a step the OPERATOR started.
//
// So the deny class owes one entry per such name, and this table is the
// claim: the programs iterion itself runs there, and what each of them
// reads. It exists because the hole was found twice in a row at a program
// nobody had listed (tar, then the go a bot's build step runs) — the
// previous sweeps had read the agent CLIs' own variables, never iterion's
// container-side commands.
//
// A new in-container command adds a row. A row whose name the class does
// not refuse is a hole: either deny the name, or write here why the
// program cannot be reached.
func TestDeniedEnvClassCoversTheCommandsIterionRunsInTheContainer(t *testing.T) {
	for _, tc := range []struct {
		command string // the program iterion starts in the container
		where   string // the site that starts it
		env     string // the name that gives the program options, a config file or a helper
		what    string // what the planted value buys
	}{
		{"sh", "every Run.Exec of a generated script", "ENV", "a startup file sourced by the shell"},
		{"sh", "every Run.Exec of a generated script", "BASH_ENV", "the same, when the image's sh is bash"},
		{"sh", "every Run.Exec of a generated script", "PATH", "the binaries the script's bare names resolve to"},
		{"bash", "a tool node's command", "BASH_FUNC_x%%", "a function that shadows a command the script calls"},
		{"bash", "a tool node's command", "SHELLOPTS", "xtrace, whose PS4 substitutions then run"},
		{"tar", "kubernetes populate/export, scratch bank and restore", "TAR_OPTIONS", "--checkpoint-action=exec=<program>"},
		{"git", "the clone, commit and push of the run's work", "GIT_SSH_COMMAND", "the transport program"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_GLOBAL", "a config file, hence insteadOf and every helper"},
		{"git", "the clone, commit and push of the run's work", "EDITOR", "the program a commit without a message opens"},
		{"nix", "the devbox install of the post-create prologue", "NIX_CONFIG", "plugin-files, dlopened on any invocation"},
		{"npx", "an MCP server a workflow declares", "NPM_CONFIG_NODE_OPTIONS", "--require <module>, in the server's own process"},
		{"node", "the claude, pi and opencode CLIs", "NODE_OPTIONS", "--require <module>"},
		{"go", "a bot's build or verify step", "GOFLAGS", "-toolexec=<program>"},
		{"go", "a bot's build or verify step", "GOENV", "a file that sets GOFLAGS itself"},
		{"kubectl", "a deploy-shaped workflow's step", "KUBECONFIG", "a kubeconfig whose exec plugin is a program"},
		{"docker", "a deploy-shaped workflow's step", "DOCKER_CONFIG", "a config whose credsStore names a program"},
		{"iterion __claw-runner", "every sandboxed claw node", "ITERION_OPENAI_USE_OAUTH", "which instrument the node spends"},
		{"iterion __claw-runner", "every sandboxed claw node", "SENTRY_DSN", "an outbound reporting channel, switched on"},
	} {
		t.Run(tc.command+"/"+tc.env, func(t *testing.T) {
			if !devcontainer.DeniedEnvKey(tc.env) {
				t.Errorf("iterion runs %s (%s) in the container and %s gives it %s, but devcontainer.DeniedEnvKey refuses neither: a reviewed repository's containerEnv reaches that step",
					tc.command, tc.where, tc.env, tc.what)
			}
		})
	}
}

// The table above is a claim about THIS tree: the commands it names must
// still be the ones iterion starts in a container. A command that leaves
// the tree takes its rows with it — and the hole this guards against is a
// command that ARRIVES without them, so the inventory is read from the
// code, not from memory.
func TestTheInContainerCommandTableNamesCommandsThisTreeRuns(t *testing.T) {
	// The argv heads of every Run.Exec / ExecOpts call site of the runtime,
	// plus the generated post-create prologue: a literal first word.
	execArgv := regexp.MustCompile(`\[\]string\{"([a-z][a-z0-9_-]*)"`)
	seen := map[string]bool{}
	for _, name := range []string{"scratch_bank.go", "sandbox.go", "sandbox_mounts.go", "sandbox_devbox.go", "sandbox_secret_files.go"} {
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range execArgv.FindAllStringSubmatch(string(body), -1) {
			seen[m[1]] = true
		}
	}
	// The commands the table claims iterion starts, which this file's own
	// sites must still show. The rest of the table's commands (the CLIs,
	// npx, go, kubectl, docker) are started by other packages or by an
	// operator-authored node, so they are not greppable here.
	for _, want := range []string{"sh", "tar"} {
		if !seen[want] {
			t.Errorf("the table claims iterion runs %q in the container, but no Run.Exec site of this package starts it any more: re-read the table against the tree", want)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no in-container command found at all: the inventory regexp no longer matches this package")
	}
	// A command this package starts that the table never mentions is the
	// shape that cost two rounds of review: surface it rather than let it
	// pass. Listed here are the ones reviewed as carrying no env-named
	// options, program or configuration file.
	reviewed := map[string]string{
		"sh":       "in the table",
		"tar":      "in the table",
		"rm":       "coreutils reads no env for options",
		"mkdir":    "coreutils reads no env for options",
		"cp":       "coreutils reads no env for options",
		"id":       "coreutils reads no env for options",
		"test":     "a shell builtin",
		"true":     "a no-op probe",
		"cat":      "coreutils reads no env for options",
		"printf":   "a shell builtin",
		"env":      "started only to set names explicitly",
		"getent":   "reads no env for options",
		"devbox":   "its nix is in the table (NIX_CONFIG); DEVBOX_NO_PROMPT is set by the prologue itself",
		"nix":      "in the table",
		"git":      "in the table",
		"iterion":  "in the table (the claw runner)",
		"kubectl":  "in the table",
		"docker":   "in the table",
		"chmod":    "coreutils reads no env for options",
		"chown":    "coreutils reads no env for options",
		"ln":       "coreutils reads no env for options",
		"mv":       "coreutils reads no env for options",
		"sleep":    "coreutils reads no env for options",
		"touch":    "coreutils reads no env for options",
		"umask":    "a shell builtin",
		"bash":     "in the table",
		"readlink": "coreutils reads no env for options",
		"stat":     "coreutils reads no env for options",
		"uname":    "reads no env for options",
		"whoami":   "coreutils reads no env for options",
	}
	var unreviewed []string
	for cmd := range seen {
		if reviewed[cmd] == "" {
			unreviewed = append(unreviewed, cmd)
		}
	}
	if len(unreviewed) > 0 {
		t.Errorf("this package starts %s in the container, and neither the table above nor its reviewed list mentions it: say what it reads from the environment, then deny it or write down why it is unreachable (%s)",
			strings.Join(unreviewed, ", "), "#2316")
	}
}
