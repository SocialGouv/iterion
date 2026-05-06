// Command iterion is the CLI for the iterion workflow engine.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/SocialGouv/iterion/pkg/cli"
	// Register the cloud-mode store opener (Mongo + S3) so
	// ITERION_MODE=cloud works out of the box. The package's init()
	// calls store.RegisterCloudOpener; no other reference is needed.
	// Cloud-ready plan §F (T-19).
	_ "github.com/SocialGouv/iterion/pkg/store/cloud"
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if jsonOutput {
			newPrinter().JSON(map[string]string{"error": err.Error()})
		} else {
			cli.PrintError(os.Stderr, err)
		}
		os.Exit(1)
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
