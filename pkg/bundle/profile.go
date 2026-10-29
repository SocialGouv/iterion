package bundle

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// sourceState is what a reader could do with a subbot child's source.
type sourceState int

const (
	sourceRead    sourceState = iota // read: its profile counts
	sourceMissing                    // inside the bundle and not there: the compiler's to report
	sourceOutside                    // beyond what this reader may read: reported as unread
)

// MaxSyntaxProfile is the highest `dsl: N` profile declared by a bundle's
// executable sources: its main.bot, every child a `subbot source:` reaches
// from it, transitively, and every other .bot at the bundle root — an entry
// of its own, launched by path or declared by another bot, that no edge
// from main.bot reaches. files maps a bundle-relative path to its
// content, as a bot source carries it. declaredBy names the files that
// declare the profile returned. unread names the sources that lie beyond the
// files — a root entry whose target does, or a child in a sibling bundle
// (`../other/main.bot`, a child shape
// the runner resolves within the bundle's collection), an absolute path —
// so a profile of 1 never passes for "checked" when part of the executable
// sources was not read; the caller reports them (C253). A child that is
// simply missing is the compiler's to report.
//
// Why the children count: a cloud runner receives the main workflow as an
// AST, but re-parses a subbot child as TEXT with its own binary, so a child
// written in a newer profile than the runner reads fails at its first parse
// — which is what a declared `requires.iterion` floor exists to refuse at
// admission instead.
func MaxSyntaxRequirements(files map[string]string) SyntaxRequirements {
	var entries []string
	for rel := range files {
		if IsRootEntry(rel) {
			entries = append(entries, rel)
		}
	}
	return walkSyntax(entries, func(rel string) (string, sourceState) {
		if escapesBundle(rel) {
			return "", sourceOutside
		}
		src, ok := files[rel]
		if !ok {
			return "", sourceMissing
		}
		return src, sourceRead
	})
}

// MaxSyntaxProfileDir is MaxSyntaxProfile over a bundle directory on disk.
// It reads within the bundle's COLLECTION — the directory that holds the
// bundle — because a sibling bundle is a child shape the runner resolves
// there; a source that resolves beyond the collection, through `..` or a
// symlink, is not read and is reported as unread.
func MaxSyntaxRequirementsDir(dir string) SyntaxRequirements {
	return maxSyntaxRequirementsDir(dir, nil)
}

// MaxSyntaxRequirementsDirWithMain is MaxSyntaxRequirementsDir for a bundle
// whose main is handed over as text rather than read from disk — an author
// document validated in its bundle: the document's own profile, imports and
// contract count, and a stale main.bot beside it is not read in its place.
func MaxSyntaxRequirementsDirWithMain(dir, mainRel, mainText string) SyntaxRequirements {
	return maxSyntaxRequirementsDir(dir, map[string]string{mainRel: mainText})
}

// MaxSyntaxRequirementsDirStaged is MaxSyntaxRequirementsDir with some
// files' texts — keyed by slash path from the bundle's root — handed over in
// place of the disk's: a writer that decides a floor before it writes reads
// the sources as it will leave them.
func MaxSyntaxRequirementsDirStaged(dir string, staged map[string]string) SyntaxRequirements {
	return maxSyntaxRequirementsDir(dir, staged)
}

func maxSyntaxRequirementsDir(dir string, staged map[string]string) SyntaxRequirements {
	root, collection, ok := collectionOf(dir)
	if !ok {
		root = filepath.Clean(dir) // not there: every source of it is missing
		collection = filepath.Dir(root)
	}
	var entries []string
	if dents, err := os.ReadDir(root); err == nil {
		for _, d := range dents {
			if !d.IsDir() && IsRootEntry(d.Name()) {
				entries = append(entries, d.Name())
			}
		}
	}
	for rel := range staged {
		if IsRootEntry(rel) && !slices.Contains(entries, rel) {
			entries = append(entries, rel)
		}
	}
	return walkSyntax(entries, func(rel string) (string, sourceState) {
		if src, ok := staged[rel]; ok {
			return src, sourceRead // the text handed over, whatever the disk holds under that name
		}
		if strings.HasPrefix(rel, "../../") || rel == "../.." {
			return "", sourceOutside // two levels up leaves the collection by construction
		}
		real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if escapesBundle(rel) {
				return "", sourceOutside // a sibling that is not there: beyond the bundle either way
			}
			return "", sourceMissing
		}
		if !within(real, collection) {
			return "", sourceOutside
		}
		src, err := os.ReadFile(real)
		if err != nil {
			return "", sourceMissing
		}
		return string(src), sourceRead
	})
}

