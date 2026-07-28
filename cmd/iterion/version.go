package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var versionCommitOnly bool

// commitDisplayLen mirrors the truncation appinfo.FullVersion applies to the
// `+<sha>` suffix, so `--commit` always equals the SHA `iterion version`
// prints. The raw value's width is build-dependent — a plain `go build` infers
// the full 40-char vcs.revision, release builds inject `git rev-parse --short`,
// and the container images pass a 40-char `github.sha` — which would otherwise
// make the flag's output vary with the binary's provenance.
const commitDisplayLen = 12

// versionInfo is the seam tests stub. A `go test` binary carries no VCS
// stamping and no ldflags, so cli.RawCommit() is always empty under test and
// the populated-SHA path would be unreachable end to end.
var versionInfo = func() (full, commit string) { return cli.Version(), cli.RawCommit() }

// versionOutput resolves what `iterion version` prints for the given build
// metadata.
func versionOutput(full, commit string, commitOnly bool) (string, error) {
	if !commitOnly {
		return full, nil
	}
	// A build with neither an -X ldflag nor VCS build info carries no SHA, and
	// the Dockerfile defaults `ARG COMMIT=unknown` so a bare `docker build .`
	// injects that sentinel. Printing either would hand a script a bogus value
	// as if it were the commit — fail visibly instead.
	commit = strings.TrimSpace(commit)
	if commit == "" || commit == "unknown" {
		return "", fmt.Errorf("this binary carries no commit SHA (built without -ldflags -X ...appinfo.Commit and without VCS build info)")
	}
	if len(commit) > commitDisplayLen {
		commit = commit[:commitDisplayLen]
	}
	return commit, nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		full, commit := versionInfo()
		out, err := versionOutput(full, commit, versionCommitOnly)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
		return nil
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionCommitOnly, "commit", false, "Print only the git commit SHA (as embedded in `iterion version`)")
	rootCmd.AddCommand(versionCmd)
}
