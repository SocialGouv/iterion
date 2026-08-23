package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/permissionhook"
	"github.com/spf13/cobra"
)

var permissionHookCmd = &cobra.Command{
	Use:    "__permission-hook",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		probe, _ := cmd.Flags().GetBool("probe")
		if probe {
			return nil
		}
		backend, _ := cmd.Flags().GetString("backend")
		// Only --backend is required. An absent or empty --policy-b64 is
		// deliberately handed to Run, which fails CLOSED by emitting a deny;
		// erroring out here would exit non-zero with empty stdout, and both
		// CLIs read that as allow (PR #498 review, R7af386).
		policyB64, _ := cmd.Flags().GetString("policy-b64")
		if backend == "" {
			return fmt.Errorf("__permission-hook: --backend is required")
		}
		return permissionhook.Run(backend, policyB64, os.Stdin, os.Stdout)
	},
}

func init() {
	permissionHookCmd.Flags().String("backend", "", "native CLI hook dialect")
	// By value, never a path: the CLI freezes this argv at session start, so
	// the gated agent cannot rewrite the gate's own authority mid-run.
	permissionHookCmd.Flags().String("policy-b64", "", "base64 permission policy (carried by value)")
	permissionHookCmd.Flags().Bool("probe", false, "verify that this binary supports permission hooks")
	rootCmd.AddCommand(permissionHookCmd)
}
