package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/spf13/cobra"
)

var openapiOutput string

// openapiCmd generates the instance's OpenAPI 3.1 spec OFFLINE (no server, no
// network, no database) from the in-code routing table. It is the single
// source of truth behind the committed openapi.json and the generated client;
// CI regenerates with this command and fails on drift (see `task openapi:check`).
var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Generate the OpenAPI 3.1 spec for this build (offline)",
	Long: "Generate the complete OpenAPI 3.1 document from the in-code route\n" +
		"table, with no running server. Drives the committed openapi.json and\n" +
		"the generated client; CI regenerates and diffs to prevent drift.\n\n" +
		"For the LIVE spec of a remote instance instead, use `iterion remote openapi`.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		doc, err := server.BuildOpenAPISpec()
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if openapiOutput == "" || openapiOutput == "-" {
			_, err = os.Stdout.Write(b)
			return err
		}
		if err := os.WriteFile(openapiOutput, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", openapiOutput, len(b))
		return nil
	},
}

func init() {
	openapiCmd.Flags().StringVarP(&openapiOutput, "output", "o", "", "Write to file instead of stdout")
	rootCmd.AddCommand(openapiCmd)
}
