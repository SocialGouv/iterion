package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/spf13/cobra"
)

var openapiOutput string

var openapiInventoryOpts struct {
	root  string
	check bool
}

// openapiInventoryCmd writes the generated REST inventory annex
// (docs/cloud-rest-api-generated.md) next to the curated cloud-rest-api.md:
// every route the recording mux registers, with its outer auth rung,
// cross-checked against the committed openapi.json. CI holds it fresh
// (`task restapi:check`, and the gates of pkg/server's
// docs_rest_inventory_test.go).
var openapiInventoryCmd = &cobra.Command{
	Use:   "inventory",
	Short: "Generate the exhaustive REST inventory annex (offline)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := openapiInventoryOpts.root
		if root == "" {
			root = "."
		}
		body, err := server.GenerateRESTInventoryAnnex(root)
		if err != nil {
			return err
		}
		rel := server.RESTInventoryAnnexPath
		abs := filepath.Join(root, rel)
		current, readErr := os.ReadFile(abs) // #nosec G304 -- the annex's fixed path under the given root
		if openapiInventoryOpts.check {
			if readErr != nil || !bytes.Equal(current, body) {
				return fmt.Errorf("%s is stale — run `task restapi:gen` and commit", rel)
			}
			fmt.Fprintf(os.Stderr, "%s is fresh.\n", rel)
			return nil
		}
		if readErr == nil && bytes.Equal(current, body) {
			fmt.Fprintf(os.Stderr, "%s already up to date.\n", rel)
			return nil
		}
		if err := os.WriteFile(abs, body, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", rel, len(body))
		return nil
	},
}

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
	openapiInventoryCmd.Flags().StringVar(&openapiInventoryOpts.root, "root", "", "Repository root (default: the working directory)")
	openapiInventoryCmd.Flags().BoolVar(&openapiInventoryOpts.check, "check", false, "Fail when the committed annex drifts; write nothing")
	openapiCmd.AddCommand(openapiInventoryCmd)
	rootCmd.AddCommand(openapiCmd)
}
