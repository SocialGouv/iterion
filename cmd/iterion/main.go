// Command iterion is the CLI for the iterion workflow engine.
package main

import (
	"bufio"
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

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/spf13/cobra"
)

var jsonOutput bool

var rootCmd = &cobra.Command{
	Use:           "iterion",
	Short:         "Workflow orchestration engine",
	Long:          "iterion — workflow orchestration engine",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	rootCmd.Version = cli.Version()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}

func main() {
	// Auto-load `.env` from the working directory (and walk up to
	// the closest one) before subcommands run, so iterion behaves
	// like every other modern CLI tool when API keys / model env
	// vars live in a `.env` next to a project. Pre-existing env
	// vars take precedence; .env only fills in missing keys.
	loadDotEnvFromCwd()

	// Error tracking is opt-in: with SENTRY_DSN unset this is a no-op
	// and iterion behaves exactly as it did. Init sits after the .env
	// load so a project can carry its DSN there, and before anything
	// else runs so the very first panic is already covered.
	errtrack.Init(errtrack.Config{Logger: bootLogger(), ServerName: invokedCommand()})
	defer errtrack.Flush()
	defer func() {
		if r := recover(); r != nil {
			errtrack.CapturePanic(r)
			// Re-panic: the tracker observes the crash, it does not
			// change how the process dies.
			panic(r)
		}
	}()

	rejectUnknownSubcommands(rootCmd)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
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
		// os.Exit skips the deferred flush, so the fatal error is
		// captured AND flushed here or it never leaves the process.
		errtrack.CaptureError(err, map[string]any{"command": invokedCommand()})
		errtrack.Flush()
		os.Exit(1)
	}
}

// bootLogger is the minimal stderr logger the root command uses before
// a subcommand builds its own — currently only to report a failed
// errtrack init. It honours ITERION_LOG_FORMAT so the line does not
// break the JSON stream of a server/runner/dispatch process.
func bootLogger() *iterlog.Logger {
	format := iterlog.FormatHuman
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ITERION_LOG_FORMAT")), "json") {
		format = iterlog.FormatJSON
	}
	level, _ := iterlog.ResolveLevel("", "ITERION_LOG_LEVEL")
	return iterlog.NewWithFormat(level, os.Stderr, format)
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
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		// Allow `export KEY=VALUE` lines (common shell convention).
		key = strings.TrimPrefix(key, "export ")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		// Strip a single surrounding pair of matching quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
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
