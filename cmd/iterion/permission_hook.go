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
		policyPath, _ := cmd.Flags().GetString("policy")
		if backend == "" || policyPath == "" {
			return fmt.Errorf("__permission-hook: --backend and --policy are required")
		}
		return permissionhook.Run(backend, policyPath, os.Stdin, os.Stdout)
	},
}

func init() {
	permissionHookCmd.Flags().String("backend", "", "native CLI hook dialect")
	permissionHookCmd.Flags().String("policy", "", "serialised permission policy")
	permissionHookCmd.Flags().Bool("probe", false, "verify that this binary supports permission hooks")
	rootCmd.AddCommand(permissionHookCmd)
}
