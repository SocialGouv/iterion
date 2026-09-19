// Command iterion-live-status is the entry point behind
// `task test:live:status`. It answers "do the live e2e still pass?"
// without spending a cent: read the committed ledger, enumerate the
// Taskfile's `test:live:*` recording targets, and print each with its
// last verdict (or `never`) sorted by staleness.
//
// The command is a READ by default: it never writes the committed ledger
// unless -write-back is passed explicitly. See docs/live-e2e-coverage.md
// and #1422. This binary never spends on a model call — no credential is
// read, no network is touched.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/liveledger"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one status invocation and returns the process exit code
// (0 ok, 1 error, 2 setup failure). Kept separate from main so tests
// drive the CLI boundary without a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("iterion-live-status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ledgerPath := fs.String("ledger", "", "path to the ledger file (default: e2e/testdata/live/ledger.json under the repo root)")
	taskfilePath := fs.String("taskfile", "", "path to the Taskfile (default: Taskfile.yml under the repo root)")
	writeBack := fs.Bool("write-back", false, "seed missing never rows into the committed ledger when the Taskfile has recording targets the file does not; a status read never writes unless this is passed")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	opts := liveledger.StatusOptions{
		LedgerPath:   *ledgerPath,
		TaskfilePath: *taskfilePath,
		WriteBack:    *writeBack,
	}
	if opts.LedgerPath == "" || opts.TaskfilePath == "" {
		root, ok := findRoot()
		if !ok {
			fmt.Fprintln(stderr, "iterion-live-status: could not locate the repository root (no go.mod on the way up)")
			return 2
		}
		if opts.LedgerPath == "" {
			opts.LedgerPath = filepath.Join(root, liveledger.DefaultRelPath)
		}
		if opts.TaskfilePath == "" {
			opts.TaskfilePath = filepath.Join(root, liveledger.TaskfileRelPath)
		}
	}

	res, err := liveledger.Status(stdout, opts)
	if err != nil {
		fmt.Fprintf(stderr, "iterion-live-status: %v\n", err)
		return 1
	}
	if res.Added > 0 && !*writeBack {
		fmt.Fprintf(stderr, "%d recording target(s) missing from the ledger — shown as never; run with -write-back to seed them\n", res.Added)
	}
	if res.Added > 0 && *writeBack {
		fmt.Fprintf(stderr, "iterion-live-status: seeded %d never row(s) into %s\n", res.Added, opts.LedgerPath)
	}
	return 0
}

// findRoot walks up from cwd to the first directory that contains a
// go.mod. Returns the root and true on success.
func findRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