// escapesBundle reports a cleaned slash path that leaves the bundle root.
func escapesBundle(rel string) bool {
	return filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../")
}

// within reports whether path lies under root (both resolved).
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// IsRootEntry reports a bundle-relative slash path that is a .bot file at
// the bundle root other than main.bot: an entry of its own (a child another
// bot declares, or a workflow launched by path), never a fragment — those
// live under lib/ and are reached through imports. The extension folds case,
// the rule workflowfile.IsWorkflowFile opens the file by: a path-promoted
// entry (DirForEntry) must not be invisible to the floor walk. The store
// gate of a cloud bot's validation asks it too, so disk and store cannot
// drift.
func IsRootEntry(rel string) bool {
	return rel != MainBotFile && !strings.Contains(rel, "/") && len(rel) >= 4 && strings.EqualFold(rel[len(rel)-4:], ".bot")
}

func walkSyntax(entries []string, read func(rel string) (string, sourceState)) SyntaxRequirements {
	profile := 0
	var declaredBy, unread, short, importedBy, contractedBy, aliasBy, matchingBy, emptyToolsBy []string
	visited := map[string]bool{}
	var visit func(rel string)
	visit = func(rel string) {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if visited[rel] || rel == "." {
			return
		}
		visited[rel] = true
		src, state := read(rel)
		switch state {
		case sourceOutside:
			unread = append(unread, rel)
			return
		case sourceMissing:
			return
		}
		// The workflow's unit: the file and the fragments its imports reach,
		// each read where the workflow is; a fragment that resolves outside
		// is unread, like a child that does, and a child a fragment
		// declares is followed like the main's own.
		base := filepath.ToSlash(filepath.Dir(rel))
		join := func(frag string) string {
			if base == "." {
				return frag
			}
			return path.Join(base, frag)
		}
		main := path.Base(rel)
		u, err := unit.Load(func(frag string) ([]byte, error) {
			if frag == main {
				return []byte(src), nil
			}
			body, state := read(join(frag))
			switch state {
			case sourceOutside:
				unread = append(unread, join(frag))
				return nil, unit.ErrOutside
			case sourceMissing:
				return nil, os.ErrNotExist
			}
			return []byte(body), nil
		}, main, join)
		if err != nil {
			// The read budget, or a file's size cap, cut the workflow's
			// unit short: the files past the cut are in no list here, and
			// a floor read off the rest would admit runners that cannot
			// read them. Named, never counted (Short).
			short = append(short, rel+": "+err.Error())
			return
		}
		for _, f := range u.Files {
			if f.AST != nil && len(f.AST.Imports) > 0 {
				importedBy = append(importedBy, f.Name)
			}
			if f.AST != nil && len(f.AST.Contracts) > 0 {
				contractedBy = append(contractedBy, f.Name)
			}
			if f.AST != nil && len(aliasUses(f.AST)) > 0 {
				aliasBy = append(aliasBy, f.Name)
			}
			if f.AST != nil && declaresVarMatching(f.AST) {
				matchingBy = append(matchingBy, f.Name)
			}
			if f.AST != nil && declaresEmptyTools(f.AST) {
				emptyToolsBy = append(emptyToolsBy, f.Name)
			}
			p := f.Profile
			switch {
			case p > profile:
				profile = p
				declaredBy = nil
				if p > 1 {
					declaredBy = []string{f.Name}
				}
			case p == profile && p > 1:
				declaredBy = append(declaredBy, f.Name)
			}
		}
		if u.Merged == nil {
			return
		}
		for _, sb := range u.Merged.Subbots {
			if sb.Source == "" {
				continue
			}
			child := sb.Source
			if !filepath.IsAbs(child) && base != "." {
				child = filepath.ToSlash(filepath.Join(base, child))
			}
			visit(child)
		}
	}
	visit(MainBotFile)
	// The root's other .bot files execute under this bundle's manifest
	// without any edge from main.bot reaching them, so the floor counts
	// them the same way.
	for _, e := range slices.Sorted(slices.Values(entries)) {
		visit(e)
	}
	declaredBy = slices.Compact(slices.Sorted(slices.Values(declaredBy)))
	unread = slices.Compact(slices.Sorted(slices.Values(unread)))
	short = slices.Compact(slices.Sorted(slices.Values(short)))
	importedBy = slices.Compact(slices.Sorted(slices.Values(importedBy)))
	contractedBy = slices.Compact(slices.Sorted(slices.Values(contractedBy)))
	aliasBy = slices.Compact(slices.Sorted(slices.Values(aliasBy)))
	matchingBy = slices.Compact(slices.Sorted(slices.Values(matchingBy)))
	emptyToolsBy = slices.Compact(slices.Sorted(slices.Values(emptyToolsBy)))
	return SyntaxRequirements{Profile: profile, DeclaredBy: declaredBy, ImportedBy: importedBy, ContractedBy: contractedBy, AliasBy: aliasBy, MatchingBy: matchingBy, EmptyToolsBy: emptyToolsBy, Unread: unread, Short: short}
}

