package botregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/internal/mdcode"
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
// assignee table followed by one reference card per ENABLED bot, grouped
// under their declared category (canonical order, Uncategorized last) so
// a router narrows by intent before reading cards. Cards derive entirely
// from each bot's manifest (persona, description, tags, triggers,
// capabilities, when_to_use) plus its workflow-declared vars, so the
// output stays current as bots are edited. Disabled bots are omitted.
// selfName marks the catalog's owning bot ("(this bot)") and workdir
// relativises the printed paths. entries are assumed sorted by name
// (List guarantees this) for deterministic output.
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
	b.WriteString("| Persona | `assignee` (technical name) | Category |\n")
	b.WriteString("|---|---|---|\n")
	// The three cells are a bundle's own text, and a table cell ends at the
	// first pipe it carries: internal/mdcode.Cell escapes it, so a persona
	// writing one keeps its pipe instead of pushing its own tail into the
	// `assignee` column the dispatcher routes on.
	for _, e := range enabled {
		persona := mdcode.Cell(strings.TrimSpace(e.DisplayName))
		if persona == "" {
			persona = "—"
		}
		category := mdcode.Cell(e.Category)
		if category == "" {
			category = "—"
		}
		name := mdcode.Cell(e.Name)
		if selfName != "" && NormalizeName(e.Name) == NormalizeName(selfName) {
			fmt.Fprintf(&b, "| %s | `%s` (this bot) | %s |\n", persona, name, category)
		} else {
			fmt.Fprintf(&b, "| %s | `%s` | %s |\n", persona, name, category)
		}
	}
	b.WriteString("\n## Bot reference\n")
	for _, group := range bundle.GroupByCategory(enabled, func(e EntryWithSchema) string { return e.Category }) {
		if len(group.Bots) == 0 {
			continue // an empty category is noise in a routing document
		}
		fmt.Fprintf(&b, "\n### %s — %s\n", group.Category.Title, group.Category.Tagline)
		for _, e := range group.Bots {
			b.WriteString("\n")
			renderCatalogCard(&b, e, workdir)
		}
	}
	// The block is spliced into a hand-authored document whose tail — the
	// owning bot's own routing ritual — reads as code under any fence the
	// block leaves open. renderCatalogCard keeps every field on the lines it
	// is given, so no value reaches this call today; it is the chokepoint
	// that answers for a field added later, and what it costs a forgotten
	// field is its own broken line instead of the whole page below it.
	return mdcode.EscapeDanglingFence(strings.TrimRight(b.String(), "\n"))
}

