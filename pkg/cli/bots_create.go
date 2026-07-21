package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

// BotsCreateOptions configures [BotsCreate]. Empty string fields and nil
// pointers mean "keep the template's value" — only what the operator
// actually passed overrides the gallery template.
type BotsCreateOptions struct {
	// Slug is the bundle directory + technical name (botscaffold.SlugRe).
	Slug string
	// Workdir is the workspace root (default: current directory). It
	// anchors both Dest and the catalog refresh, mirroring
	// botinstall.Options.
	Workdir string
	// Dest is the parent directory for the bundle, relative to Workdir
	// unless absolute (default: bots/).
	Dest string
	// Template is a gallery template ID (default: blank).
	Template string

	DisplayName  string
	Description  string
	Instructions string
	Model        string
	Backend      string
	Worktree     *bool
	Sandbox      *bool
}

// BotsCreate scaffolds a new bot bundle at <Dest>/<Slug> from a gallery
// template, then refreshes the orchestrator-facing catalog so the new bot
// is routable. It is the CLI half of the studio's "New bot" flow — both
// call the same botscaffold engine, so a bot created either way is
// byte-identical.
func BotsCreate(opts BotsCreateOptions, p *Printer) error {
	workdir := opts.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("bots: resolve workdir: %w", err)
		}
		workdir = wd
	}

	template := opts.Template
	if template == "" {
		template = botscaffold.DefaultTemplateID
	}
	spec, err := botscaffold.SpecFromTemplate(template, botscaffold.Overrides{
		Slug:         opts.Slug,
		DisplayName:  opts.DisplayName,
		Description:  opts.Description,
		Instructions: opts.Instructions,
		Model:        opts.Model,
		Backend:      opts.Backend,
		Worktree:     opts.Worktree,
		Sandbox:      opts.Sandbox,
	})
	if err != nil {
		return UserInputError(err)
	}
	// Validate normalizes too, so spec.Slug is trimmed from here on.
	if err := spec.Validate(); err != nil {
		return UserInputError(err)
	}

	dest := opts.Dest
	if dest == "" {
		dest = botregistry.BotsDirName
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(workdir, dest)
	}
	// Name collision anywhere discovery looks — not just under Dest —
	// because a duplicate name makes catalog routing ambiguous. Same
	// precondition the studio's create endpoint enforces.
	if err := botregistry.EnsureNameFree(
		botregistry.ListOptions{Paths: botregistry.DefaultPaths(workdir), Workdir: workdir},
		spec.Slug,
	); err != nil {
		if errors.Is(err, botregistry.ErrNameTaken) {
			return UserInputError(err)
		}
		return err
	}

	dir := filepath.Join(dest, spec.Slug)
	if _, err := os.Stat(dir); err == nil {
		return UserInputError(fmt.Errorf("bots: %s already exists", dir))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("bots: cannot stat %s: %w", dir, err)
	}

	res, err := botscaffold.Scaffold(dir, spec)
	if err != nil {
		return err
	}

	// Refresh the generated bot catalog Nexie routes on. Best-effort, and
	// deliberately non-fatal — matching the studio's create endpoint: the
	// bundle on disk is valid either way, and the catalog is regenerated
	// at whats-next start and by `iterion bots regen-catalog`. Failing the
	// command here would contradict the "Bot created" we just printed.
	catalog, catErr := BotsRegenCatalog(workdir)

	if p.Format == OutputJSON {
		p.JSON(res)
		return nil
	}

	mainBot := filepath.Join(dir, "main.bot")
	p.Header("Bot created")
	p.KV("Directory", res.Dir)
	p.KV("Template", template)
	p.Blank()
	for _, f := range res.Files {
		p.Line("  + %s", f)
	}
	p.Blank()
	if catalog != "" {
		p.Line("  Catalog refreshed: %s", catalog)
		p.Blank()
	}
	if catErr != nil {
		p.Line("  ! catalog not refreshed: %v", catErr)
		p.Line("    (the bundle is valid — rerun `iterion bots regen-catalog`)")
		p.Blank()
	}
	p.Line("  Next steps:")
	p.Line("    1. Write the mission (the `mission` prompt block):")
	p.Line("         $EDITOR %s", mainBot)
	p.Line("    2. Run it (it is validated first):")
	p.Line("         iterion run %s", mainBot)
	p.Blank()

	return nil
}

// BotsTemplates renders the bot-creation gallery — the same catalog the
// studio builder shows at /bots/new.
func BotsTemplates(p *Printer) error {
	tpls := botscaffold.Templates()
	if p.Format == OutputJSON {
		p.JSON(map[string]any{"templates": tpls})
		return nil
	}
	rows := make([][]string, 0, len(tpls))
	for _, t := range tpls {
		rows = append(rows, []string{t.ID, t.Name, t.Description})
	}
	p.Header("Bot templates")
	p.Table([]string{"ID", "NAME", "DESCRIPTION"}, rows)
	p.Blank()
	p.Line("  iterion bots create <slug> --template <ID>")
	p.Blank()
	return nil
}