// aliasUses collects the tool-name spellings of one file's AST that resolve
// only through the Claw alias tier (toolcatalog.BuiltinAlias): the
// agent/judge `tools:` and `tool_policy:` lists, the workflow-level
// `tool_policy:`, a Verified Action's rung-4 `agent_tools:`, and a tool
// node's `command:` spelled as a bare registry-tool name. The exact
// spellings only — a pattern, a ${VAR} or an mcp-qualified name is resolved
// where it is used, exactly as the runtime resolves it. `capabilities:` is
// not a tool list: host rights are C081's domain and never alias.
func aliasUses(f *ast.File) []string {
	var out []string
	add := func(list []string) {
		for _, n := range list {
			if toolcatalog.BuiltinAlias(n) != "" {
				out = append(out, n)
			}
		}
	}
	for _, a := range f.Agents {
		add(a.Tools)
		add(a.ToolPolicy)
	}
	for _, j := range f.Judges {
		add(j.Tools)
		add(j.ToolPolicy)
	}
	for _, g := range f.Groups {
		for _, a := range g.Agents {
			add(a.Tools)
			add(a.ToolPolicy)
		}
		for _, j := range g.Judges {
			add(j.Tools)
			add(j.ToolPolicy)
		}
		for _, t := range g.Tools {
			add([]string{t.Command})
			if t.Recovery != nil {
				add(t.Recovery.AgentTools)
			}
		}
	}
	for _, t := range f.Tools {
		add([]string{t.Command})
		if t.Recovery != nil {
			add(t.Recovery.AgentTools)
		}
	}
	for _, w := range f.Workflows {
		add(w.ToolPolicy)
	}
	return out
}

// MaxSyntaxProfile is MaxSyntaxRequirements projected on the profile.
func MaxSyntaxProfile(files map[string]string) (profile int, declaredBy, unread []string) {
	r := MaxSyntaxRequirements(files)
	return r.Profile, r.DeclaredBy, r.Unread
}

// MaxSyntaxProfileDir is MaxSyntaxRequirementsDir projected on the profile.
func MaxSyntaxProfileDir(dir string) (profile int, declaredBy, unread []string) {
	r := MaxSyntaxRequirementsDir(dir)
	return r.Profile, r.DeclaredBy, r.Unread
}

