package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

// DefaultBotsDir is where a new bot bundle lands, relative to the
// workspace root. It matches the path `iterion bots list` walks by
// default, so a freshly created bot is immediately discoverable.
const DefaultBotsDir = "bots"

// BotsCreateOptions configures [BotsCreate]. Empty string fields and nil
// pointers mean "keep the template's value" — only what the operator
// actually passed overrides the gallery template.
type BotsCreateOptions struct {
	// Slug is the bundle directory + technical name (botscaffold.SlugRe).
	Slug string
	// Dest is the parent directory for the bundle (default: bots/).
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
	spec, err := specFromTemplate(opts)
	if err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return err
	}

	dest := opts.Dest
	if dest == "" {
		dest = DefaultBotsDir
	}
	dir := filepath.Join(dest, spec.Slug)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("bots: %s already exists", dir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("bots: cannot stat %s: %w", dir, err)
	}

	res, err := botscaffold.Scaffold(dir, spec)
	if err != nil {
		return err
	}

	// Refresh the generated bot catalog Nexie routes on. A workspace that
	// ships no catalog template returns "" — not an error. A genuine
	// failure is reported but does not undo a valid bundle: the catalog is
	// regenerated automatically at whats-next start and by
	// `iterion bots regen-catalog`.
	catalog, catErr := BotsRegenCatalog(".")

	if p.Format == OutputJSON {
		p.JSON(res)
		return catErr
	}

	p.Header("Bot created")
	p.KV("Directory", res.Dir)
	p.KV("Template", templateID(opts.Template))
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
	p.Line("         $EDITOR %s", filepath.Join(dir, "main.bot"))
	p.Line("    2. Check it compiles:")
	p.Line("         iterion validate %s", filepath.Join(dir, "main.bot"))
	p.Line("    3. Run it:")
	p.Line("         iterion run %s", filepath.Join(dir, "main.bot"))
	p.Blank()

	return catErr
}

// specFromTemplate resolves the gallery template and applies the
// operator's overrides on top.
func specFromTemplate(opts BotsCreateOptions) (botscaffold.Spec, error) {
	id := templateID(opts.Template)
	var spec botscaffold.Spec
	found := false
	for _, t := range botscaffold.Templates() {
		if t.ID == id {
			spec, found = t.Spec, true
			break
		}
	}
	if !found {
		return botscaffold.Spec{}, fmt.Errorf(
			"bots: unknown template %q (available: %s)", id, strings.Join(templateIDs(), ", "))
	}

	spec.Slug = strings.TrimSpace(opts.Slug)
	if opts.DisplayName != "" {
		spec.DisplayName = opts.DisplayName
	}
	if opts.Description != "" {
		spec.Description = opts.Description
	}
	if opts.Instructions != "" {
		spec.Instructions = opts.Instructions
	}
	if opts.Model != "" {
		spec.Model = opts.Model
	}
	if opts.Backend != "" {
		spec.Backend = opts.Backend
	}
	if opts.Worktree != nil {
		spec.Worktree = *opts.Worktree
	}
	if opts.Sandbox != nil {
		spec.Sandbox = *opts.Sandbox
	}
	return spec, nil
}

func templateID(id string) string {
	if strings.TrimSpace(id) == "" {
		return "blank"
	}
	return strings.TrimSpace(id)
}

func templateIDs() []string {
	tpls := botscaffold.Templates()
	ids := make([]string, 0, len(tpls))
	for _, t := range tpls {
		ids = append(ids, t.ID)
	}
	return ids
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
