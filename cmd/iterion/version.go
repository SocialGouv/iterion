package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var versionCommitOnly bool

// versionOutput resolves what `iterion version` prints for the given build
// metadata. It takes both values as arguments rather than reading them from
// cli: a test binary carries no injected SHA, so the populated-commit branch
// is unreachable from a test that drives the command end to end.
func versionOutput(full, commit string, commitOnly bool) (string, error) {
	if !commitOnly {
		return full, nil
	}
	// A build with neither an -X ldflag nor VCS build info carries no SHA.
	// Printing an empty line would hand a script the empty string as if it
	// were the commit — fail visibly instead.
	if commit = strings.TrimSpace(commit); commit == "" {
		return "", fmt.Errorf("this binary carries no commit SHA (built without -ldflags -X ...appinfo.Commit and without VCS build info)")
	}
	return commit, nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := versionOutput(cli.Version(), cli.RawCommit(), versionCommitOnly)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
		return nil
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionCommitOnly, "commit", false, "Print only the git commit SHA")
	rootCmd.AddCommand(versionCmd)
}