// SyntaxRequirements is what a bundle's executable sources ask of the
// engine that reads them: the highest `dsl: N` profile they declare and
// whether any of them imports — each with the files that do — plus the
// children the walk could not read.
type SyntaxRequirements struct {
	Profile    int
	DeclaredBy []string
	// ImportedBy names the files that carry `import` lines: a bot in
	// several files needs the release that reads them (parser.ImportSince),
	// whatever its profile.
	ImportedBy []string
	// ContractedBy names the files that declare a `contract`: a bot with a
	// public contract needs the release that reads one
	// (parser.ContractSince), whatever its profile.
	ContractedBy []string
	// AliasBy names the files that spell a Claw tool alias (`Read`, `Bash`,
	// `Grep`) in a tool list: the names resolve only on a runner carrying
	// the alias resolver (ToolAliasesSince), whatever their profile.
	AliasBy []string
	// MatchingBy names the files that declare a var's `[matching: "<re>"]`
	// constraint: the form is unknown below parser.VarMatchingSince, where
	// it is a parse error on the file rather than an unmet requirement on
	// the bundle.
	MatchingBy []string
	// EmptyToolsBy names the files where an agent or judge declares
	// `tools: []`: below DeclaredEmptyToolsSince that list reads as absent,
	// which is the opposite bound on every CLI backend.
	EmptyToolsBy []string
	Unread       []string
	// Short names the executable sources whose unit a bound of the reader
	// cut short — the read budget its files share, a file past the size
	// cap — each with the refusal: what those files use is not known, so no
	// floor read off the others certifies the bundle. A writer of a floor
	// (the push admission, `dsl migrate`, the scaffold) refuses on it and
	// `validate` reports it as an error (C256); the remedy is the reader's
	// budget, never a lower floor.
	Short []string
}

// UsesImport reports whether any source of the bundle imports.
func (r SyntaxRequirements) UsesImport() bool { return len(r.ImportedBy) > 0 }

// UsesContract reports whether any source of the bundle declares a contract.
func (r SyntaxRequirements) UsesContract() bool { return len(r.ContractedBy) > 0 }

// UsesVarMatching reports whether any source declares a var pattern.
func (r SyntaxRequirements) UsesVarMatching() bool { return len(r.MatchingBy) > 0 }

// UsesToolAliases reports whether any source of the bundle spells a Claw
// tool alias in a tool list.
func (r SyntaxRequirements) UsesToolAliases() bool { return len(r.AliasBy) > 0 }

// UsesDeclaredEmptyTools reports whether any source declares an empty
// agent/judge `tools:` list.
func (r SyntaxRequirements) UsesDeclaredEmptyTools() bool { return len(r.EmptyToolsBy) > 0 }

// Asks reports whether the sources use anything a floor is asked for. A
// caller that also has the manifest asks the whole subject
// (FloorSubject.Asks): a floor a manifest KEY asks for is invisible here by
// construction, since no source carries it.
func (r SyntaxRequirements) Asks() bool {
	return FloorSubject{Syntax: r}.Asks()
}

// Describe names what the sources use, for a diagnostic: "dsl profile 2
// (main.bot)", "`import` (main.bot)", or both.
func (r SyntaxRequirements) Describe() string {
	var parts []string
	if r.Profile >= 2 {
		parts = append(parts, fmt.Sprintf("dsl profile %d (%s)", r.Profile, strings.Join(r.DeclaredBy, ", ")))
	}
	if r.UsesImport() {
		parts = append(parts, fmt.Sprintf("`import` (%s)", strings.Join(r.ImportedBy, ", ")))
	}
	if r.UsesContract() {
		parts = append(parts, fmt.Sprintf("`contract` (%s)", strings.Join(r.ContractedBy, ", ")))
	}
	if r.UsesVarMatching() {
		parts = append(parts, fmt.Sprintf("a var `[matching: ...]` constraint (%s)", strings.Join(r.MatchingBy, ", ")))
	}
	if r.UsesToolAliases() {
		parts = append(parts, fmt.Sprintf("the Claw tool alias (%s)", strings.Join(r.AliasBy, ", ")))
	}
	if r.UsesDeclaredEmptyTools() {
		parts = append(parts, fmt.Sprintf("an empty `tools: []` declaration (%s)", strings.Join(r.EmptyToolsBy, ", ")))
	}
	return strings.Join(parts, " and ")
}

