// The helpers below are shared with internal/sweep/catalogcorpus, which holds
// the corpus-wide guards that once lived in this package and keeps its own
// copy of each: the tests that stay here — the plan-phase, copilot,
// revi-converse and secured-renovacy behaviour tests among them — read them
// over the bots this package owns.

package bots

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/types"
	"github.com/SocialGouv/iterion/pkg/store"
)

// compileBotFile compiles one workflow file with its imported fragments, and
// fails closed on BOTH stages. A parse error leaves a partial AST that
// compiles into a workflow missing whatever the parser could not read — nodes
// included — so a guard reading only the compile result would call a bot clean
// because half of it was invisible.
func compileBotFile(t *testing.T, path string) *ir.Workflow {
	t.Helper()
	pr := parseBotUnit(path)
	// Any diagnostic, not the error-severity ones: parser.SeverityError is
	// the ZERO value of parser.Severity, so a future warning that forgets to
	// set the field would read as an error — and nothing in the parser
	// constructs a warning today, which makes a severity filter here a no-op
	// that only misleads.
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("%s does not parse: %+v", path, pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	return cr.Workflow
}

// declaredSkillDependencies reads one bundle's `dependencies.skills`, keyed
// by the name its own `skills:` lists reference. A bundle with no manifest
// declares none, which is every bundle that carries a copy of every skill it
// references.
func declaredSkillDependencies(t *testing.T, bundleDir string) map[string]bundle.SkillDependency {
	t.Helper()
	m, err := bundle.LoadManifest(filepath.Join(bundleDir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("%s: load manifest: %v", bundleDir, err)
	}
	if m == nil {
		return nil
	}
	deps := make(map[string]bundle.SkillDependency, len(m.Dependencies.Skills))
	for _, dep := range m.Dependencies.Skills {
		deps[dep.Name] = dep
	}
	return deps
}

// declaredSkills returns every skill name appearing in any `skills: [...]`
// list in a .bot source, de-duplicated.
func declaredSkills(src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(src, "\n") {
		// A `##` comment may legitimately show an example list; only the
		// real declaration counts.
		if strings.HasPrefix(strings.TrimSpace(line), "##") {
			continue
		}
		m := skillsListRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, raw := range strings.Split(m[1], ",") {
			name := strings.Trim(strings.TrimSpace(raw), `"'`)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// inputFields lists what a node's input carries: its input schema's fields,
// then, sorted, every other key its incoming edges' `with` maps onto it.
//
// A value's shape follows how it ARRIVES. A mapping that is one whole
// `{{…}}` passes the value through with its type, whatever the field
// declares — a whole output reaches a `string` field as a map — while every
// other mapping delivers a string (ir.MappingArrivesAsText, the runtime's own
// resolveMapping decision). On the entry node the run's launch value is one
// more route, typed as declared. Where several routes write one key the
// NARROWEST shape is taken: the guard must hold on whichever route runs, so a
// drill counts only when every route delivers a value it can walk. A key no
// edge writes keeps its declared shape — whether it holds anything at all is
// the read-side guard's question (TestCatalogInputReadsAreMappedByAnIncomingEdge).
func inputFields(t *testing.T, w *ir.Workflow, node, schemaName string) []inputField {
	t.Helper()
	declared := map[string]types.FieldType{}
	var names []string
	if schemaName != "" {
		s := w.Schemas[schemaName]
		if s == nil {
			t.Fatalf("%s: input schema %q is not in the compiled workflow — a clean compile refuses that (C002)", node, schemaName)
		}
		for _, f := range s.Fields {
			if f == nil {
				continue
			}
			if _, dup := declared[f.Name]; !dup {
				declared[f.Name] = f.Type
				names = append(names, f.Name)
			}
		}
	}
	mapped, text := map[string]bool{}, map[string]bool{}
	var undeclared []string
	for _, e := range w.Edges {
		if e == nil || e.To != node {
			continue
		}
		for _, dm := range e.With {
			if dm == nil {
				continue
			}
			if _, isDeclared := declared[dm.Key]; !isDeclared && !mapped[dm.Key] {
				undeclared = append(undeclared, dm.Key)
			}
			mapped[dm.Key] = true
			if ir.MappingArrivesAsText(dm) {
				text[dm.Key] = true
			}
		}
	}
	sort.Strings(undeclared)
	names = append(names, undeclared...)

	fields := make([]inputField, 0, len(names))
	for _, name := range names {
		ft, isDeclared := declared[name]
		launched := isDeclared && node == w.Entry
		var shape valueShape
		switch {
		case !mapped[name]:
			shape = declaredShape(ft)
		case text[name] || (launched && declaredShape(ft) == shapeText):
			shape = shapeText
		case isDeclared && ft == types.FieldTypeFile:
			shape = shapeFile
		default:
			shape = shapeData
		}
		fields = append(fields, inputField{name: name, declared: isDeclared, shape: shape})
	}
	return fields
}

// marker is the value a field carries during the render. Delimited on both
// sides, so the marker of `a` is not a substring of the marker of `ab`.
func marker(field string) string { return "MARKER\u00b7" + field + "\u00b7MARKER" }

var numericSegment = regexp.MustCompile(`^[0-9]+$`)

// valueShape is how a value on a node's input can be walked at runtime.
type valueShape int

const (
	// shapeText renders whole, and a sub-path on it stays verbatim.
	shapeText valueShape = iota
	// shapeData is walked by the node's own drills: what it holds is data.
	shapeData
	// shapeFile is the upload descriptor, with its fixed keys.
	shapeFile
)

// missingInputFields is THE reading, shared by the catalogue sweep and the
// fixtures below: for one node, the input fields none of its channels deliver
// to the model, decided by rendering those channels with the real resolver.
// ok is false for a node that is not an LLM node, or whose input carries
// nothing the bot put there — no input schema, and no key mapped onto it.
func missingInputFields(t *testing.T, w *ir.Workflow, node string) (r nodeReading, ok bool) {
	t.Helper()
	n, isLLM := w.Nodes[node].(ir.LLMNode)
	if !isLLM {
		return nodeReading{}, false
	}
	llm := n.GetLLMFields()
	r.schema = n.GetSchemaFields().InputSchema
	fields := inputFields(t, w, node, r.schema)
	if len(fields) == 0 {
		return nodeReading{}, false
	}
	// No user prompt: the whole input is serialized, every field arrives.
	if llm.UserPrompt == "" {
		return r, true
	}

	var bodies []string
	var refs []*ir.Ref
	for _, pn := range []string{llm.SystemPrompt, llm.UserPrompt} {
		if p := w.Prompts[pn]; p != nil {
			bodies = append(bodies, p.Body)
			refs = append(refs, p.TemplateRefs...)
		}
	}
	if imagesReachModel(w, n) {
		for _, img := range llm.Images {
			parsed, err := ir.ParseRefs(img)
			if err != nil {
				t.Fatalf("%s: images entry %q does not parse: %v", node, img, err)
			}
			bodies = append(bodies, img)
			refs = append(refs, parsed...)
		}
	}

	delivered := renderedInputNames(fields, bodies, refs)

	for _, f := range fields {
		if strings.HasPrefix(f.name, "_") || delivered[f.name] {
			continue
		}
		r.missing = append(r.missing, f.name)
		if !f.declared {
			if r.undeclared == nil {
				r.undeclared = map[string]bool{}
			}
			r.undeclared[f.name] = true
		}
	}
	return r, true
}

// A node's `skills: [...]` list names skills BY NAME, and nothing in the
// toolchain checks that a name resolves.
//
// `iterion validate` compiles the graph and lints the bundle's skill
// FILES (C231–C234: frontmatter present, description not terse, names
// unique) — but it never joins the two. A name in `skills:` that matches
// no file is not a compile error and not a lint finding; the runtime
// simply mirrors nothing for it, and the agent's Skill tool comes up
// empty on a skill the prompt told it to load.
//
// That failure is silent in the worst way: the bot still answers. It just
// answers from the model's own priors instead of the bundle's authored
// knowledge, which reads as "the bot got dumber" rather than as a typo.
// Same family as the `_session_id` trap the DSL-authoring skill warns
// about — a load-bearing string the compiler does not check.
//
// So: every name declared in a catalog bot's `skills:` list must exist as
// `<bundle>/skills/<name>.md`, with matching `name:` frontmatter — or be
// DECLARED as a shared dependency the install materializes at exactly that
// path (`dependencies.skills`, ADR-144 clause 2), which this gate holds to
// the same promise, against the producer in this very tree.
//
// EXCEPT that `skills:` resolves from three places, not one — bundle,
// plugin, then the operator-curated skill LIBRARY (ADR-059), in that
// precedence. A bot may therefore reference a skill it deliberately does
// NOT ship, so the operator supplies it. That is a design choice, not a
// typo, and the two are indistinguishable from the name alone — hence the
// allowlist below rather than a cleverer rule. Adding to it is a decision
// to be defended in review; the entry carries its reason.
var operatorAttachedSkills = map[string]string{
	// app-dev (Appy) and review-env deploy to a platform they refuse to
	// know: the playbook — auth, provisioning, how to derive the public
	// URL — is the skill the operator attaches, alongside a mounted
	// credential. Shipping one would put a platform literal in a catalog
	// bot, which is exactly what "catalog bots are repo-agnostic" forbids.
	// Both bots handle its absence explicitly.
	"deploy-target": "operator-attached platform playbook; shipping one would pin the bot to a platform",
	// product-docs (Prody) publishes the same way: the Onyxia/SSPCloud
	// HOW is an org-private skill the operator attaches. Shipping it
	// would pin a catalog bot to one datalab. Absence is handled by
	// publish_gate + optional secrets; swap the skill to swap target.
	"deploy-onyxia-sspcloud": "org-private operator-attached Onyxia-SSPCloud publish playbook; product-docs handles its absence via publish_gate / optional secrets",
}

// planRevisePromptNode is the node this pair is about, and the edge target
// whose incoming `with` mappings build its input.
const planRevisePromptNode = "plan_revise"

// promptSurfacesOf lists them. A tool's command, a compute node's
// expressions and a human node's `review_url` are resolved against the same
// input but are not prompts: no model reads them, so prose cannot live
// there. The human INTERACTION prompt is left out for the reason the mirror
// guard leaves it out — the runtime resolves the question map against the
// run, not against this node's input.
func promptSurfacesOf(w *ir.Workflow, n ir.Node) []promptSurface {
	var out []promptSurface
	add := func(surface, name string) {
		if name == "" {
			return
		}
		p := w.Prompts[name]
		if p == nil {
			return
		}
		out = append(out, promptSurface{surface: surface, prompt: name, body: p.Body, refs: p.TemplateRefs})
	}
	switch x := n.(type) {
	case *ir.AgentNode:
		add("agent system prompt", x.SystemPrompt)
		add("agent user prompt", x.UserPrompt)
	case *ir.JudgeNode:
		add("judge system prompt", x.SystemPrompt)
		add("judge user prompt", x.UserPrompt)
	case *ir.RouterNode:
		add("router system prompt", x.SystemPrompt)
		add("router user prompt", x.UserPrompt)
	case *ir.HumanNode:
		add(surfaceHumanInstructions, x.Instructions)
		add(surfaceHumanSystemPrompt, x.SystemPrompt)
	}
	return out
}

// renderedInputNames is THE reading of "a channel of this node renders it",
// shared by the ratchet above and by the prose guard
// (prompt_prose_input_refs_test.go): render the given bodies through the real
// resolver with a distinct marker as each field's value, and report the
// fields whose marker comes out. Two guards asking the same question ask it
// of the same renderer, so they cannot disagree about one field.
func renderedInputNames(fields []inputField, bodies []string, refs []*ir.Ref) map[string]bool {
	input := markerInput(fields, refs)
	resolver := &model.TemplateResolver{}
	var rendered strings.Builder
	for _, b := range bodies {
		rendered.WriteString(resolver.Resolve(b, input, nil))
		rendered.WriteString("\n")
	}
	out := rendered.String()
	delivered := make(map[string]bool, len(fields))
	for _, f := range fields {
		if strings.Contains(out, marker(f.name)) {
			delivered[f.name] = true
		}
	}
	return delivered
}

// secretShape is a declaration as the run materialises and guards it: every
// field but the name and the description, the host and sink lists as sets.
func secretShape(s *ir.Secret) ir.Secret {
	c := *s
	c.Name, c.Description = "", ""
	c.Hosts, c.Sinks = sortedSet(s.Hosts), sortedSet(s.Sinks)
	return c
}

// skillFrontmatterNameRe reads `name:` out of a SKILL.md's frontmatter.
var skillFrontmatterNameRe = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)

// stripSkillsDeclarations removes everything the MODEL never reads, so that
// what remains is prompt text and only prompt text:
//
//   - the `skills: [...]` declaration itself — mirroring a file is not
//     telling the agent the file exists;
//   - every `##` comment. `##` is the DSL's comment marker: the lexer eats
//     the line, so a skill "documented" beside its declaration is invisible
//     to the model. Caught by mutation-testing this very check, which passed
//     against a prompt roster I had deleted because a comment still named
//     the skill.
func stripSkillsDeclarations(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if skillsListRe.MatchString(line) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "##") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestCatalogBotsParseAndCompileClean is the CI floor that every shipped
// catalog workflow (bundles under bots/ + loose .bot files under examples/)
// must PARSE and COMPILE without a single error-severity diagnostic.
//
// This is the guard that was missing when bots/review-pr/main.bot shipped with
// `agent emit:` — a node named after the reserved `emit` node-type keyword
// (ADR-051), which the parser rejects with E002. That bot passed CI because the
// two tests that DO load every catalog bot both bail on a parse/compile failure
// ("another test's job") without ever failing:
//   - bundle_consistency_test.go: `if pr.File == nil { continue }`
//   - catalog_typing_test.go: only inspects four typing codes
//
// So a non-parseable bot sailed through CI and only surfaced in prod as a 502
// on the review-pr webhook (`launch failed: parse error … expected agent
// name`). This test IS that job: a catalog bot that a fresh `iterion validate`
// would reject must fail here, at build time, not at launch time.
//
// It asserts exactly what the launch path requires: parser.Parse must produce
// a File with no error diagnostics, and ir.Compile must produce a Workflow with
// no error diagnostics. Warnings are out of scope (the typing/consistency tests
// own those).
// teamBotFiles lists every workflow of the catalog's bundles, not only each
// bot's main.bot: a sibling workflow (extend.bot, reanchor.bot,
// sync-harness.bot) launches by the same path and must pass the same gates.
//
// It reads the bundle ROOT of catalogRoots, so the bundles this hands to the
// guards that read them alone and the bundles catalogWorkflowFiles walks can
// never be spelled apart — and the root's floor is asserted here too. Measured
// with a second spelling of the same glob: narrowing this one alone left FOUR
// of its seven readers green over zero bundle workflow files — the
// author-quoted-ref guard (an injectable template shape), the typing
// regressions and the two universality guards — while the other three
// reddened only on an emptiness check of their own.
func teamBotFiles(t *testing.T) []string { return catalogRootFiles(t, bundleRoot) }

// exampleFragments are the .bot files under examples/ that are not
// workflows: declaration catalogues meant to be pasted into one
// (examples_cursors_test.go compiles the cursor catalogue inside a workflow).
// Discovery routes them anyway, so each is named in routableNonWorkflows too —
// and dropping it from either list reddens the other.
var exampleFragments = map[string]bool{"../examples/cursors/cursors.bot": true}

// catalogRoot is one glob of the shipped corpus, with the floor under that
// root's OWN discovery and what the root holds.
type catalogRoot struct {
	glob  string
	least int
	what  string
}

// bundleManifestRoot is every bundle manifest under bots/, floored on its own
// discovery: the persona walk (catalog_naming_test.go) reads the catalogue
// through it.
var bundleManifestRoot = catalogRoot{"*/manifest.yaml", 30, "every bundle manifest under bots/"}

func toSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}

// untrustedInputBoundaryMarker is the phrase every acting prompt carries. It
// is a posture cue, not a directive list: the paragraph it heads tells the
// LLM that what it reads out of the target repository is data, and a guard
// that enumerated forbidden spellings instead would be widened by the next
// payload (docs/agents/bots/untrusted-input.md, "Prompts that can act carry the
// UNTRUSTED INPUT BOUNDARY").
const untrustedInputBoundaryMarker = "UNTRUSTED INPUT BOUNDARY"

// untrustedInputBoundaryParagraphs cuts out, per UNTRUSTED INPUT BOUNDARY
// marker in a prompt body, the paragraph that marker heads: the marker line
// itself and every line up to the blank line that closes it. The marker line
// is INSIDE the region — it is the line an author is most likely to extend
// ("… BOUNDARY: {{input.body}} below is data"), and a cut starting past it
// would call that paragraph read while a payload rendered into it. The scan
// resumes after the paragraph it just took, so a body carrying two markers
// yields two paragraphs.
//
// One cut, read by both guards built on it — what the paragraph may not
// interpolate, and what it must name — so they cannot disagree about where a
// paragraph ends.
func untrustedInputBoundaryParagraphs(body string) [][]string {
	lines := strings.Split(body, "\n")
	var out [][]string
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(lines[i], untrustedInputBoundaryMarker) {
			continue
		}
		start := i
		for j := start; j < len(lines) && (j == start || strings.TrimSpace(lines[j]) != ""); j++ {
			i = j
		}
		out = append(out, lines[start:i+1])
	}
	return out
}

// bundleRoot is the catalogue's bundles. Named, because teamBotFiles hands the
// same corpus to the guards that read the bundles alone: one root, one
// spelling, so the two enumerators of this file cannot drift apart.
var bundleRoot = catalogRoot{"*/*.bot", 35, "the catalogue bundles under bots/, every entrypoint of each — not only main.bot"}

// catalogRootFiles lists what ONE root contributes to the shipped corpus — its
// glob's matches, the declaration fragments dropped — and fails its caller
// when that is fewer files than the root's floor, naming the root that broke.
//
// It is the one place a root's files are produced, so no enumerator can hand
// them on without the floor: most walks of the catalogue hold no floor of
// their own, and a guard reading a root that matches nothing — a layout
// rename, or a pattern filepath.Glob rejects, which yields no match and no
// error — reports the same green as one reading it whole. A `-run` that
// selects a single guard reddens here too.
//
// The floor reads the KEPT files, not the raw match count: four more
// declaration fragments would otherwise shrink the corpus every guard walks
// while the floor still read the files the glob matched.
func catalogRootFiles(t *testing.T, root catalogRoot) []string {
	t.Helper()
	paths, err := filepath.Glob(root.glob)
	if err != nil {
		t.Fatalf("glob %q (%s): %v", root.glob, root.what, err)
	}
	kept := make([]string, 0, len(paths))
	for _, p := range paths {
		if !exampleFragments[p] {
			kept = append(kept, p)
		}
	}
	if len(kept) < root.least {
		t.Fatalf("root %q (%s) contributes %d workflow file(s), want at least %d: the discovery broke, and every guard built on this root is now passing over a corpus that lost it",
			root.glob, root.what, len(kept), root.least)
	}
	return kept
}

// declaredShape is the shape of a value that arrives typed as declared.
func declaredShape(t types.FieldType) valueShape {
	switch t {
	case types.FieldTypeFile:
		return shapeFile
	case types.FieldTypeJSON:
		return shapeData
	}
	return shapeText
}

// imagesReachModel reports whether a node's `images:` entries are delivered to
// a model at all. Only the codex backend forwards them. The route read is the
// one the source DECLARES, as the compiler reads it: the node's own
// `backend:`, else the workflow's `default_backend:`, a `${X:-codex}` dial by
// its default — and a `{{vars.x}}` declares nothing, since it leaves the route
// to the launch, so it does not count as codex. A launch-time override (the
// studio's backend picker, `--backend`) is the operator re-routing what the
// source declared, out of scope like a preset fragment. A fallback onto
// another backend drops the entries on that route, so it disqualifies the
// channel too; a route naming no backend runs on the node's (a skip route
// never names one — the compiler refuses it).
func imagesReachModel(w *ir.Workflow, n ir.LLMNode) bool {
	if ir.SourceNodeBackendName(n.GetLLMFields().Backend, w.DefaultBackend) != "codex" {
		return false
	}
	for _, fb := range n.GetFallbacks() {
		if strings.TrimSpace(fb.Backend) != "" && ir.SourceBackendName(fb.Backend) != "codex" {
			return false
		}
	}
	return true
}

// inputField is one value an LLM node's input carries.
type inputField struct {
	name string
	// declared is false for a key an incoming edge maps onto the node that its
	// input schema does not declare.
	declared bool
	shape    valueShape
}

// markerInput builds the input a node is rendered against: each field's
// marker, in the field's shape. A file value is the upload descriptor, so a
// drill renders exactly into the keys the descriptor has; a data value is
// shaped from the node's own drills, so that each finds a map to walk; a text
// value is the bare marker, and a sub-path on it stays verbatim.
func markerInput(fields []inputField, refs []*ir.Ref) map[string]any {
	input := map[string]any{}
	drillable := map[string]bool{}
	for _, f := range fields {
		switch f.shape {
		case shapeFile:
			input[f.name] = fileDescriptor(marker(f.name))
		case shapeData:
			input[f.name] = marker(f.name)
			drillable[f.name] = true
		default:
			input[f.name] = marker(f.name)
		}
	}
	for _, ref := range refs {
		if ref.Kind != ir.RefInput || len(ref.Path) < 2 || !drillable[ref.Path[0]] {
			continue
		}
		name := ref.Path[0]
		// A numeric segment is read as an array index, which the resolver
		// never walks — it walks maps only — so the drill ends there. A json
		// map keyed by digits would render it; where an index is the likely
		// intent, the report beats the benefit of the doubt. A drill that ends
		// at the field itself shapes nothing: the field keeps its marker, which
		// a whole-value render of it then shows.
		sub := ref.Path[1:]
		for i, seg := range sub {
			if numericSegment.MatchString(seg) {
				sub = sub[:i]
				break
			}
		}
		if len(sub) == 0 {
			continue
		}
		// Walk the value, turning each hop into a map; the leaf carries the
		// marker. A leaf that is already a map is kept: rendering it whole
		// still shows the marker inside it.
		m, ok := input[name].(map[string]any)
		if !ok {
			m = map[string]any{}
			input[name] = m
		}
		for i, seg := range sub {
			if i == len(sub)-1 {
				if _, isMap := m[seg].(map[string]any); !isMap {
					m[seg] = marker(name)
				}
				break
			}
			next, isMap := m[seg].(map[string]any)
			if !isMap {
				next = map[string]any{}
				m[seg] = next
			}
			m = next
		}
	}
	return input
}

// nodeReading is what one LLM node's channels deliver of its input.
type nodeReading struct {
	schema string // the node's input schema; "" when it declares none
	// missing are the input fields no channel delivers: schema fields in
	// declaration order, then undeclared mapped keys.
	missing []string
	// undeclared marks the missing keys that are mapped onto the node but
	// declared on no input schema of it.
	undeclared map[string]bool
}

// promptSurface is one prompt body the runtime resolves against a node's own
// input, named as the mirror guard names it (catalog_input_mapping_test.go,
// inputReadsOf).
type promptSurface struct {
	surface string
	prompt  string
	body    string
	refs    []*ir.Ref
}

// skillsListRe matches a node's inline skills list. Lists in this DSL are
// inline arrays — a multi-line YAML sequence does not parse (see the
// iterion-dsl-authoring skill), so one line always holds the whole list.
var skillsListRe = regexp.MustCompile(`^\s+skills:\s*\[([^\]]*)\]`)

func sortedSet(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := slices.Clone(xs)
	sort.Strings(out)
	return slices.Compact(out)
}

// fileDescriptor is a `file` value as a gate upload delivers it — the
// producers' own descriptor plus the `path` the runtime adds
// (pkg/runtime/attachment_path.go) — each key carrying m. The other shape a
// file value can take, a bare path string, renders whole and never drilled.
func fileDescriptor(m string) map[string]any {
	d := store.AttachmentRecord{}.AnswerDescriptor()
	for k := range d {
		d[k] = m
	}
	d["path"] = m
	return d
}

// The two human surfaces promptSurfacesOf names.
const (
	surfaceHumanInstructions = "human instructions"
	surfaceHumanSystemPrompt = "human system prompt"
)
