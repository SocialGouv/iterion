// Command iterion is the CLI for the iterion workflow engine.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	// Embed the IANA tz database so the TZ env var works in minimal
	// containers (debian-slim and the sandbox images ship no tzdata) —
	// log timestamps and time.Local then honour the operator's timezone.
	_ "time/tzdata"

	"github.com/SocialGouv/iterion/internal/dotenv"
	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/spf13/cobra"
)

var jsonOutput bool

var rootCmd = &cobra.Command{
	Use:   "iterion",
	Short: "Build, run and orchestrate agentic AI workflows",
	Long: "iterion — build, run and orchestrate agentic AI workflows\n" +
		"\n" +
		"The nearest `.env` walking up from the working directory is auto-loaded, filling in\n" +
		"every variable left unset: a convenience for API keys next to a project. It does not\n" +
		"CONFIGURE iterion — a name a `.env` filled speaks for that project, not for the\n" +
		"deployment, so it decides nothing an operator answers for: where plugins are\n" +
		"installed, which table prices a model and where that table is cached, when a node\n" +
		"compacts, and which credential a gateway is presented. A route it names — a *_URL\n" +
		"or *_URI, an *_ENDPOINT, a *_HOST or *_HOSTS, a *_DSN, a proxy — is not applied at\n" +
		"all: a project `.env` does not choose where a connection goes, and the refusal\n" +
		"names the variable and the way past it. Export those in the environment iterion\n" +
		"runs with, or export ITERION_DOTENV_ENDPOINTS=allow to let the file plant its\n" +
		"routes. The same class is refused on the studio lane and in the desktop's own\n" +
		"env file.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	rootCmd.Version = cli.Version()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}

func main() {
	// The permission hook is the grok/kimi anti-prompt-injection
	// boundary. Both CLIs spawn it with cwd = the gated workspace and
	// re-execute it on every tool call; a timeout is an ALLOW. So this
	// process must not read anything the gated agent can write —
	// loadDotEnvFromCwd walks up from cwd applying an unbounded `.env`
	// line by line (a 37 MB file was measured at 5.4 s, past grok's
	// hook timeout), and errtrack.Init then honours a SENTRY_DSN that
	// same file just planted (#498 review, R6fa6d2). The hook needs
	// neither. Detected from argv, not cobra, so it runs before any
	// of that work.
	if !isPermissionHookInvocation() && !isContractsInvocation() {
		// Auto-load `.env` from the working directory (and walk up to
		// the closest one) before subcommands run, so iterion behaves
		// like every other modern CLI tool when API keys / model env
		// vars live in a `.env` next to a project. Pre-existing env
		// vars take precedence; .env only fills in missing keys. Which
		// invocations get that convenience — a human's, not a child the
		// engine spawned against a repository — is the predicate's.
		//
		// The `.env` walk also aimed errtrack at a host of the file's
		// choosing (#498 review, R6fa6d2): SENTRY_DSN is a wire like
		// the rest and sits in the routing class — a crash envelope
		// carries the operator's stack and machine context, so a
		// planted DSN delivers them where the file said. Init still
		// sits after the load: the opt-in (or the operator's own DSN)
		// applies to it as to every other name.
		//
		// FILLING a name is not answering for it: applyDotEnv marks
		// every key it sets (internal/envtrust), and the reads that
		// need the operator's own answer — the plugin home, the model
		// table that prices a run and the cache file it is read from,
		// the window a node compacts at, the gateway catalog and the
		// credential it presents — resolve the INHERITED value and take
		// a planted one as nothing said. One CLASS is refused outright
		// rather than marked: the routing-shaped names (a *_URL or
		// *_URI, an *_ENDPOINT, a *_HOST or *_HOSTS, a *_DSN, a proxy),
		// which decide where a connection goes or what reaches it — a
		// planted route would point every wire, the operator's
		// credentials included, at a host the file chose. A `.env`
		// that legitimately configures its own gateway needs the
		// operator's ITERION_DOTENV_ENDPOINTS=allow. The file sits in
		// a repository, including the one a run is reviewing. The
		// studio lane plants the same file by another road
		// (projectenv.Snapshot) and asks the same question there; the
		// desktop's own env-file loader asks it too.
		if shouldLoadWorkspaceDotEnv() {
			loadDotEnvFromCwd()
		}

		// Error tracking is opt-in: with SENTRY_DSN unset this is a no-op
		// and iterion behaves exactly as it did. Init sits after the .env
		// load so a project can carry its DSN there, and before anything
		// else runs so the very first panic is already covered.
		// The root has no logger of its own yet; NewFromEnv honours
		// ITERION_LOG_FORMAT so a failed init does not break the JSON
		// stream of a server/runner/dispatch process.
		errtrack.Init(errtrack.Config{Logger: iterlog.NewFromEnv(os.Stderr), ServerName: invokedCommand()})
		defer errtrack.Flush()
		defer func() {
			if r := recover(); r != nil {
				errtrack.CapturePanic(r)
				// Re-panic: the tracker observes the crash, it does not
				// change how the process dies.
				panic(r)
			}
		}()
	}

	rejectUnknownSubcommands(rootCmd)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		// The command already wrote its diagnosis (a --json result with
		// valid:false): exit non-zero without a second JSON document. Not an
		// incident either, so nothing is captured — but whatever the run
		// captured before is flushed, as on every other exit.
		if errors.Is(err, cli.ErrReported) {
			errtrack.Flush()
			os.Exit(1)
		}
		if jsonOutput {
			newPrinter().JSON(map[string]string{"error": err.Error()})
		} else {
			cli.PrintError(os.Stderr, err)
		}
		// Exit 2 for user-input errors (bad flag, missing file, …) so
		// shell scripts can branch on the distinction; 1 otherwise.
		if errors.Is(err, cli.ErrUserInput) {
			// A typo is not an incident: user-input errors are never
			// reported to the tracker.
			errtrack.Flush()
			os.Exit(2)
		}
		var attention *cli.AttentionError
		if errors.As(err, &attention) {
			// A health read succeeded; its attention result is an expected
			// operational signal, not an application exception.
			errtrack.Flush()
			os.Exit(3)
		}
		// os.Exit skips the deferred flush, so the fatal error is
		// captured AND flushed here or it never leaves the process.
		errtrack.CaptureError(err, map[string]any{"command": invokedCommand()})
		errtrack.Flush()
		os.Exit(1)
	}
}

