package main

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var connectorsCmd = &cobra.Command{
	Use:   "connectors",
	Short: "Generate and validate connector packages",
	Long: `Work with connector packages — the catalog's unit of third-party access.

A package has two halves. Everything under ops/ is GENERATED from a vendor's
API description and is disposable: regenerate and get the same bytes. The
overlay.yaml beside it is AUTHORED — the auth a description states in prose,
how a collection paginates, which operations an agent should see, the ids
pinned so a vendor's next release cannot move them.

"gen" produces the generated half. It reads OpenAPI 3.x and Swagger 2.0 alike,
and it is also the install-time lane: a description iterion may not
redistribute can still be generated from locally by whoever holds the right to
do so, which is why --license and --redistributable are explicit inputs
recorded in the package's provenance rather than inferred from the document.

"validate" applies the overlay and runs the complete check — what a launch
would do.`,
}

var connectorsGenCmd = &cobra.Command{
	Use:   "gen",
	Short: "Generate a connector package from a vendor API description",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var o cli.ConnectorsGenOptions
		o.Spec, _ = cmd.Flags().GetString("spec")
		o.ID, _ = cmd.Flags().GetString("id")
		o.Out, _ = cmd.Flags().GetString("out")
		o.Version, _ = cmd.Flags().GetString("package-version")
		o.License, _ = cmd.Flags().GetString("license")
		o.Redistributable, _ = cmd.Flags().GetBool("redistributable")
		o.OperatorSuppliedBaseURL, _ = cmd.Flags().GetBool("self-hosted")
		o.KeepOverlay, _ = cmd.Flags().GetBool("keep-overlay")
		return cli.ConnectorsGen(o, os.Stdout)
	},
}

var connectorsValidateCmd = &cobra.Command{
	Use:   "validate <package-dir>",
	Short: "Apply a package's overlay and run the complete check",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return cli.ConnectorsValidate(args[0], os.Stdout)
	},
}

func init() {
	connectorsGenCmd.Flags().String("spec", "", "path or https URL to the vendor's OpenAPI 3.x / Swagger 2.0 description")
	connectorsGenCmd.Flags().String("id", "", "connector slug — the package name and the first segment of every operation id")
	connectorsGenCmd.Flags().String("out", "", "package directory to write (default connectors/<id>)")
	connectorsGenCmd.Flags().String("package-version", "0.1.0", "package semver to stamp")
	connectorsGenCmd.Flags().String("license", "", "the licence the DESCRIPTION carries — recorded verbatim in the provenance")
	// Defaults to false on purpose: a package generated from an unexamined
	// description must not enter a redistributable catalog just because
	// nobody said otherwise.
	connectorsGenCmd.Flags().Bool("redistributable", false, "assert that the generated operations may ship in iterion's own catalog")
	connectorsGenCmd.Flags().Bool("self-hosted", false, "the product is commonly self-hosted, so a connection supplies its own instance URL")
	connectorsGenCmd.Flags().Bool("keep-overlay", true, "re-apply the package's existing overlay.yaml, failing if it no longer matches")

	connectorsCmd.AddCommand(connectorsGenCmd, connectorsValidateCmd)
	rootCmd.AddCommand(connectorsCmd)
}
