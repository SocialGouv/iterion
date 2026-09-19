// Command iterion-live-status is the entry point behind
// `task test:live:status`. It answers "do the live e2e still pass?"
// without spending a cent: read the committed ledger, enumerate the
// Taskfile's `test:live:*` targets, and print each with its last
// verdict (or `never`) sorted by staleness.
//
// See docs/live-e2e-coverage.md and #1422. This binary never spends
// on a model call — no credential is read, no network is touched.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/liveledger"
)

func main() {
	ledgerPath := flag.String("ledger", "", "path to the ledger file (default: e2e/testdata/live/ledger.json under the repo root)")
	taskfilePath := flag.String("taskfile", "", "path to the Taskfile (default: Taskfile.yml under the repo root)")
	writeBack := flag.Bool("write-back", true, "seed missing `never` rows into the ledger when the Taskfile has targets the file does not; disable for a strict read-only read")
	flag.Parse()

	opts := liveledger.StatusOptions{
		LedgerPath:   *ledgerPath,
		TaskfilePath: *taskfilePath,
		WriteBack:    *writeBack,
	}
	if opts.LedgerPath == "" {
		root, ok := findRoot()
		if !ok {
			fmt.Fprintln(os.Stderr, "iterion-live-status: could not locate the repository root (no go.mod on the way up)")
			os.Exit(2)
		}
		opts.LedgerPath = filepath.Join(root, liveledger.DefaultRelPath)
	}
	if opts.TaskfilePath == "" {
		root, ok := findRoot()
		if !ok {
			fmt.Fprintln(os.Stderr, "iterion-live-status: could not locate the repository root (no go.mod on the way up)")
			os.Exit(2)
		}
		opts.TaskfilePath = filepath.Join(root, liveledger.TaskfileRelPath)
	}

	res, err := liveledger.Status(os.Stdout, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iterion-live-status: %v\n", err)
		os.Exit(1)
	}
	if res.Added > 0 {
		fmt.Fprintf(os.Stderr, "\niterion-live-status: seeded %d `never` row(s) into %s\n", res.Added, opts.LedgerPath)
	}
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