// ProfileFloor is what a manifest's `requires.iterion` says about the
// syntax profile a bundle's sources are written in.
type ProfileFloor struct {
	// Profile is the sources' highest profile (MaxSyntaxProfile).
	Profile int
	// Need is the release that reads it (parser.ProfileSince), "" when the
	// profile has none on record.
	Need string
	// Declared is the manifest's requires.iterion, trimmed; "" when absent.
	Declared string
	// Reason names what asks for the floor: "dsl profile 2", "import",
	// "top-level `category`/`tags`" — the entry of the one table that
	// decided Need.
	Reason string
	// Uses names everything of the subject that asks for a floor, each
	// syntax with the files that declare it: the subject of the refusal
	// `validate` (C252) and the push admission write, so neither describes
	// a manifest key from a list of its own.
	Uses string
	// Consequence is what a build below Need does with this bundle, from
	// the table entry that decided the floor: the sentence those two
	// refusals end on, so neither tells an author to expect a failure this
	// floor does not have.
	Consequence string
	// OK: nothing asks for a floor; otherwise a declared floor that is
	// ordered at or above Need (or any declared floor, when Need is unknown).
	OK bool
}

// CheckSyntaxFloor holds a manifest's engine floor against what its bundle
// asks for — a syntax profile above 1, `import`, a key the manifest itself
// declares (FloorSubject) — one predicate for the author's
// `validate` (C252) and the deployment's push admission (409), so the two
// never read a floor two different ways. A floor's PRESENCE is not enough:
// `>= 0.0.1` on such a bundle admits every runner that cannot read it, which
// is exactly what the floor exists to refuse.
func CheckSyntaxFloor(m *Manifest, req SyntaxRequirements) ProfileFloor {
	subject := SubjectOf(m, req)
	pf := ProfileFloor{Profile: req.Profile, Uses: subject.Describe()}
	pf.Need, pf.Reason, pf.Consequence = requiredRelease(subject)
	if pf.Reason == "" {
		pf.OK = true
		return pf
	}
	if m != nil && m.Requires != nil {
		pf.Declared = strings.TrimSpace(m.Requires.Iterion)
	}
	if pf.Declared == "" {
		return pf
	}
	c, err := ParseEngineConstraint(pf.Declared)
	if err != nil {
		return pf
	}
	cmp, ok := c.CompareFloor(pf.Need)
	if !ok {
		pf.OK = true // no release on record: a declared floor is what can be asked
		return pf
	}
	pf.OK = cmp >= 0
	return pf
}

// FloorSubject is what the ONE floors table judges: a bundle's executable
// sources and the keys its own manifest declares. A release floor can be
// asked for by either — a syntax the sources are written in, or a manifest
// key a build below the floor refuses at its strict decode — so every
// reader of the table reads ONE subject: `validate`'s C252, the push
// admission, the scaffold's own manifest and the release test.
type FloorSubject struct {
	// Syntax is what the `.bot` sources use (MaxSyntaxRequirements).
	Syntax SyntaxRequirements
	// Keys is what the manifest declares (Manifest.DeclaredKeys), empty for
	// a caller that has sources alone.
	Keys ManifestKeys
}

// SubjectOf is the floor subject of a manifest and its sources; a nil
// manifest declares no key.
func SubjectOf(m *Manifest, req SyntaxRequirements) FloorSubject {
	s := FloorSubject{Syntax: req}
	if m != nil {
		s.Keys = m.DeclaredKeys
	}
	return s
}

// Asks reports whether anything of the subject asks for a floor.
func (s FloorSubject) Asks() bool {
	_, reason, _ := requiredRelease(s)
	return reason != ""
}

// Describe names what asks for a floor, for a diagnostic: the sources'
// syntaxes with the files that declare them, then the manifest keys — "dsl
// profile 2 (main.bot)", "top-level `category`/`tags`", or both. The key
// half is read from the one table, so a key added there is described
// without a second list to keep in step.
func (s FloorSubject) Describe() string {
	var parts []string
	if sources := s.Syntax.Describe(); sources != "" {
		parts = append(parts, sources)
	}
	for _, f := range syntaxFloors {
		if !f.onManifestKey {
			continue
		}
		if _, reason, asks := f.need(s); asks {
			parts = append(parts, reason)
		}
	}
	return strings.Join(parts, " and ")
}

