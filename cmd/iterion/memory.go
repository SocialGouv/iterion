package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
)

var memoryOpts struct {
	visibility string
	name       string
	project    string
	user       string
	tenant     string
	bot        string
	out        string
	in         string
	strategy   string
}

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Manage local shared-knowledge memory spaces (export/import/du)",
	Long: `Operate on the local filesystem memory store
(~/.iterion/...). A space is addressed by --visibility (bot|project|
cross_project|user|org|global, default bot) + --name, plus --project
(defaults to the current directory) for bot/project spaces and
--user/--tenant for the shared trees.`,
}

var memoryExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export a memory space to a .tar.gz archive (stdout or --out)",
	Args:  cobra.NoArgs,
	RunE:  runMemoryExport,
}

var memoryImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import a memory archive into a space (--in, --strategy)",
	Args:  cobra.NoArgs,
	RunE:  runMemoryImport,
}

var memoryDuCmd = &cobra.Command{
	Use:   "du",
	Short: "Show a memory space's usage and quota",
	Args:  cobra.NoArgs,
	RunE:  runMemoryDu,
}

func init() {
	for _, c := range []*cobra.Command{memoryExportCmd, memoryImportCmd, memoryDuCmd} {
		f := c.Flags()
		f.StringVar(&memoryOpts.visibility, "visibility", "bot", "space visibility (bot|project|cross_project|user|org|global)")
		f.StringVar(&memoryOpts.name, "name", "", "space name (required)")
		f.StringVar(&memoryOpts.project, "project", "", "project dir (bot/project spaces; default cwd)")
		f.StringVar(&memoryOpts.bot, "bot", "", "bot id (required for visibility=bot)")
		f.StringVar(&memoryOpts.user, "user", "", "user id (user spaces)")
		f.StringVar(&memoryOpts.tenant, "tenant", "", "tenant id (org/cross_project spaces)")
	}
	memoryExportCmd.Flags().StringVar(&memoryOpts.out, "out", "", "output file (default stdout)")
	memoryImportCmd.Flags().StringVar(&memoryOpts.in, "in", "", "input archive (default stdin)")
	memoryImportCmd.Flags().StringVar(&memoryOpts.strategy, "strategy", "skip", "on conflict: skip|overwrite|rename")
	memoryCmd.AddCommand(memoryExportCmd, memoryImportCmd, memoryDuCmd)
	rootCmd.AddCommand(memoryCmd)
}

func memoryRefFromFlags() (knowledge.SpaceRef, error) {
	if memoryOpts.name == "" {
		return knowledge.SpaceRef{}, fmt.Errorf("--name is required")
	}
	project := memoryOpts.project
	if project == "" {
		project, _ = os.Getwd()
	}
	vis := knowledge.Visibility(memoryOpts.visibility)
	if vis == knowledge.VisibilityBot && memoryOpts.bot == "" {
		return knowledge.SpaceRef{}, fmt.Errorf("--bot is required when --visibility=bot")
	}
	ref := memory.ResolveSpaceRef(
		vis,
		memoryOpts.name, memoryOpts.bot, memoryOpts.user,
		memory.SpaceRefInputs{TenantID: memoryOpts.tenant, UserID: memoryOpts.user, ProjectID: memory.ProjectKey(project), BotID: memoryOpts.bot},
	)
	if err := ref.Validate(); err != nil {
		return knowledge.SpaceRef{}, err
	}
	return ref, nil
}

func runMemoryExport(cmd *cobra.Command, _ []string) error {
	ref, err := memoryRefFromFlags()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if memoryOpts.out != "" {
		f, err := os.Create(memoryOpts.out)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	m, err := knowledge.ExportSpace(cmd.Context(), memory.DefaultFSStore(), ref, out)
	if err != nil {
		return err
	}
	if memoryOpts.out != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "exported %d document(s) to %s\n", m.DocCount, memoryOpts.out)
	}
	return nil
}

func runMemoryImport(cmd *cobra.Command, _ []string) error {
	ref, err := memoryRefFromFlags()
	if err != nil {
		return err
	}
	in := cmd.InOrStdin()
	if memoryOpts.in != "" {
		f, err := os.Open(memoryOpts.in)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	// The operator at their own console, on their own filesystem store, holds
	// the platform write authority the `global` space requires: this command
	// IS the super-admin surface locally. The grant is explicit (and
	// greppable) rather than implied by the adapter, so the engine's
	// memory_write tool — which proves nothing — stays refused on the same
	// store.
	ctx := knowledge.WithPlatformWriteAuthority(cmd.Context())
	sum, err := knowledge.ImportSpace(ctx, memory.DefaultFSStore(), ref, in, knowledge.ImportStrategy(memoryOpts.strategy))
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "imported=%d skipped=%d renamed=%d\n", sum.Imported, sum.Skipped, sum.Renamed)
	return nil
}

func runMemoryDu(cmd *cobra.Command, _ []string) error {
	ref, err := memoryRefFromFlags()
	if err != nil {
		return err
	}
	used, quota, err := memory.DefaultFSStore().UsageBytes(cmd.Context(), ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "space %s: used=%d quota=%d bytes\n", ref.ID(), used, quota)
	return nil
}
