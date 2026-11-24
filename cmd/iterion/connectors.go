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
would do.

"install" is the same check at install time: it fetches a package from a git
source AT A PINNED COMMIT, validates it with the loader a run uses, and installs
the bytes that passed. Nothing is fetched while a run resolves an operation — a
package decides where a connection's credential goes on the wire, so what serves
is what an operator installed, never what the source serves today.`,
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
		o.ValidateResponses, _ = cmd.Flags().GetBool("validate-responses")
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

var connectorsInstallCmd = &cobra.Command{
	Use:   "install <git url | repository path>",
	Short: "Install a connector package from a git source at a pinned commit",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var o cli.ConnectorsInstallOptions
		o.Source = args[0]
		o.Ref, _ = cmd.Flags().GetString("ref")
		o.Path, _ = cmd.Flags().GetString("path")
		o.Dest, _ = cmd.Flags().GetString("dest")
		o.Force, _ = cmd.Flags().GetBool("force")
		return cli.ConnectorsInstall(cmd.Context(), o, os.Stdout)
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
	// Off by default: it stamps package format v2, which an older iterion
	// refuses to load. The responses a contract cannot represent are listed at
	// the end of the run rather than failing it.
	connectorsGenCmd.Flags().Bool("validate-responses", false, "derive explicit response contracts and validate vendor answers against them (package format v2)")

	// No default: an empty ref is refused rather than resolved to a branch,
	// because the install that follows decides what every run resolves.
	connectorsInstallCmd.Flags().String("ref", "", "the commit to install — a full 40-character sha, not a branch or a tag")
	connectorsInstallCmd.Flags().String("path", "", "package directory inside the repository (default: the repository root)")
	connectorsInstallCmd.Flags().String("dest", "", "catalog root to install into (default <iterion home>/connectors, the tier a run reads)")
	connectorsInstallCmd.Flags().Bool("force", false, "replace a package already installed under that connector id")

	connectorsCmd.AddCommand(connectorsGenCmd, connectorsValidateCmd, connectorsInstallCmd)
	rootCmd.AddCommand(connectorsCmd)
}
