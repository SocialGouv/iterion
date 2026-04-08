package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/internal/appinfo"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(appinfo.FullVersion())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
