package botregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Catalog markers delimit the machine-generated region inside the
// whats-next bundle's hand-authored iterion-bot-catalog-static.md. The
// editorial reasoning (decision tree, distinguishers, rituals) lives
// outside the markers and is never touched; the persona table + per-bot
// cards are regenerated between them from the live manifests so editing a
// bot's metadata (or toggling it) reaches Nexie without a manual edit.
const (
	catalogGeneratedBegin = "<!-- ITERION:CATALOG:GENERATED:BEGIN -->"
	catalogGeneratedEnd   = "<!-- ITERION:CATALOG:GENERATED:END -->"

	// catalogStaticName is the hand-authored source the generated catalog
	// is spliced into; catalogGeneratedName is the file Nexie actually
	// reads (mirrored into <workspace>/.claude/skills/ at run start).
	catalogStaticName    = "iterion-bot-catalog-static.md"
	catalogGeneratedName = "iterion-bot-catalog.md"
)

// RenderCatalogBlock renders the generated catalog region: a persona ↔
// assignee table followed by one reference card per ENABLED bot. Cards
// derive entirely from each bot's manifest (persona, description,
// triggers, capabilities, when_to_use) plus its workflow-declared vars,
// so the output stays current as bots are edited. Disabled bots are
// omitted. selfName marks the catalog's owning bot ("(this bot)") and
// workdir relativises the printed paths. entries are assumed sorted by
// name (List guarantees this) for deterministic output.
func RenderCatalogBlock(entries []EntryWithSchema, selfName, workdir string) string {
	enabled := make([]EntryWithSchema, 0, len(entries))
	for _, e := range entries {
		if e.Enabled {
			enabled = append(enabled, e)
		}
	}

	var b strings.Builder
	b.WriteString("## The team — persona ↔ assignee\n\n")
	b.WriteString("When you emit an `assignee`, always use the **technical name** (the\n")
	b.WriteString("dispatcher routes on it), never the persona.\n\n")
	b.WriteString("| Persona | `assignee` (technical name) |\n")
	b.WriteString("|---|---|\n")
	for _, e := range enabled {
		persona := strings.TrimSpace(e.DisplayName)
		if persona == "" {
			persona = "—"
		}
		if selfName != "" && NormalizeName(e.Name) == NormalizeName(selfName) {
			fmt.Fprintf(&b, "| %s | `%s` (this bot) |\n", persona, e.Name)
		} else {
			fmt.Fprintf(&b, "| %s | `%s` |\n", persona, e.Name)
		}
	}
	b.WriteString("\n## Bot reference\n")
	for _, e := range enabled {
		b.WriteString("\n")
		renderCatalogCard(&b, e, workdir)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCatalogCard writes one bot's reference card.
func renderCatalogCard(b *strings.Builder, e EntryWithSchema, workdir string) {
	heading := "### `" + e.Name + "`"
	if persona := strings.TrimSpace(e.DisplayName); persona != "" {
		heading += " — " + persona
	}
	b.WriteString(heading + "\n\n")

	if desc := strings.TrimSpace(e.Description); desc != "" {
		b.WriteString(desc + "\n\n")
	}
	if wu := strings.TrimSpace(e.WhenToUse); wu != "" {
		if strings.Contains(wu, "\n") {
			b.WriteString("- **Use when**:\n")
			for _, ln := range strings.Split(wu, "\n") {
				b.WriteString("  " + strings.TrimRight(ln, " ") + "\n")
			}
		} else {
			b.WriteString("- **Use when**: " + wu + "\n")
		}
	}
	if len(e.Triggers) > 0 {
		b.WriteString("- **Triggers**: " + strings.Join(e.Triggers, ", ") + "\n")
	}
	if v := renderCatalogVars(e.Vars); v != "" {
		b.WriteString("- **Vars**: " + v + "\n")
	}
	if len(e.Capabilities) > 0 {
		b.WriteString("- **Capabilities**: " + strings.Join(e.Capabilities, ", ") + "\n")
	}
	b.WriteString("- **Path**: `" + catalogRelPath(e.MainFile(), workdir) + "`\n")
}

// renderCatalogVars formats a bot's declared vars as a one-line summary,
// marking the ones with no default as required (those an orchestrator
// must supply via bot_args).
func renderCatalogVars(vars *VarsBlock) string {
	if vars == nil || len(vars.Fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(vars.Fields))
	for _, f := range vars.Fields {
		seg := "`" + f.Name + "`"
		var meta []string
		if f.Type != "" {
			meta = append(meta, f.Type)
		}
		if f.Default == nil {
			meta = append(meta, "required")
		}
		if len(meta) > 0 {
			seg += " (" + strings.Join(meta, ", ") + ")"
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ", ")
}

// catalogRelPath renders abs relative to workdir (slash form) when it is
// inside the workspace, falling back to the slash-form absolute path.
func catalogRelPath(abs, workdir string) string {
	if workdir == "" {
		return filepath.ToSlash(abs)
	}
	if r, err := filepath.Rel(workdir, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(abs)
}

// RegenerateWhatsNextCatalog refreshes the orchestrator-facing bot
// catalog. It discovers EVERY bundle that ships
// iterion-bot-catalog-static.md (whats-next, issue-triage, any future
// router bot), and for each splices a freshly-rendered persona table
// + per-bot cards (enabled bots only, overlay applied) between that
// file's generated markers, atomically writing the result to the
// sibling skills/iterion-bot-catalog.md — the file the owning bot reads
// (and the runtime mirrors into the workspace skills dir at run start).
//
// Returns the first written path on success, or "" (with nil error) when
// no bundle under workdir ships the static template — e.g. running
// against a packed .botz cache or a workspace that doesn't vendor any
// catalog owner. Writing into the bundle SOURCE (never directly into
// <workspace>/.claude/skills/) lets the runtime's "workspace wins" mirror
// markers refresh the workspace copy cleanly.
func RegenerateWhatsNextCatalog(workdir string) (string, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("botregistry: resolve workdir %s: %w", workdir, err)
	}
	entries, diags, err := ListWithSchemaDiagnostics(ListOptions{Paths: DefaultPaths(abs), Workdir: abs})
	if err != nil {
		return "", err
	}
	// Discovery is now per-entry fault-tolerant, which is right for READ
	// surfaces but wrong here: regenerating from a partial list would
	// rewrite the COMMITTED catalog with the skipped bundle's row and card
	// silently deleted from Nexie's routing table. Refuse to write until
	// the malformed bundle is fixed or removed — the file stays untouched,
	// exactly as when discovery used to fail hard.
	if len(diags) > 0 {
		parts := make([]string, 0, len(diags))
		for _, d := range diags {
			parts = append(parts, d.Path+": "+d.Error)
		}
		return "", fmt.Errorf("botregistry: refusing to regenerate the catalog from partial discovery — fix or remove the malformed bundle(s): %s", strings.Join(parts, "; "))
	}

	// The catalog routes to dispatchable bundles only. Loose .bot demo
	// files (examples/, smoke/) are not catalog bots — exclude them so
	// the generated catalog matches the set the dispatcher can resolve.
	bundles := make([]EntryWithSchema, 0, len(entries))
	for _, e := range entries {
		if e.IsBundleDir {
			bundles = append(bundles, e)
		}
	}

	first := ""
	for i := range bundles {
		owner := bundles[i]
		// The template lives at the BUNDLE ROOT, not skills/, so it is
		// never mirrored into <workspace>/.claude/skills/ as a (duplicate
		// `name: iterion-bot-catalog`) skill — only the generated file is.
		staticPath := filepath.Join(owner.Path, catalogStaticName)
		if _, statErr := os.Stat(staticPath); statErr != nil {
			continue
		}
		static, err := os.ReadFile(staticPath)
		if err != nil {
			return first, fmt.Errorf("botregistry: read catalog template %s: %w", staticPath, err)
		}
		block := RenderCatalogBlock(bundles, owner.Name, abs)
		out, err := spliceGeneratedBlock(string(static), block)
		if err != nil {
			return first, fmt.Errorf("botregistry: %s: %w", staticPath, err)
		}
		skillsDir := filepath.Join(owner.Path, bundle.DirSkills)
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			return first, fmt.Errorf("botregistry: mkdir %s: %w", skillsDir, err)
		}
		dest := filepath.Join(skillsDir, catalogGeneratedName)
		if err := store.WriteFileAtomic(dest, []byte(out), 0o644); err != nil {
			return first, fmt.Errorf("botregistry: write catalog %s: %w", dest, err)
		}
		if first == "" {
			first = dest
		}
	}
	return first, nil
}

// spliceGeneratedBlock replaces the content between the generated markers
// in static with block (markers preserved). Errors when either marker is
// missing or out of order.
func spliceGeneratedBlock(static, block string) (string, error) {
	beginAt := strings.Index(static, catalogGeneratedBegin)
	endAt := strings.Index(static, catalogGeneratedEnd)
	if beginAt < 0 || endAt < 0 || endAt < beginAt {
		return "", fmt.Errorf("catalog markers missing or out of order (%s … %s)", catalogGeneratedBegin, catalogGeneratedEnd)
	}
	head := static[:beginAt+len(catalogGeneratedBegin)]
	tail := static[endAt:]
	return head + "\n\n" + block + "\n\n" + tail, nil
}
