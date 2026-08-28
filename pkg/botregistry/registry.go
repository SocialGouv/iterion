// Package botregistry discovers bots on disk: single .bot files and
// .botz bundle directories. It is the shared layer used by the
// `iterion bots list` CLI command, the studio HTTP server
// (GET /api/v1/bots), and the dispatcher when resolving a per-ticket
// bot override to a workflow file path.
//
// Discovery is purely metadata (name, description, triggers,
// capabilities). The companion file schema.go layers on the workflow's
// declared vars + presets so the studio can render a typed form per
// bot.
package botregistry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Entry is one bot discovered by List. Path is the file (single .bot)
// or directory (bundle) that produced the entry — operators can grep
// back to it.
type Entry struct {
	Name string `json:"name" yaml:"name"`
	// DisplayName is the bundle's friendly persona (manifest.yaml
	// display_name) — e.g. "Nexie" for whats-next, "Featurly" for
	// feature_dev. Empty for loose .bot files and bundles that declare
	// no persona. Surfaced by `iterion bots list` and the studio bot
	// picker so operators recognise the team by name, not just by id.
	DisplayName string `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	// Icon is the bot's emoji identity from the manifest (manifest.yaml
	// icon:). Empty for loose .bot files and bundles without one — the
	// studio then falls back to its persona/hash identity.
	Icon         string   `json:"icon,omitempty" yaml:"icon,omitempty"`
	Description  string   `json:"description" yaml:"description,omitempty"`
	Path         string   `json:"path" yaml:"path"`
	Triggers     []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`

	// DispatchVars is the bot's default dispatch var template (manifest
	// dispatch_vars) — how the dispatcher maps the issue into THIS bot's
	// inputs (e.g. {"feature_prompt": "{{issue.title}}\n\n{{issue.body}}"}
	// for feature-dev, {"scope_notes": "…"} for a reviewer). The
	// dispatcher renders these per issue (per-ticket bot_args merge on
	// top). Empty = the bot reads only the global dispatch vars
	// (issue_title/body/id). This lives in the manifest so adding/renaming
	// a bot needs ZERO dispatcher-code edits — discovery carries it.
	DispatchVars map[string]string `json:"dispatch_vars,omitempty" yaml:"dispatch_vars,omitempty"`

	// Forge mirrors the manifest forge: block (forge-access
	// requirements). Nil when the bot declares no forge ambitions; the
	// studio Integrations "enable on this repo" picker filters those out
	// and renders this verbatim ("Revi will subscribe to … and post
	// as …") for the rest. Carried by discovery so the studio knows what
	// a bot will provision before any run exists.
	Forge *bundle.ForgeRequirements `json:"forge,omitempty" yaml:"forge,omitempty"`

	// Repo mirrors the manifest repo: block (the bot's runtime
	// repository need). The Launch surfaces render it as the "Target
	// repository" section — required soft-blocks, optional offers,
	// allow_create adds "create a new repository" on a connected forge.
	Repo *bundle.RepoRequirement `json:"repo,omitempty" yaml:"repo,omitempty"`

	// ConfigShare mirrors the manifest config_share: block (the bot's
	// scoped config-share surface). The studio's "Share config" card reads
	// it to drive a data-driven mint form (the editable fields + config
	// file come from the bot, not hardcoded per bot); nil = the bot exposes
	// no config-share surface and the card is not offered.
	ConfigShare *bundle.ConfigShareSpec `json:"config_share,omitempty" yaml:"config_share,omitempty"`

	// Launch mirrors the manifest launch: block (launch-form hints): which
	// vars the studio launch form surfaces as primary (in order) and which
	// it hides (still settable via --var). Nil when the bot declares no
	// opinion — the form renders every var as before.
	Launch *bundle.LaunchHints `json:"launch,omitempty" yaml:"launch,omitempty"`

	// Chat mirrors the manifest chat: block — the bot's declaration that it
	// is a conversational bot the studio hosts in its assistant dock. Nil for
	// every ordinary bot. Carried by discovery because the studio's chat
	// registry IS this list: without it the surface falls back to a
	// hard-coded const, and a second chat bot needs a studio release.
	Chat *bundle.ChatSurface `json:"chat,omitempty" yaml:"chat,omitempty"`

	// Produces / Consumes mirror the manifest's run-to-run hand-off blocks:
	// what this bot leaves behind for a later run, and what it wants handed to
	// it at launch. Carried by discovery because the hand-off is resolved AT
	// LAUNCH, when the only thing known is the bot's name — matching a consumer
	// to a producer by KIND is what keeps the engine from naming either bot.
	Produces []bundle.ProducedArtifact `json:"produces,omitempty" yaml:"produces,omitempty"`
	Consumes []bundle.ConsumedArtifact `json:"consumes,omitempty" yaml:"consumes,omitempty"`

	// Invocations is the typed routing contract from the manifest
	// (manifest.yaml invocations:) — how this bot can be triggered (forge
	// event, /slash-command, schedule, board) and the execution mode each
	// path uses. Falls back to bundle.SyntheticInvocations for a legacy
	// forge:-only bundle. Carried by discovery so the studio Integrations
	// picker can group bots by trigger and the command router / orchestrator
	// can resolve a command to a bot. Empty for loose .bot files and bots
	// that declare no invocations (orchestrators like Nexie/Evoly).
	Invocations []bundle.Invocation `json:"invocations,omitempty" yaml:"invocations,omitempty"`

	// WhenToUse is the orchestrator-facing "use when" guidance from the
	// bundle manifest (manifest.yaml when_to_use). Empty for loose .bot
	// files. Surfaced in the generated iterion-bot-catalog "Use when"
	// card and editable via the studio Bot-metadata panel.
	WhenToUse string `json:"when_to_use,omitempty" yaml:"when_to_use,omitempty"`

	// Author and Version mirror the manifest fields so the studio Bot
	// metadata panel can pre-fill + edit them. Empty for loose .bot files.
	Author  string `json:"author,omitempty" yaml:"author,omitempty"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// Enabled is the RESOLVED catalog-visibility decision: the manifest
	// `enabled` default composed with the workspace overlay
	// (.iterion/bot-overrides.yaml), the overlay winning. Always
	// serialised so the studio toggle has a deterministic initial state.
	// List/ListWithSchema return disabled bots too (Enabled=false) so the
	// studio can show them to flip back on; only catalog generation and
	// auto-dispatch filter on it.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// ManifestEnabled is the bot's `enabled` DEFAULT from its manifest,
	// BEFORE the workspace overlay is applied. The studio Bot panel edits
	// this (writes the manifest), while the Catalog manager toggles the
	// overlay (Enabled). The panel compares the two to surface a "locally
	// overridden" note. Not overwritten by List.
	ManifestEnabled bool `json:"manifest_enabled" yaml:"manifest_enabled"`

	// IsBundleDir reports whether this entry is a bundle directory
	// (manifest.yaml + main.bot) rather than a loose .bot file.
	// The studio uses it to gate manifest editing — only bundles have a
	// manifest.yaml to write.
	IsBundleDir bool `json:"is_bundle,omitempty" yaml:"is_bundle,omitempty"`

	// RelPath is Path made workspace-relative (slash form), set by List
	// only when ListOptions.Workdir is given and Path is inside it. The
	// studio uses it to open a bot's main.bot without reconstructing the
	// relative path from the absolute one. Empty when no workdir is known
	// or the bot lives outside it.
	RelPath string `json:"rel_path,omitempty" yaml:"rel_path,omitempty"`
}

// IsBundle reports whether the entry came from a .botz bundle (Path
// points at a directory containing manifest.yaml + main.bot) rather
// than a single .bot file.
func (e Entry) IsBundle() bool {
	info, err := os.Stat(e.Path)
	return err == nil && info.IsDir()
}

// MainFile returns the workflow source file the entry points at. For
// a bundle this is <Path>/main.bot; for a loose file it is Path itself.
// Used by the dispatcher to resolve a per-ticket bot override to a
// concrete workflow path.
func (e Entry) MainFile() string {
	if e.IsBundle() {
		return filepath.Join(e.Path, "main.bot")
	}
	return e.Path
}

// ListOptions configures discovery for List.
type ListOptions struct {
	// Paths are the roots to walk. Each may be a single .bot file
	// (treated as one entry), a .botz bundle directory, or a directory
	// containing many .bot files / sub-bundles. Missing paths are
	// skipped silently — the caller's defaults often include
	// optimistic locations like "./examples" or "./bots".
	Paths []string

	// Workdir, when set, is the workspace root whose
	// .iterion/bot-overrides.yaml overlay is composed over each bot's
	// manifest `enabled` default to produce the resolved Entry.Enabled.
	// Empty disables overlay resolution (entries keep the manifest
	// default). Typically the same dir passed to DefaultPaths.
	Workdir string
}

// Config carries the discovery roots for the bot registry. Lives here
// so both pkg/server (studio HTTP endpoint) and pkg/dispatcher
// (per-ticket bot override resolution) can declare the same field
// without one importing the other.
type Config struct {
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// ErrNameTaken reports that a bot of the requested name is already
// discoverable. Callers that need a distinct status for it (the studio's
// 409, the CLI's exit 2) match with errors.Is.
var ErrNameTaken = errors.New("bot name already in use")

// FindByName returns the discovered bot named name, if any.
func FindByName(opts ListOptions, name string) (Entry, bool, error) {
	entries, err := List(opts)
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// EnsureNameFree is the shared precondition for creating a bot: it fails
// when name is already taken anywhere discovery looks, not merely where
// the new bundle would be written.
//
// Both creation surfaces call it so they agree. Checking only the target
// directory is not enough — a bot of the same name living in `.botz/` or
// any other configured root would still collide in the catalog, and a
// duplicate name there is what makes `iterion run <name>` and dispatcher
// routing ambiguous.
func EnsureNameFree(opts ListOptions, name string) error {
	// ONE discovery walk: the entries and the diagnostics must come from
	// the same filesystem snapshot, or a bundle repaired between two
	// walks is neither found nor reported.
	entries, diags, err := ListWithDiagnostics(opts)
	if err != nil {
		return err
	}
	// Normalize like ResolveBotPath and the cross-root dedupe do: a bot
	// created as "my_bot" while "my-bot" exists would be permanently
	// shadowed by the dedupe and unreachable by its own name.
	nn := NormalizeName(name)
	for _, existing := range entries {
		if NormalizeName(existing.Name) == nn {
			return fmt.Errorf("%w: %q is already defined at %s", ErrNameTaken, name, existing.Path)
		}
	}
	// A malformed bundle produces no entry, but its directory still HOLDS
	// the name: creating a second bot under it would shadow the first the
	// day its manifest is fixed (discovery's dedupe keeps the first
	// occurrence). Declare the name taken, with the cause attached.
	if d := diagForName(diags, name); d != nil {
		return fmt.Errorf("%w: %q matches a bundle that failed to load at %s (%s) — fix or remove it first", ErrNameTaken, name, d.Path, d.Error)
	}
	return nil
}

// diagForName matches a requested bot name against the discovery
// diagnostics. Only bot-source diagnostics (a bundle dir or a loose
// file that failed to load) are considered — an unreadable plain
// directory never claimed a bot name. The only handle a malformed
// source offers is its directory / file base (normalized the way
// ResolveBotPath resolves); the extension trim applies to files only,
// so a bundle dir with a dot in its name keeps it.
func diagForName(diags []DiscoveryError, name string) *DiscoveryError {
	nn := NormalizeName(name)
	for i := range diags {
		d := &diags[i]
		if d.Kind != DiscoveryErrorBundle && d.Kind != DiscoveryErrorFile {
			continue
		}
		base := filepath.Base(d.Path)
		if d.Kind == DiscoveryErrorFile {
			base = strings.TrimSuffix(base, filepath.Ext(base))
		}
		if NormalizeName(base) == nn {
			return d
		}
	}
	return nil
}

// List walks Opts.Paths and returns the discovered bots sorted by
// name. A missing path is treated as empty (not an error) so callers
// can pass optimistic defaults. A malformed bundle or unreadable bot
// file is SKIPPED rather than failing the list — one bad source must
// not blank its valid siblings; the diagnostic rides the structured
// channel ListWithDiagnostics returns.
func List(opts ListOptions) ([]Entry, error) {
	entries, _, err := ListWithDiagnostics(opts)
	return entries, err
}

// DiscoveryErrorKind identifies what kind of source a DiscoveryError is
// about — recorded at skip time so consumers (name resolution, the
// catalog-regen guard) don't have to re-stat the path to guess.
type DiscoveryErrorKind string

const (
	// DiscoveryErrorBundle is a bundle directory whose manifest failed to
	// load (parse or validation error).
	DiscoveryErrorBundle DiscoveryErrorKind = "bundle"
	// DiscoveryErrorFile is a loose .bot file that failed to read.
	DiscoveryErrorFile DiscoveryErrorKind = "file"
	// DiscoveryErrorWalk is a path the filesystem walk itself could not
	// read (e.g. a permission-denied directory). It may or may not hold a
	// bot source — the contents are unknown.
	DiscoveryErrorWalk DiscoveryErrorKind = "walk"
)

// DiscoveryError is one skipped bot source: the bundle directory or bot
// file at Path failed to load, so it produced no Entry. Discovery is
// per-entry fault-tolerant — one malformed manifest (e.g. an invalid
// chat: block, which validateChatSurface fails on purpose) must not
// blank discovery for every otherwise-valid bot in the workspace — and
// the failure rides this structured channel to the CLI/API/Studio
// instead of aborting the walk.
type DiscoveryError struct {
	// Path is the bundle directory or bot file that failed to load.
	Path string `json:"path" yaml:"path"`
	// Error is the load diagnostic (parse or validation failure).
	Error string `json:"error" yaml:"error"`
	// Kind is what the skipped source is known to be. Only "bundle" and
	// "file" name an actual bot source; "walk" is an unreadable path
	// whose contents are unknown.
	Kind DiscoveryErrorKind `json:"kind" yaml:"kind"`
}

// ListWithDiagnostics is List plus the per-entry discovery errors: one
// DiscoveryError per skipped bundle directory or bot file. The returned
// error stays reserved for FATAL failures (an unstat-able root, a
// broken workspace overlay) — a partial one never fails the list.
func ListWithDiagnostics(opts ListOptions) ([]Entry, []DiscoveryError, error) {
	entries, diags, err := discoverBots(opts.Paths)
	if err != nil {
		return nil, nil, err
	}
	// Compose the workspace overlay over each bot's manifest `enabled`
	// default so Entry.Enabled is the resolved catalog-visibility
	// decision, and fill RelPath. Disabled bots are still returned (the
	// studio shows them to flip back on); only catalog generation +
	// auto-dispatch filter.
	if opts.Workdir != "" {
		ov, err := LoadOverrides(opts.Workdir)
		if err != nil {
			return nil, nil, err
		}
		for i := range entries {
			entries[i].Enabled = ResolveEnabled(entries[i].Name, entries[i].Enabled, ov)
			if rel, relErr := filepath.Rel(opts.Workdir, entries[i].Path); relErr == nil && !strings.HasPrefix(rel, "..") {
				entries[i].RelPath = filepath.ToSlash(rel)
			}
		}
		// Same treatment for the diagnostics: the studio surfaces them, so
		// give them the workspace-relative path it can open too. Manifest
		// loading also embeds the absolute path in Error; strip the same
		// workspace prefix there before the diagnostic crosses an API boundary.
		workdirPrefix := filepath.Clean(opts.Workdir) + string(filepath.Separator)
		for i := range diags {
			if rel, relErr := filepath.Rel(opts.Workdir, diags[i].Path); relErr == nil && !strings.HasPrefix(rel, "..") {
				diags[i].Path = filepath.ToSlash(rel)
			}
			diags[i].Error = strings.ReplaceAll(diags[i].Error, workdirPrefix, "")
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, diags, nil
}

// WorkdirFromPaths recovers the workspace root from a set of discovery
// paths produced by DefaultPaths (i.e. the parent of a `bots`,
// `examples`, or `.botz` root). Returns "" for custom path sets with no
// recognised root — callers then fall back to manifest-default behaviour
// (no overlay). Lets the dispatcher's RoutingRunner apply the workspace
// overlay without threading an explicit workdir through its Config.
func WorkdirFromPaths(paths []string) string {
	for _, p := range paths {
		switch filepath.Base(p) {
		case "bots", "examples", ".botz":
			return filepath.Dir(p)
		}
	}
	return ""
}

// ResolveBotPath looks up a bot by name across paths and returns the
// path to its workflow source file (bundle's main.bot or loose .bot).
// Returns os.ErrNotExist when no bot with that name is found — unless a
// skipped (malformed) bundle matches by directory name, in which case
// the load diagnostic explains WHY the bot is unavailable instead of a
// bare "not found".
func ResolveBotPath(name string, paths []string) (string, error) {
	entries, diags, err := ListWithDiagnostics(ListOptions{Paths: paths})
	if err != nil {
		return "", err
	}
	// Exact match first (fast, unambiguous).
	for _, e := range entries {
		if e.Name == name {
			return e.MainFile(), nil
		}
	}
	// Normalized fallback: tolerate kebab/snake/case differences between
	// the requested name and the discovered bot (e.g. a ticket's
	// bot:"feature_dev" against a catalogue dir "feature-dev"). Without
	// this, every bot needed a dual kebab+snake alias registered.
	nn := NormalizeName(name)
	for _, e := range entries {
		if NormalizeName(e.Name) == nn {
			return e.MainFile(), nil
		}
	}
	// A malformed bundle has no parseable name, so it can only match by
	// its directory / file base — enough to turn "bot not found" into the
	// actual cause on the launch/dispatch path.
	if d := diagForName(diags, name); d != nil {
		return "", fmt.Errorf("bot %q is unavailable — its bundle failed to load: %s", name, d.Error)
	}
	return "", fmt.Errorf("bot %q not found in %v: %w", name, paths, os.ErrNotExist)
}

// NormalizeName canonicalises a bot/assignee name for tolerant matching:
// lowercased, with '_' and spaces folded to '-'. So "feature_dev",
// "Feature Dev", and "feature-dev" all compare equal. Used by
// ResolveBotPath and the dispatcher's RoutingRunner so a ticket's
// bot/assignee need not match the catalogue's exact spelling.
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// BotsDirName is the conventional committable bot directory at a
// workspace root. It is where `iterion bots create` and the studio
// builder land a new bundle, and the first root DefaultPaths discovers.
const BotsDirName = "bots"

// DefaultPaths returns the conventional bot-discovery roots relative to a
// working directory: <dir>/bots, <dir>/examples, <dir>/.botz. Missing
// roots are skipped silently by discovery, so returning all three is
// safe. Shared by the studio HTTP server (GET /api/v1/bots) and the
// studio-embedded dispatcher so both resolve the same catalog when the
// operator didn't pass an explicit --bots-path. (Before this was shared,
// the dispatcher got raw-nil paths and could resolve no catalog bot,
// silently falling back to the default workflow on every explicit-bot
// ticket.)
func DefaultPaths(workDir string) []string {
	return []string{
		filepath.Join(workDir, BotsDirName),
		filepath.Join(workDir, "examples"),
		filepath.Join(workDir, ".botz"),
	}
}

// discoverBots walks each root and produces one Entry per discovered
// bot. Bundles (directories with manifest.yaml + main.bot) collapse
// into one entry; individual .bot files become one entry each.
// Missing roots are skipped silently so callers can pass optimistic
// default paths. A source that fails to load (malformed manifest,
// unreadable file or subdirectory) is recorded as a DiscoveryError and
// skipped — the walk continues so one bad bundle cannot blank its valid
// siblings. The error return stays for fatal failures (an unstat-able
// or misconfigured explicit root).
func discoverBots(roots []string) ([]Entry, []DiscoveryError, error) {
	var entries []Entry
	var diags []DiscoveryError
	seen := map[string]bool{}
	// Names are claimed as sources are encountered, not in a final pass.
	// This preserves root precedence even when the higher-precedence source
	// is malformed: bots/foo must reserve "foo" so a stale .botz/foo cannot
	// silently become runnable in its place.
	seenName := map[string]bool{}
	addEntry := func(e *Entry) {
		if e == nil || seen[e.Path] {
			return
		}
		seen[e.Path] = true
		key := NormalizeName(e.Name)
		if seenName[key] {
			return
		}
		seenName[key] = true
		entries = append(entries, *e)
	}
	// Diags dedupe by path the way entries do: overlapping roots (e.g. an
	// explicit "." beside "./bots") would otherwise report the same
	// malformed bundle once per root that reaches it.
	seenDiag := map[string]bool{}
	recordDiag := func(path string, kind DiscoveryErrorKind, err error) {
		if kind == DiscoveryErrorBundle || kind == DiscoveryErrorFile {
			base := filepath.Base(path)
			if kind == DiscoveryErrorFile {
				base = strings.TrimSuffix(base, filepath.Ext(base))
			}
			seenName[NormalizeName(base)] = true
		}
		if seenDiag[path] {
			return
		}
		seenDiag[path] = true
		diags = append(diags, DiscoveryError{Path: path, Error: err.Error(), Kind: kind})
	}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("bots: stat %s: %w", root, err)
		}
		if !info.IsDir() {
			if !workflowfile.IsWorkflowFile(root) {
				return nil, nil, fmt.Errorf("bots: unsupported bot file extension for %s (expected .bot)", root)
			}
			e, err := parseBotFile(root)
			if err != nil {
				recordDiag(root, DiscoveryErrorFile, err)
				continue
			}
			addEntry(e)
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				// An unreadable subdirectory (permission denied) is the
				// same failure shape as a malformed bundle — one bad branch
				// must not blank the rest of the workspace. Record and
				// carry on (for a directory, WalkDir skips its contents).
				recordDiag(path, DiscoveryErrorWalk, walkErr)
				return nil
			}
			if d.IsDir() {
				manifest := filepath.Join(path, bundle.ManifestFile)
				mainBot := filepath.Join(path, bundle.MainBotFile)
				if fileExists(manifest) && fileExists(mainBot) {
					e, err := parseBundle(path)
					if err != nil {
						recordDiag(path, DiscoveryErrorBundle, err)
						// SkipDir either way: a malformed bundle must not
						// leak its main.bot back in as a loose bot file.
						return filepath.SkipDir
					}
					addEntry(e)
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !workflowfile.IsWorkflowFile(name) {
				return nil
			}
			e, err := parseBotFile(path)
			if err != nil {
				recordDiag(path, DiscoveryErrorFile, err)
				return nil
			}
			addEntry(e)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return entries, diags, nil
}

func parseBundle(dir string) (*Entry, error) {
	m, err := bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("bots: %w", err)
	}
	if m == nil {
		m = &bundle.Manifest{}
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	if fm := bundle.ReadFrontmatter(filepath.Join(dir, "main.bot")); fm != nil {
		if len(fm.Triggers) > 0 {
			m.Triggers = fm.Triggers
		}
		if len(fm.Capabilities) > 0 {
			m.Capabilities = fm.Capabilities
		}
	}
	return &Entry{
		Name:            m.Name,
		DisplayName:     m.DisplayName,
		Icon:            m.Icon,
		Description:     strings.TrimSpace(m.Description),
		Path:            dir,
		Triggers:        m.Triggers,
		Capabilities:    m.Capabilities,
		DispatchVars:    m.DispatchVars,
		Forge:           m.Forge,
		Repo:            m.Repo,
		ConfigShare:     m.ConfigShare,
		Launch:          m.Launch,
		Chat:            m.Chat,
		Produces:        m.Produces,
		Consumes:        m.Consumes,
		Invocations:     bundle.EffectiveInvocations(m),
		WhenToUse:       strings.TrimSpace(m.WhenToUse),
		Author:          m.Author,
		Version:         m.Version,
		Enabled:         m.IsEnabled(), // manifest default; overlay composed in List
		ManifestEnabled: m.IsEnabled(), // preserved pre-overlay for the studio Bot panel
		IsBundleDir:     true,
	}, nil
}

func parseBotFile(path string) (*Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bots: read %s: %w", path, err)
	}
	fm := bundle.ParseFrontmatter(raw)
	// Loose .bot files carry no manifest, so they default to
	// enabled (overlay may still flip them in List) and are not
	// manifest-editable.
	e := &Entry{Path: path, Enabled: true, ManifestEnabled: true}
	if fm != nil {
		e.Name = fm.Name
		e.Description = fm.Description
		e.Triggers = fm.Triggers
		e.Capabilities = fm.Capabilities
	}
	if e.Name == "" {
		base := filepath.Base(path)
		if ext := filepath.Ext(base); strings.EqualFold(ext, ".bot") {
			e.Name = strings.TrimSuffix(base, ext)
		} else {
			e.Name = base
		}
	}
	if e.Description == "" {
		e.Description = leadingCommentDescription(raw, filepath.Base(path))
	}
	return e, nil
}

// leadingCommentDescription returns the first paragraph of `## ` lines
// at the top of the file (excluding any `## ---` framing). Stops at the
// first blank line or non-comment line. Decoration-only lines (banner
// rules like `## ────`) and a header line repeating the file's own name
// are skipped — they are framing, not description.
func leadingCommentDescription(raw []byte, filename string) string {
	lines := strings.Split(string(raw), "\n")
	var out []string
	skippingFM := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == "## ---" {
			skippingFM = !skippingFM
			continue
		}
		if skippingFM {
			continue
		}
		if !strings.HasPrefix(trim, "##") {
			if len(out) > 0 {
				break
			}
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trim, "##"), " "))
		if body == "" || isDecorationLine(body) || body == filename {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, body)
	}
	return strings.Join(out, " ")
}

// isDecorationLine reports whether a comment body is pure framing — no
// letter or digit in it (e.g. `────…`, `=====`, `***`).
func isDecorationLine(body string) bool {
	for _, r := range body {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
