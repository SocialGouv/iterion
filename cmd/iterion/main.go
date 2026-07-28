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
			os.Exit(2)
		}
		os.Exit(1)
	}
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
//
// Each group it hardens is stamped with groupHelpOnlyAnnotation, so a
// group that carries its OWN positional contract (`iterion models
// [provider/model-id]`, runnable and already validating) stays
// distinguishable from one that exists only to host subcommands.
func rejectUnknownSubcommands(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		rejectUnknownSubcommands(sub)
	}
	if cmd.HasSubCommands() && !cmd.Runnable() && cmd.Args == nil {
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
		if cmd.Annotations == nil {
			cmd.Annotations = map[string]string{}
		}
		cmd.Annotations[groupHelpOnlyAnnotation] = "true"
	}
}

// groupHelpOnlyAnnotation marks a group whose only behaviour is printing
// its own help — the shape rejectUnknownSubcommands installs. Read by
// the coverage test to tell a help-only group from a command that hosts
// subcommands AND takes positional args of its own.
const groupHelpOnlyAnnotation = "iterion.group_help_only"

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