// renderCatalogCard writes one bot's reference card.
//
// Description and when_to_use are the two manifest fields spliced as BLOCKS
// of their own, so they go through internal/mdcode.EscapeDanglingFence as
// the lines they will occupy: a field word-wrapped onto a ```suggestion
// line must not swallow the cards below it, and the two spaces of list
// indentation under "Use when" do not stop a fence (CommonMark opens one up
// to three columns in).
//
// Every OTHER manifest value the card embeds goes through
// internal/mdcode.Inline, which is what makes "inside a line a fence
// delimiter cannot start" true of them: a value is free text, and a newline
// in it would end the line it was spliced into and put the rest of itself at
// column 0.
func renderCatalogCard(b *strings.Builder, e EntryWithSchema, workdir string) {
	heading := "### `" + mdcode.Inline(e.Name) + "`"
	if persona := mdcode.Inline(strings.TrimSpace(e.DisplayName)); persona != "" {
		heading += " — " + persona
	}
	b.WriteString(heading + "\n\n")

	if desc := strings.TrimSpace(e.Description); desc != "" {
		b.WriteString(mdcode.EscapeDanglingFence(desc) + "\n\n")
	}
	if wu := strings.TrimSpace(e.WhenToUse); wu != "" {
		// A field carrying ANY of CommonMark's line endings is the multi-line
		// field — internal/mdcode.Lines is what a renderer breaks on, and a
		// value split on line feeds alone would take the single-line road
		// with a line break still in it and land its tail at column 0. The
		// value with no line break at all cannot start a line, which is why
		// the road below splices it as it is.
		if strings.ContainsAny(wu, "\r\n") {
			b.WriteString("- **Use when**:\n")
			var indented strings.Builder
			for _, ln := range mdcode.Lines(wu) {
				// The line is INDENTED, never trimmed: two trailing spaces
				// are a hard line break and trailing spaces inside a fenced
				// example are the example's own bytes, and the fence rule
				// needs neither gone (a closer's trailing spaces are trimmed
				// where it is read, and an info string is read off what
				// follows the marker).
				indented.WriteString("  " + ln + "\n")
			}
			b.WriteString(mdcode.EscapeDanglingFence(indented.String()))
		} else {
			b.WriteString("- **Use when**: " + wu + "\n")
		}
	}
	if len(e.Triggers) > 0 {
		b.WriteString("- **Triggers**: " + mdcode.Inline(strings.Join(e.Triggers, ", ")) + "\n")
	}
	if len(e.Tags) > 0 {
		b.WriteString("- **Tags**: " + mdcode.Inline(strings.Join(e.Tags, ", ")) + "\n")
	}
	if v := renderCatalogVars(e.Vars); v != "" {
		b.WriteString("- **Vars**: " + mdcode.Inline(v) + "\n")
	}
	if len(e.Capabilities) > 0 {
		b.WriteString("- **Capabilities**: " + mdcode.Inline(strings.Join(e.Capabilities, ", ")) + "\n")
	}
	b.WriteString("- **Path**: `" + mdcode.Inline(catalogRelPath(e.MainFile(), workdir)) + "`\n")
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

// catalogTemplateShipped reports whether the bundle rooted at bundleDir
// ships the hand-authored catalog template — the condition that makes the
// generated skills/iterion-bot-catalog.md one of that bundle's own skills,
// and the ONE test every write below is gated on (the per-bundle loop and
// the single-bundle entry point both ask it, so neither can hold its own
// idea of who owns a catalog).
//
// A missing template is "owns none". An empty root and every other stat
// failure are errors, never a bare false: ownership unknown is not
// ownership refused, and writing into a bundle is not a decision to take
// off a verdict the filesystem never gave.
func catalogTemplateShipped(bundleDir string) (bool, error) {
	if strings.TrimSpace(bundleDir) == "" {
		return false, fmt.Errorf("botregistry: catalog ownership needs a bundle root, got an empty path")
	}
	if _, err := os.Stat(filepath.Join(bundleDir, catalogStaticName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("botregistry: stat catalog template under %s: %w", bundleDir, err)
	}
	return true, nil
}

// samePath reports whether two paths name the same directory: the absolute
// cleaned forms first, then their symlink-resolved forms, so a workspace
// and a bundle spelled through different links still match. Paths that
// cannot be resolved compare as given — a mismatch stays a mismatch rather
// than becoming a guess.
func samePath(a, b string) bool {
	absA, err := filepath.Abs(a)
	if err != nil {
		absA = filepath.Clean(a)
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		absB = filepath.Clean(b)
	}
	if absA == absB {
		return true
	}
	resA, errA := filepath.EvalSymlinks(absA)
	resB, errB := filepath.EvalSymlinks(absB)
	return errA == nil && errB == nil && resA == resB
}

// pathUnder reports whether path lies inside root (root itself included).
// The lexical test runs on the cleaned absolute forms, then on the
// symlink-resolved ones: a workspace handed over through a link (the
// run's workspace, a bind-mounted checkout) holds the same bundle under
// another spelling, and reading that as "outside" would skip the owner's
// own refresh in silence.
func pathUnder(path, root string) bool {
	if samePath(path, root) {
		return true
	}
	if lexicallyUnder(path, root) {
		return true
	}
	resPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	return lexicallyUnder(resPath, resRoot)
}

// lexicallyUnder reports whether path is root or sits below it, comparing
// the absolute cleaned forms only.
func lexicallyUnder(path, root string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
//
// An owner whose own template cannot be read is reported — never read as
// "not an owner" — but costs that bundle alone: the other owners are
// refreshed and the failures come back joined, with the first written
// path.
//
// Every owner is the right write set for an operator gesture (the CLI, a
// studio save, the freshness gate): it refreshes the whole checkout. A
// caller acting on behalf of ONE run wants RegenerateCatalogForBundle.
func RegenerateWhatsNextCatalog(workdir string) (string, error) {
	return regenerateCatalogs(workdir, "")
}

// RegenerateCatalogForBundle refreshes ONLY the catalog owned by the
// bundle rooted at bundleDir, rendered from the manifests discovered under
// workdir. It is the write set a caller regenerating on behalf of ONE run
// needs: the workspace-wide set is every owner under the workspace, while
// a run mirrors only its OWN bundle's skills, so a write for any other
// owner leaves a file that run has no copy of — in a git workspace, an
// uncommitted edit to a tracked file every time the running generator
// renders differently from the checkout.
//
// Returns the written path, or "" with a nil error when that bundle has
// nothing of its own to refresh: it ships no template (it owns no
// catalog), or its root lies outside workdir (a materialized .botz, a
// bundle opened from a clone beside the workspace) — the regeneration
// renders from the workspace's manifests and never writes outside it, so
// such a bundle keeps the copy its packer froze.
//
// A bundle UNDER workdir that ships the template but is not among the
// bundles discovered there is an error: discovery owns what a bundle IS
// (a directory with manifest.yaml and main.bot), and refreshing the other
// owners instead is exactly the collateral write this entry point exists
// to prevent.
func RegenerateCatalogForBundle(workdir, bundleDir string) (string, error) {
	shipped, err := catalogTemplateShipped(bundleDir)
	if err != nil {
		return "", err
	}
	if !shipped {
		return "", nil
	}
	if !pathUnder(bundleDir, workdir) {
		return "", nil
	}
	return regenerateCatalogs(workdir, bundleDir)
}

// regenerateCatalogs renders the generated region from the manifests
// discovered under workdir into every discovered owner's skills dir — or,
// when onlyBundleDir is non-empty, into that bundle alone. The rendered
// CONTENT is the full discovered set either way: narrowing the write set
// never narrows the routing table it carries.
//
// Per-owner failures are collected and returned joined, the healthy
// owners refreshed regardless of where the broken one sorts. A partial
// DISCOVERY is the opposite case and still refuses everything before the
// first write: it would silently delete the skipped bundle's row from
// every catalog it rewrote.
func regenerateCatalogs(workdir, onlyBundleDir string) (string, error) {
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
	//
	// Only diagnostics that could change the catalog's CONTENT block: a
	// skipped bundle dir, or an unreadable walk path whose contents are
	// unknown (conservative). A skipped LOOSE .bot file never contributes
	// a card (they are excluded below), so a broken demo under examples/
	// must not freeze the catalog. The kind recorded at skip time decides
	// — re-stat'ing the path here would silently downgrade to
	// non-blocking whenever the stat itself fails.
	var blocking []DiscoveryError
	for _, d := range diags {
		if d.Kind != DiscoveryErrorFile {
			blocking = append(blocking, d)
		}
	}
	if len(blocking) > 0 {
		parts := make([]string, 0, len(blocking))
		for _, d := range blocking {
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
	var failures []error
	for i := range bundles {
		owner := bundles[i]
		if onlyBundleDir != "" && !samePath(owner.Path, onlyBundleDir) {
			continue
		}
		dest, err := refreshCatalogOwner(bundles, owner, abs)
		if err != nil {
			// One bundle's filesystem problem is charged to that
			// bundle alone: the owners hold independent files, and
			// cancelling the rest would make the write set the walk
			// order instead of the rule, leaving healthy owners stale
			// depending on where the broken one happens to sort.
			failures = append(failures, err)
			continue
		}
		if dest != "" && first == "" {
			first = dest
		}
	}
	if len(failures) > 0 {
		return first, errors.Join(failures...)
	}
	if onlyBundleDir != "" && first == "" {
		return "", fmt.Errorf("botregistry: the bundle at %s ships %s but is none of the bundles discovered under %s (a layout discovery does not accept — %s plus %s at the root — or a name another discovery root claimed first), so nothing was regenerated for it", onlyBundleDir, catalogStaticName, abs, bundle.ManifestFile, bundle.MainBotFile)
	}
	return first, nil
}

// refreshCatalogOwner splices the generated region rendered from bundles
// into the catalog owned by one of them, and returns where it wrote.
// ("", nil) means that bundle ships no template, i.e. it owns no catalog
// and there was nothing to write. Every error names the bundle it
// belongs to, so the caller can report it without cancelling the others.
func refreshCatalogOwner(bundles []EntryWithSchema, owner EntryWithSchema, workdir string) (string, error) {
	// The template lives at the BUNDLE ROOT, not skills/, so it is
	// never mirrored into <workspace>/.claude/skills/ as a (duplicate
	// `name: iterion-bot-catalog`) skill — only the generated file is.
	staticPath := filepath.Join(owner.Path, catalogStaticName)
	switch shipped, err := catalogTemplateShipped(owner.Path); {
	case err != nil:
		return "", err
	case !shipped:
		return "", nil
	}
	static, err := os.ReadFile(staticPath)
	if err != nil {
		return "", fmt.Errorf("botregistry: read catalog template %s: %w", staticPath, err)
	}
	block := RenderCatalogBlock(bundles, owner.Name, workdir)
	out, err := spliceGeneratedBlock(string(static), block)
	if err != nil {
		return "", fmt.Errorf("botregistry: %s: %w", staticPath, err)
	}
	skillsDir := filepath.Join(owner.Path, bundle.DirSkills)
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return "", fmt.Errorf("botregistry: mkdir %s: %w", skillsDir, err)
	}
	dest := filepath.Join(skillsDir, catalogGeneratedName)
	out = reanchorForDepth(out, filepath.Dir(staticPath), skillsDir)
	if err := store.WriteFileAtomic(dest, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("botregistry: write catalog %s: %w", dest, err)
	}
	return dest, nil
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

// relTargetRe matches the destination of an inline markdown link:
// `](target)`. A destination holds no newline, and bounding the match to one
// line is what keeps a stray `](` — a form the catalogue documents — from
// reaching down the page for the next parenthesis and replacing every line
// between with a path.
var relTargetRe = regexp.MustCompile(`\]\(([^)\n]*)\)`)

// externalTargetRe matches a target that leaves the repository: a URL with
// a scheme, or a protocol-relative one.
var externalTargetRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*:|//)`)

// reanchorForDepth rewrites the relative link targets of text spliced from
// fromDir into toDir, so each keeps naming the file it named where it was
// written. The catalog template lives at the bundle root and becomes the
// skill copy one directory deeper; its static text is spliced verbatim,
// and the depth the copy descends is depth the targets must climb.
// Scheme, `/`-prefixed and bare `#fragment` targets pass through — a
// fragment keeps naming the copy's own headings, which the copy carries
// whole (unlike the docs map, which quotes ANOTHER page's anchors and
// must gain that page's path).
//
// Which bytes are code is internal/mdcode's rule: a `](target)` the
// catalogue QUOTES — inside a fenced block or an inline span — is a form
// the document shows, not a link it makes, and re-anchoring it corrupts the
// example the page is teaching. The grammar runs over the mask, whose
// offsets are the content's own, and the bytes are read back out of the
// content.
func reanchorForDepth(content, fromDir, toDir string) string {
	if fromDir == toDir {
		return content
	}
	rewrite := func(m string) string {
		target := m[2 : len(m)-1]
		if target == "" || strings.HasPrefix(target, "#") ||
			externalTargetRe.MatchString(target) || strings.HasPrefix(target, "/") {
			return m
		}
		frag := ""
		if i := strings.Index(target, "#"); i >= 0 {
			frag, target = target[i:], target[:i]
		}
		rel, err := filepath.Rel(toDir, filepath.Join(fromDir, filepath.FromSlash(target)))
		if err != nil {
			return m
		}
		return "](" + filepath.ToSlash(rel) + frag + ")"
	}
	return mdcode.ReplaceOutsideCode(content, relTargetRe, rewrite)
}