// isWorkspaceStudioInvocation detects the one long-lived command that must
// keep the host environment clean before Cobra parses flags. A unified Studio
// loads each project's dotenv in a child shell and passes the resulting
// snapshot to that project's executions; preloading cwd/.env here would make
// the first project leak into every runtime.
func isWorkspaceStudioInvocation() bool {
	hasStudio := false
	hasWorkspace := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "studio":
			hasStudio = true
		case "--workspace":
			hasWorkspace = true
		default:
			if arg == "--workspace=true" || arg == "--workspace=1" {
				hasWorkspace = true
			}
		}
	}
	return hasStudio && hasWorkspace
}

// invokedCommand returns the full command path being run
// ("iterion runner", "iterion remote runs launch"), used to tag tracker
// events with the surface they came from. Flags and their values are
// deliberately dropped — they carry operator data.
func invokedCommand() string {
	cmd, _, err := rootCmd.Find(os.Args[1:])
	if err != nil || cmd == nil {
		return rootCmd.Name()
	}
	return cmd.CommandPath()
}

// firstPositionalArg returns what argv names as the subcommand: the first
// argument that is not a flag, or "" when there is none. Cobra is not consulted
// because every caller has to decide BEFORE loadDotEnvFromCwd, which runs
// before Execute. A `--` ends the search: what follows belongs to whatever
// is on the other side of it, not to this binary's command tree.
func firstPositionalArg() string {
	for _, a := range os.Args[1:] {
		if a == "--" {
			return ""
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// isPermissionHookInvocation reports whether this process is the grok/kimi
// PreToolUse hook.
func isPermissionHookInvocation() bool {
	return firstPositionalArg() == "__permission-hook"
}

// isInternalSubcommandInvocation reports whether this process runs one of the
// hidden `__`-prefixed subcommands: the in-container claw runner, the shard
// dispatcher, the MCP stdio helpers, the agent-CLI hooks. None of them is a
// human's CLI call — each is re-entered by the engine, or by an agent CLI the
// engine configured, with cwd = the workspace, and each gets its environment
// from the process that spawned it.
//
// Two of them re-execute on EVERY tool call of a session
// (__permission-hook, __claude-hook-drain) where a `.env` walk is also a
// per-call cost on a file the gated agent writes (R6fa6d2); the others
// re-enter the CLI holding a credential the engine resolved. The class is the
// prefix, so a hidden subcommand added later is covered rather than
// re-introducing the hole.
func isInternalSubcommandInvocation() bool {
	return strings.HasPrefix(firstPositionalArg(), "__")
}

// shouldLoadWorkspaceDotEnv reports whether this invocation gets the `.env`
// convenience loadDotEnvFromCwd implements. It is the whole decision: main
// calls nothing else.
//
// The convenience belongs to a human running the CLI in a project of their
// own. A process the engine spawned against a workspace — a tool node's
// command, a CLI agent, the in-container claw runner, an iterion a bot script
// re-enters — already holds the environment the engine handed it, so reading
// the workspace `.env` on top only lets the repository under review fill in
// the names nobody set on purpose: the provider endpoints a credential is
// spent against first among them (#2844). The operator's own `.env` still
// reaches such a child, as every other variable does — through the
// environment its parent passed on.
func shouldLoadWorkspaceDotEnv() bool {
	// `studio --workspace` serves a workspace it must not take its own
	// configuration from; its reason is its own and no override applies.
	if isWorkspaceStudioInvocation() {
		return false
	}
	if envtrust.SpawnedByEngine() || isInternalSubcommandInvocation() {
		return envtrust.DotEnvForced()
	}
	return true
}

// rejectUnknownSubcommands makes every GROUP command — one that only
// hosts subcommands and does nothing itself, like `iterion bundle` or
// `iterion remote runs` — reject an argument it has no subcommand for.
//
// Cobra's default (legacyArgs) does that check for the root command
// only; every other group silently accepts arbitrary args, and since a
// group is not Runnable, cobra falls through to printing help and
// exiting 0. A typo or a retired command (`iterion bundle init`) then
// looks like a success. NoArgs turns it into `unknown command "init"
// for "iterion bundle"` with a non-zero exit.
//
// Applied by walking the tree rather than by annotating each group, so
// a group added later inherits the behaviour instead of re-introducing
// the silent no-op. Groups that deliberately take positional args of
// their own already set Args or a Run and are left untouched.
//
// Both fields are required: cobra returns ErrHelp for a non-Runnable
// command BEFORE it validates args, so Args alone would never be
// consulted. RunE makes the group Runnable — it only ever prints help,
// which is what a bare `iterion bundle` did before.
func rejectUnknownSubcommands(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		rejectUnknownSubcommands(sub)
	}
	if cmd.HasSubCommands() && !cmd.Runnable() && cmd.Args == nil {
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	}
}

// loadDotEnvFromCwd walks up from $CWD looking for a `.env` file and
// applies it. We stop at the first one found OR at the filesystem
// root. Already-set env vars are preserved (parent shell wins).
//
// The format is the standard one: `KEY=VALUE` per line, optional
// `#` comments, optional surrounding `"` or `'` quotes around the
// value. We deliberately don't pull in godotenv to avoid a
// dependency for ~30 lines of code.
func loadDotEnvFromCwd() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if applyDotEnv(candidate) {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func applyDotEnv(path string) bool {
	entries, err := dotenv.Parse(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		key, val := entry.Key, entry.Value
		// The provenance record is not a value a repository gets to write.
		// Without this, one `.env` line naming ITERION_DOTENV_PLANTED seeded
		// the whole planted set from the file itself — marking the
		// OPERATOR's own variables as planted, and stripping every plugin
		// they installed of its authority. A marker can then only ever be
		// what an ancestor process exported — in any case on Windows, where
		// the variable is the same one whatever the case of its name.
		if envtrust.CanonicalName(key) == envtrust.EnvPlantedNames {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			// A name that decides where a connection goes is NOT applied.
			// The loader is the site that knows it is planting, so it is one
			// of the lanes that closes the whole routing class (ARB-122):
			// the *_URL/_URI wires, the *_ENDPOINTs, the *_HOST/*_HOSTS
			// lists, the proxies, the *_DSN, the git transport family. Every
			// child inherits what lands here and the reads are not all
			// iterion's — the vendored claw provider packages and SDKs read
			// these names themselves — so a route never planted is a route
			// nothing can follow, where a guard at one read site closes
			// exactly that site; the per-name credential tables
			// (internal/envtrust) stay as the belt at the reads. The same
			// predicate refuses the operator grade: the infra names the
			// bot-var gate denies a stored record, widened to the server's
			// credential spellings — the file a project holds has no more
			// authority over ITERION_SANDBOX_OVERRIDE or ITERION_JWT_SECRET
			// than a bot does. The operator's channels are the environment
			// itself, or ITERION_DOTENV_ENDPOINTS=allow for the ROUTES. The
			// studio lane (projectenv.Snapshot) and the desktop env-file
			// loader ask the same question at their own plants. The guard
			// sits before the planted mark: a refused name does not exist,
			// it is not "planted but distrusted".
			if envtrust.DotenvPlantRefused(key) {
				// The refusal goes to stderr directly, not through the
				// level-configured logger: the level it would ride on is
				// itself something this file may have planted one line above
				// (ITERION_LOG_LEVEL=error mutes the message the moment the
				// file wants its route applied quietly). A safety refusal is
				// not a log level.
				fmt.Fprintln(os.Stderr, "iterion: "+envtrust.RoutingRefusalLine(key, "the project `.env`", path))
				continue
			}
			// The parser kept this file's bytes as bytes; no shell of ours
			// evaluates them — but every child MIGHT (a value reaches a
			// run's own shell tooling one fork away), so a value spelling
			// command substitution is refused rather than planted: nothing
			// of ours executes it, and nothing downstream gets handed the
			// trigger either.
			if envtrust.HasCommandSubstitution(val) {
				fmt.Fprintln(os.Stderr, "iterion: "+envtrust.CommandSubstitutionRefusalLine(key, "the project `.env`", path))
				continue
			}
			_ = os.Setenv(key, val)
			// A `.env` sits in a repository — including one under review — so
			// what it plants must not later speak for the operator. The value
			// is used exactly as before; only questions of AUTHORITY consult
			// this (see internal/envtrust), and the record travels to child
			// processes so a fork cannot launder it.
			envtrust.MarkPlanted(key)
		}
	}
	return true
}

func mustMarkRequired(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("flag %q: %v", n, err))
		}
	}
}

func newPrinter() *cli.Printer {
	format := cli.OutputHuman
	if jsonOutput {
		format = cli.OutputJSON
	}
	return cli.NewPrinter(format)
}