// sourceParseConsequence is what a build below a floor a SOURCE syntax asks
// for does with the bundle: it re-parses as text what it was handed and
// fails there.
const sourceParseConsequence = "a runner older than the release that reads it re-parses a subbot child, a fragment or the contract as text and fails at that parse"

// syntaxFloor is one thing a floor is asked for: the pins it contributes to
// the registry (by the name of the constant that pins each), what it needs
// of a floor subject, and what a build below it does with the bundle. The
// table below is the ONE list — RequiredRelease reads it, SyntaxFloors
// projects it — so an entry added to it is asked for by `validate`'s C252,
// the push admission, the scaffold and the release test at once, and one
// added anywhere else is a test failure
// (TestEveryFloorTheTableAsksForIsPinned).
type syntaxFloor struct {
	pins map[string]string
	// onManifestKey marks an entry that reads what the MANIFEST declares
	// rather than what the sources use: no source file carries it, so its
	// reason is the whole description a diagnostic can give of it.
	onManifestKey bool
	// consequence is what a build below the floor does with the bundle. It
	// lives with the pin so a floor's failure mode cannot be read two ways,
	// and so a floor added later cannot inherit a consequence that is false
	// for it.
	consequence string
	need        func(FloorSubject) (release, reason string, asks bool)
}

var syntaxFloors = []syntaxFloor{
	{
		pins:        profilePins(),
		consequence: sourceParseConsequence,
		need: func(s FloorSubject) (string, string, bool) {
			if s.Syntax.Profile < 2 {
				return "", "", false
			}
			return parser.ProfileSince[s.Syntax.Profile], fmt.Sprintf("dsl profile %d", s.Syntax.Profile), true
		},
	},
	{
		pins:        map[string]string{"parser.ImportSince": parser.ImportSince},
		consequence: sourceParseConsequence,
		need: func(s FloorSubject) (string, string, bool) {
			return parser.ImportSince, "import", s.Syntax.UsesImport()
		},
	},
	{
		pins:        map[string]string{"parser.ContractSince": parser.ContractSince},
		consequence: sourceParseConsequence,
		need: func(s FloorSubject) (string, string, bool) {
			return parser.ContractSince, "contract", s.Syntax.UsesContract()
		},
	},
	{
		pins:        map[string]string{"bundle.ToolAliasesSince": ToolAliasesSince},
		consequence: "a runner older than the release that reads it keeps resolving the aliased spellings as exact tools or unique MCP shorthand only, and the node's tool list fails at dispatch",
		need: func(s FloorSubject) (string, string, bool) {
			return ToolAliasesSince, "the Claw tool alias", s.Syntax.UsesToolAliases()
		},
	},
	{
		pins: map[string]string{"bundle.DeclaredEmptyToolsSince": DeclaredEmptyToolsSince},
		// This floor is the one whose failure is SILENT: an older runner
		// parses `tools: []` without complaint and reads it as an absent
		// list, which is the opposite bound.
		consequence: "a runner older than the release that reads it parses `tools: []` as an ABSENT list — the opposite bound — and hands the node the CLI backend's own toolset instead of none",
		need: func(s FloorSubject) (string, string, bool) {
			return DeclaredEmptyToolsSince, "an empty `tools: []` declaration", s.Syntax.UsesDeclaredEmptyTools()
		},
	},
	{
		pins:        map[string]string{"parser.VarMatchingSince": parser.VarMatchingSince},
		consequence: sourceParseConsequence,
		need: func(s FloorSubject) (string, string, bool) {
			return parser.VarMatchingSince, `a var's [matching: "<re>"] constraint`, s.Syntax.UsesVarMatching()
		},
	},
	{
		pins:          map[string]string{"bundle.CatalogTaxonomySince": CatalogTaxonomySince},
		onManifestKey: true,
		consequence:   "a build older than the release that reads them decodes the manifest strictly, knows no such field and refuses the whole document",
		need: func(s FloorSubject) (string, string, bool) {
			return CatalogTaxonomySince, "top-level `category`/`tags`", s.Keys.Declares(keyCategory, keyTags)
		},
	},
}

// declaresEmptyTools reports whether any agent or judge of the file — top
// level or inside a group — declares an EMPTY `tools:` list. Nilness is the
// carrier (toolcatalog.ToolsDeclared): a non-nil list of length zero is the
// declaration, an absent one is nil.
func declaresEmptyTools(f *ast.File) bool {
	empty := func(tools []string) bool { return tools != nil && len(tools) == 0 }
	for _, a := range f.Agents {
		if empty(a.Tools) {
			return true
		}
	}
	for _, j := range f.Judges {
		if empty(j.Tools) {
			return true
		}
	}
	for _, g := range f.Groups {
		for _, a := range g.Agents {
			if empty(a.Tools) {
				return true
			}
		}
		for _, j := range g.Judges {
			if empty(j.Tools) {
				return true
			}
		}
	}
	return false
}

// declaresVarMatching reports whether any var of the file — top-level or
// workflow-level — carries a pattern constraint.
func declaresVarMatching(f *ast.File) bool {
	blocks := []*ast.VarsBlock{f.Vars}
	for _, w := range f.Workflows {
		blocks = append(blocks, w.Vars)
	}
	for _, vb := range blocks {
		if vb == nil {
			continue
		}
		for _, field := range vb.Fields {
			if field != nil && field.Matching != "" {
				return true
			}
		}
	}
	return false
}

func profilePins() map[string]string {
	pins := map[string]string{}
	for profile, since := range parser.ProfileSince {
		pins[fmt.Sprintf("parser.ProfileSince[%d]", profile)] = since
	}
	return pins
}

// SyntaxFloors is the registry of the releases a syntax first ships in, by
// the name of the parser constant that pins each — the projection of the
// one table the floor predicate reads, so the release test and a reference
// walk exactly what asks for a floor.
func SyntaxFloors() map[string]string {
	out := map[string]string{}
	for _, f := range syntaxFloors {
		for name, release := range f.pins {
			out[name] = release
		}
	}
	return out
}

// RequiredRelease is the release a set of SOURCES needs — the HIGHEST among
// what they use, read from the one table of what asks for a floor — with
// the reason, or "" and "" when they use nothing a floor is asked for. It
// is the form for a caller that has sources alone (`dsl migrate`); a caller
// that also has the manifest reads RequiredReleaseOf, so a floor a manifest
// key asks for is not lost on the way.
func RequiredRelease(req SyntaxRequirements) (release, reason string) {
	release, reason, _ = requiredRelease(FloorSubject{Syntax: req})
	return release, reason
}

// RequiredReleaseOf is the release a whole subject needs — its sources AND
// the keys its manifest declares. The one arithmetic behind `validate`'s
// C252, the push admission, the scaffold's manifest and `dsl migrate`'s
// floor.
func RequiredReleaseOf(s FloorSubject) (release, reason string) {
	release, reason, _ = requiredRelease(s)
	return release, reason
}

// requiredRelease is RequiredReleaseOf with the consequence of the entry
// that decided the floor — what a build below it does with the bundle,
// which only the winning entry knows.
func requiredRelease(s FloorSubject) (release, reason, consequence string) {
	for _, f := range syntaxFloors {
		rel, why, asks := f.need(s)
		if !asks {
			continue
		}
		if reason == "" || laterRelease(rel, release) {
			release, reason, consequence = rel, why, f.consequence
		}
	}
	return release, reason, consequence
}

// laterRelease reports whether a orders after b; a release not on record
// (unorderable) is earlier than any that is.
func laterRelease(a, b string) bool {
	if _, ok := CompareVersions(a, a); !ok {
		return false
	}
	if _, ok := CompareVersions(b, b); !ok {
		return true
	}
	cmp, _ := CompareVersions(a, b)
	return cmp > 0
}

// CheckProfileFloor is CheckSyntaxFloor for a profile alone.
func CheckProfileFloor(m *Manifest, profile int) ProfileFloor {
	return CheckSyntaxFloor(m, SyntaxRequirements{Profile: profile})
}
