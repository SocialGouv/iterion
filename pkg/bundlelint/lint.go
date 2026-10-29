// Package bundlelint cross-checks a bot bundle's manifest.yaml against its
// compiled main.bot workflow, surfacing structural inconsistencies that
// neither the manifest parser (pkg/bundle) nor the DSL compiler
// (pkg/dsl/ir) can see on their own — because each validates only one side.
//
// The canonical failure it catches: a manifest var-map key (dispatch_vars,
// context_vars, schedule.default_vars, launch_vars, args_var) that names a
// workflow var the main.bot doesn't declare. At runtime such a key is
// silently dropped, so the trigger payload never reaches the bot. bundlelint
// turns that silent drop into a visible diagnostic at `iterion validate`
// time and in CI.
//
// Diagnostics use a dedicated C2xx code family, distinct from the DSL
// compiler's C0xx/C1xx codes, so the two layers never collide and tooling
// can group bundle-level findings by prefix.
package bundlelint

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Code identifies a bundle-consistency diagnostic (C2xx family).
type Code string

const (
	// DiagDispatchVarUnknown: a manifest dispatch_vars key is not a declared
	// workflow var (silently dropped by the dispatcher at runtime).
	DiagDispatchVarUnknown Code = "C200"
	// DiagContextVarUnknown: an invocation context_vars key is not a declared
	// workflow var (silently dropped by the webhook/command launch path).
	DiagContextVarUnknown Code = "C201"
	// DiagScheduleDefaultVarUnknown: an invocation schedule.default_vars key
	// is not a declared workflow var (silently dropped by the scheduler).
	DiagScheduleDefaultVarUnknown Code = "C202"
	// DiagLaunchVarUnknown: a forge.webhook.launch_vars key is not a declared
	// workflow var (silently dropped by the auto-provisioned webhook).
	DiagLaunchVarUnknown Code = "C203"
	// DiagArgsVarUnknown: an invocation args_var names a var the workflow does
	// not declare, so the trigger's free-text payload is dropped.
	DiagArgsVarUnknown Code = "C204"
	// DiagChatNodeUnknown: a chat.nodes key names no compiled workflow node,
	// so the studio cannot associate the manifest renderer with the run event.
	DiagChatNodeUnknown Code = "C205"
	// DiagChatNodeKindMismatch: a manifest human turn points at a workflow
	// node that cannot pause for an operator answer.
	DiagChatNodeKindMismatch Code = "C206"
	// DiagChatFieldInvalid: a summary/text/approved field is missing from the
	// corresponding output schema or has a type the chat surface cannot send.
	DiagChatFieldInvalid Code = "C207"
	// DiagChatSeedVarInvalid: chat.seed_var is absent or not string-compatible.
	DiagChatSeedVarInvalid Code = "C208"
	// DiagChatLauncherVarInvalid: a launcher_vars entry is absent or not
	// string-compatible (the launcher submits string values).
	DiagChatLauncherVarInvalid Code = "C209"
	// DiagChatHostEventModeMismatch: the manifest's host_event_field and the
	// workflow node's `interaction: human_or_host` disagree. The two halves
	// declare one thing — "this gate also accepts a host-attested answer" —
	// and only this package can see both: the IR compiler never reads a
	// manifest, and the manifest loader never sees the compiled graph.
	//
	// The severe direction is a node declaring the mode with no field to
	// receive on: the gate then advertises a standby nothing can ever
	// deliver, which is exactly the inert-declared-gate class the fallback
	// chain refuses under C176.
	DiagChatHostEventModeMismatch Code = "C212"

	// DiagForgeSecretUnknown: the forge secret name the bot expects to be
	// bound has no matching declaration in the main.bot secrets: block.
	DiagForgeSecretUnknown Code = "C210"
	// DiagForgeSecretNotFile: the forge secret is declared but not as a file
	// mount (`as: file`), the form managed forge tokens are bound under.
	DiagForgeSecretNotFile Code = "C211"

	// DiagManifestCapNotInWorkflow: a manifest capability is granted by no
	// workflow-level or node-level capabilities: list.
	DiagManifestCapNotInWorkflow Code = "C220"
	// DiagFrontmatterCapsOverride: the main.bot `## ---` frontmatter declares
	// capabilities that silently override (and differ from) the manifest's.
	DiagFrontmatterCapsOverride Code = "C221"
	// DiagBundleUnopenable: the document is a bundle's main.bot (a
	// manifest.yaml or a skills/ beside it) but the bundle does not open —
	// its manifest does not decode — so the document was validated ALONE,
	// without the bundle's prompts/*.md, presets and skills. The CLI refuses
	// the same state outright; the editor warns, since a manifest mid-edit is
	// a normal state there and a refusal would freeze its diagnostics.
	DiagBundleUnopenable Code = "C222"
	// DiagManifestNotRead: a file named like a manifest sits beside the
	// main.bot but was not read as this bundle's manifest — a typo in its
	// only distinctive key, a file the parser cannot read, one over the size
	// bound — so the file compiled ALONE, without the prompts, presets and
	// skills beside it. The one outcome that would otherwise be silent.
	DiagManifestNotRead Code = "C223"

	// DiagBundleNameTripleMismatch: the bundle carries per-bot memory — a node
	// with `memory: visibility: bot`, or `auto_memory: on` — but the manifest
	// name, workflow name, and bundle dir name disagree, so the bot's memory
	// tree splits across launch paths.
	DiagBundleNameTripleMismatch Code = "C230"

	// Skill-authoring routability (C231–C234). A bundle's skills/*.md are
	// mirrored into .claude/skills/ and selected by the router bot (Nexie)
	// purely from their frontmatter. These checks guard *routability* — that
	// a skill can be discovered and chosen — NOT prose style; they are all
	// warnings (never block authoring) and deliberately impose no phrasing
	// template (no mandated "Use when…/Not for…"), only presence + substance.

	// DiagSkillNameMissing: a skill file has no `name:` frontmatter, so it is
	// undiscoverable by name in the mirrored .claude/skills/ tree.
	DiagSkillNameMissing Code = "C231"
	// DiagSkillDescriptionMissing: a skill file has no `description:`, so the
	// router has no signal for when to select it.
	DiagSkillDescriptionMissing Code = "C232"
	// DiagSkillDescriptionTerse: a skill `description:` is present but too
	// short to route on (e.g. "Security stuff").
	DiagSkillDescriptionTerse Code = "C233"
	// DiagSkillNameDuplicate: two skill files in the bundle declare the same
	// `name:`, so one silently clobbers the other when mirrored.
	DiagSkillNameDuplicate Code = "C234"

	// Engine contract (C250–C251). The manifest may declare the engine build
	// the bundle needs (`requires.iterion`); these hold it against the build
	// doing the validating. The family lives here rather than in ir because
	// the requirement is a MANIFEST fact — ir.Compile only ever sees the
	// .bot AST, which carries no manifest at all.

	// DiagEngineRequirementUnmet: the manifest declares a `requires.iterion`
	// floor this build is below. An error: every node the bundle was written
	// for may reach an evaluator that cannot serve it, and the failure lands
	// mid-run rather than here.
	DiagEngineRequirementUnmet Code = "C250"
	// DiagEngineRequirementUnchecked: a `requires.iterion` is declared but
	// this build carries no orderable version (a `dev` build, a fork's naming
	// scheme), so the comparison could not run. A warning, never silence — an
	// unchecked contract that reads as satisfied is the failure mode the
	// declaration exists to close.
	DiagEngineRequirementUnchecked Code = "C251"
	// DiagProfileNeedsFloor: an executable source of the bundle (its main.bot
	// or a subbot child) declares a syntax profile above 1, and the manifest
	// declares no `requires.iterion`. A warning: the main workflow travels
	// to a cloud runner as an AST, but a child is re-parsed as text by the
	// runner's own binary, and a build older than the profile fails at that
	// parse — which a declared floor refuses at admission instead.
	DiagProfileNeedsFloor Code = "C252"
	// DiagProfileChildUnread: an executable source lies beyond what the
	// profile walk could read — a root-level entry or a subbot child that
	// is a link out of the collection, a child in a sibling bundle or at an
	// absolute path — so the profile reported for the bundle does not
	// speak for that source. A warning: a source written in a newer
	// profile than a runner reads fails at that runner's parse.
	DiagProfileChildUnread Code = "C253"
	// DiagProfileSourceShort: an executable source's unit is past a bound
	// of the reader — the read budget its files share, a file's size cap —
	// so the files past the cut are not read and what they use is not
	// known: no floor read off the rest certifies the bundle. An error, as
	// the push admission refuses it (bundle.SyntaxRequirements.Short).
	DiagProfileSourceShort Code = "C256"

	// Bot navigation vocabulary (C270–C271 — the C2xx bands below 270 are
	// claimed by ir and by the manifest/workflow checks above; these start
	// the next free band). Both are warnings, never errors: a category or
	// tag this build does not know stays declared and the bot stays
	// visible (grouped under Uncategorized) — the spine is curated, but an
	// operator's explicit choice is never silently replaced. What the lint
	// buys is the typo surface and the reuse-before-invent nudge for tags.

	// DiagBotCategoryUnknown: the manifest `category:` is not one of the
	// six closed slugs (bundle.BotCategories) — usually a typo — so the
	// bot lands in Uncategorized on every grouped surface.
	DiagBotCategoryUnknown Code = "C272"
	// DiagBotTagUnknown: a manifest `tags:` entry is outside the curated
	// seed (bundle.KnownBotTags). The set is open on purpose; the warning
	// exists so a new facet is a deliberate act, not a drift.
	DiagBotTagUnknown Code = "C273"

	// Durable memory (C274). A warning, never an error: a skill teaching a
	// write the bundle cannot perform is an authoring gap, and `iterion
	// validate` does not change its exit code on one.

	// DiagSkillDurableMemoryUnserved: a bundle's skill names the durable
	// memory store's own write tool while no node of the bundle can put
	// bytes in a memory tree, so every reader of that page is taught a
	// contract the bot carrying it cannot honour.
	DiagSkillDurableMemoryUnserved Code = "C274"
)

// minRoutableDescription is the shortest `description:` the skill lint treats
// as carrying enough signal for a router to select on. Deliberately low — the
// check flags only the trivially-empty ("Security stuff"), not terse-but-real
// descriptions; routability, not verbosity, is the bar.
const minRoutableDescription = 24

// Severity mirrors ir.Severity semantics: an error makes `iterion validate`
// exit non-zero; a warning is surfaced but non-fatal.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Diag is a single manifest↔workflow consistency finding. Field carries a
// dotted path into the manifest (the attribution surface here is the
// manifest, not the workflow graph — hence Field rather than ir's
// NodeID/EdgeID).
type Diag struct {
	Code     Code
	Severity Severity
	Field    string
	Message  string
	Hint     string
}

// Error renders the diagnostic in the same shape as ir.Diagnostic.Error so
// the studio and CLI display both layers uniformly.
func (d Diag) Error() string {
	if d.Field != "" {
		return fmt.Sprintf("%s [%s] %s: %s", d.Severity, d.Code, d.Field, d.Message)
	}
	return fmt.Sprintf("%s [%s]: %s", d.Severity, d.Code, d.Message)
}

// Input bundles the consistency-check inputs. Manifest and Workflow are the
// core pair; Frontmatter and DirName enable the two checks that need more
// than the core artifacts (C221 needs the raw main.bot frontmatter; C230
// needs the bundle directory basename). Leaving the optional fields at their
// zero value simply skips the checks that depend on them.
type Input struct {
	Manifest    *bundle.Manifest
	Workflow    *ir.Workflow
	Frontmatter *bundle.Frontmatter
	DirName     string
	// Skills carries the bundle's parsed skills/*.md frontmatter for the
	// routability checks (C231–C234). Empty skips them. bundlelint stays
	// I/O-free: the caller scans the files (via skilllib.ScanFrontmatter) and
	// passes the results in, mirroring how Frontmatter is supplied.
	Skills []SkillDoc
	// EngineBuild is the iterion build the bundle is being validated against,
	// for the `requires.iterion` check (C250/C251). Empty skips it — the
	// caller supplies appinfo.FullVersion(); bundlelint stays I/O-free and
	// never reads its own binary's identity, so a caller that has no build to
	// hold the bundle against simply does not ask the question.
	EngineBuild string
	// Syntax is what the bundle's executable sources use — the highest
	// `dsl: N` profile, `import`, `contract`, each with the files that do,
	// and the subbot children the walk could not read
	// (bundle.MaxSyntaxRequirements) — for the floor checks (C252, C253);
	// the zero value asks for nothing. Supplied by the caller, like
	// Frontmatter: bundlelint stays I/O-free. It travels whole so a syntax
	// added to the requirements is checked here without a field to thread.
	Syntax bundle.SyntaxRequirements
	// SubbotContracts are the bound contracts of the `subbot` children the
	// caller could read and compile, by the parent's node id (C255); a
	// child without a contract, or beyond the bundle, is absent. Supplied
	// by the caller: bundlelint stays I/O-free.
	SubbotContracts map[string]*ir.PublicContract
}

// SkillDoc is one bundle skill file's routability-relevant frontmatter. Path
// is the bundle-relative path used for diagnostic attribution (e.g.
// "skills/repo-survey.md").
type SkillDoc struct {
	Path        string
	Name        string
	Description string
	// Body is the page's text, for the reference check that reads which
	// TOOLS a page names (C274). Empty skips that check: the routability
	// checks above need the frontmatter alone, so a caller that has not
	// read the body still asks them. It is never read for prose style —
	// checkSkills' contract forbids that, and C274 matches a tool's name.
	Body string
}

// CheckConsistency cross-checks a bot's manifest against its compiled
// workflow. A nil Manifest skips all manifest-side checks; a nil Workflow
// disables the checks that resolve names against the workflow (var-map,
// forge-secret, capability checks). Returned diagnostics are deterministically
// ordered by (Code, Field).
func CheckConsistency(in Input) []Diag {
	var diags []Diag
	m := in.Manifest
	// The profile checks read the manifest but do not need one: a bundle
	// known by its skills/ alone, written in profile 2, is asked for the
	// manifest that would carry its floor.
	checkSyntaxFloor(&diags, m, in.Syntax, in.EngineBuild)
	checkProfileUnread(&diags, in.Syntax.Unread)
	checkProfileShort(&diags, in.Syntax.Short)
	checkSubbotContracts(&diags, in.Workflow, in.SubbotContracts)
	// Manifest-free, like the checks above it: the question is the skills
	// against the workflow's own nodes, and a bundle known by its skills/
	// and its main.bot alone is owed the same answer.
	checkDurableMemoryPromises(&diags, in.Skills, in.Workflow)
	if m != nil {
		checkContractManifest(&diags, m, in.Workflow)
		checkVarMaps(&diags, m, in.Workflow)
		checkChatSurface(&diags, m, in.Workflow)
		checkForgeSecret(&diags, m, in.Workflow)
		checkCapabilities(&diags, m, in.Workflow, in.Frontmatter)
		checkBundleNameStability(&diags, m, in.Workflow, in.DirName)
		checkSkills(&diags, in.Skills)
		checkEngineRequirement(&diags, m, in.EngineBuild)
		checkBotTaxonomy(&diags, m)
	}

	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Field < diags[j].Field
	})
	return diags
}

// checkBotTaxonomy holds the manifest's declared category and tags against
// the navigation vocabulary (C270, C271). Warnings only: an unknown value
// stays declared and the bot stays visible — the check names the known set
// so the fix is a one-glance edit, never a silent rewrite.
func checkBotTaxonomy(diags *[]Diag, m *bundle.Manifest) {
	if m.Category != "" {
		if _, ok := bundle.BotCategoryBySlug(m.Category); !ok {
			*diags = append(*diags, Diag{
				Code:     DiagBotCategoryUnknown,
				Severity: SeverityWarning,
				Field:    "category",
				Message: fmt.Sprintf("unknown bot category %q — the bot is grouped under Uncategorized",
					m.Category),
				Hint: "known categories: " + bundle.KnownBotCategorySlugs(),
			})
		}
	}
	for _, tag := range m.Tags {
		if !bundle.IsKnownBotTag(tag) {
			*diags = append(*diags, Diag{
				Code:     DiagBotTagUnknown,
				Severity: SeverityWarning,
				Field:    "tags." + tag,
				Message: fmt.Sprintf("tag %q is outside the curated vocabulary — fine if intentional; prefer reusing an existing tag",
					tag),
				Hint: "known tags: " + bundle.KnownBotTagsJoined(),
			})
		}
	}
}

// checkChatSurface joins the manifest's presentation contract to the compiled
// workflow. pkg/bundle can validate only the shape of chat:, while the DSL
// compiler can validate only main.bot; a stale name between them otherwise
// ships a dock that looks interactive but cannot route an answer.
func checkChatSurface(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow) {
	if m.Chat == nil || w == nil {
		return
	}
	for _, id := range sortedChatNodeIDs(m.Chat.Nodes) {
		decl := m.Chat.Nodes[id]
		fieldBase := "chat.nodes." + id
		node, ok := w.Nodes[id]
		if !ok {
			*diags = append(*diags, Diag{
				Code: DiagChatNodeUnknown, Severity: SeverityError, Field: fieldBase,
				Message: fmt.Sprintf("node %q does not exist in the compiled workflow; the chat renderer would never match its run events", id),
				Hint:    "rename the manifest key to a workflow node id or restore the node in main.bot",
			})
			continue
		}
		if decl.Kind == bundle.ChatNodeHuman && node.NodeKind() != ir.NodeHuman {
			*diags = append(*diags, Diag{
				Code: DiagChatNodeKindMismatch, Severity: SeverityError, Field: fieldBase + ".kind",
				Message: fmt.Sprintf("manifest declares an operator turn, but workflow node %q is %s and cannot collect a human answer", id, node.NodeKind()),
				Hint:    "point the chat human entry at a human node or change kind to banner/silent",
			})
			continue
		}

		if decl.SummaryField != "" {
			checkChatSchemaField(diags, w, node, decl.SummaryField, fieldBase+".summary_field", ir.FieldTypeString)
		}
		if decl.TextField != "" {
			checkChatSchemaField(diags, w, node, decl.TextField, fieldBase+".text_field", ir.FieldTypeString)
		}
		if decl.ApprovedField != "" {
			checkChatSchemaField(diags, w, node, decl.ApprovedField, fieldBase+".approved_field", ir.FieldTypeBool)
		}
		if decl.HostEventField != "" {
			checkChatSchemaField(diags, w, node, decl.HostEventField, fieldBase+".host_event_field", ir.FieldTypeJSON)
		}
		checkHostEventMode(diags, node, decl, id, fieldBase)
	}

	if name := m.Chat.SeedVar; name != "" {
		checkChatVar(diags, w, name, "chat.seed_var", DiagChatSeedVarInvalid)
	}
	for i, v := range m.Chat.LauncherVars {
		checkChatVar(diags, w, v.Name, fmt.Sprintf("chat.launcher_vars[%d].name", i), DiagChatLauncherVarInvalid)
	}
}

func checkChatSchemaField(diags *[]Diag, w *ir.Workflow, node ir.Node, name, field string, want ir.FieldType) {
	schemaName := ir.NodeOutputSchema(node)
	schema := w.Schemas[schemaName]
	if schema == nil {
		*diags = append(*diags, Diag{
			Code: DiagChatFieldInvalid, Severity: SeverityError, Field: field,
			Message: fmt.Sprintf("field %q has no compiled output schema on node %q to land in", name, node.NodeID()),
			Hint:    "declare an output schema on the node and add the field with type " + want.String(),
		})
		return
	}
	for _, f := range schema.Fields {
		if f.Name != name {
			continue
		}
		if f.Type != want {
			*diags = append(*diags, Diag{
				Code: DiagChatFieldInvalid, Severity: SeverityError, Field: field,
				Message: fmt.Sprintf("field %q in output schema %q is %s; this chat field requires %s", name, schemaName, f.Type, want),
				Hint:    "change the schema field type or point the manifest at a compatible field",
			})
		}
		return
	}
	*diags = append(*diags, Diag{
		Code: DiagChatFieldInvalid, Severity: SeverityError, Field: field,
		Message: fmt.Sprintf("field %q does not exist in output schema %q of node %q", name, schemaName, node.NodeID()),
		Hint:    "fix the manifest field name or add it to the node's output schema",
	})
}

func checkChatVar(diags *[]Diag, w *ir.Workflow, name, field string, code Code) {
	v, ok := w.Vars[name]
	if !ok {
		*diags = append(*diags, Diag{
			Code: code, Severity: SeverityError, Field: field,
			Message: fmt.Sprintf("variable %q is not declared by the workflow; the launcher value would be discarded", name),
			Hint:    "declare it as a string in the workflow vars: block or fix the manifest name",
		})
		return
	}
	if v.Type != ir.VarString {
		*diags = append(*diags, Diag{
			Code: code, Severity: SeverityError, Field: field,
			Message: fmt.Sprintf("variable %q is %s, but the chat launcher submits a string", name, v.Type),
			Hint:    "change the workflow variable to string or remove it from the chat launcher",
		})
	}
}

func sortedChatNodeIDs(nodes map[string]bundle.ChatNode) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// varDeclared reports whether the workflow declares a var by this name.
func varDeclared(w *ir.Workflow, name string) bool {
	if w == nil || w.Vars == nil {
		return false
	}
	_, ok := w.Vars[name]
	return ok
}

// checkVarMaps verifies every manifest var-map key (and args_var) names a
// var the workflow declares. An undeclared key is dropped silently at
// runtime — exactly the bug class this linter exists to surface.
func checkVarMaps(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow) {
	if w == nil {
		return
	}
	checkVarMap(diags, w, m.DispatchVars, DiagDispatchVarUnknown, "dispatch_vars")
	if m.Forge != nil && m.Forge.Webhook != nil {
		checkVarMap(diags, w, m.Forge.Webhook.LaunchVars, DiagLaunchVarUnknown, "forge.webhook.launch_vars")
	}
	for i, inv := range m.Invocations {
		base := fmt.Sprintf("invocations[%d]", i)
		checkVarMap(diags, w, inv.ContextVars, DiagContextVarUnknown, base+".context_vars")
		if inv.Schedule != nil {
			checkVarMap(diags, w, inv.Schedule.DefaultVars, DiagScheduleDefaultVarUnknown, base+".schedule.default_vars")
		}
		if inv.ArgsVar != "" && !varDeclared(w, inv.ArgsVar) {
			*diags = append(*diags, Diag{
				Code:     DiagArgsVarUnknown,
				Severity: SeverityWarning,
				Field:    base + ".args_var",
				Message:  fmt.Sprintf("args_var %q is not a declared workflow var; the trigger payload will be dropped at runtime", inv.ArgsVar),
				Hint:     "declare it in the workflow vars: block or fix the name",
			})
		}
	}
}

func checkVarMap(diags *[]Diag, w *ir.Workflow, vars map[string]string, code Code, fieldPrefix string) {
	// Iterate in sorted key order so a single map contributes deterministic
	// diagnostics even before the final sort (helps stable test golden order).
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if varDeclared(w, k) {
			continue
		}
		*diags = append(*diags, Diag{
			Code:     code,
			Severity: SeverityWarning,
			Field:    fieldPrefix + "." + k,
			Message:  fmt.Sprintf("key %q is not a declared workflow var; it will be silently dropped at runtime", k),
			Hint:     "declare it in the workflow vars: block or remove it from the manifest",
		})
	}
}

// checkForgeSecret fulfils the cross-reference the manifest parser documents
// but cannot perform (it never sees main.bot): the forge secret the bot
// expects must be declared, and as a file mount.
func checkForgeSecret(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow) {
	if w == nil {
		return
	}
	forgeActive := m.Forge != nil && len(m.Forge.Events) > 0
	for _, inv := range m.Invocations {
		if inv.Kind == bundle.InvocationKindForge {
			forgeActive = true
			break
		}
	}
	if !forgeActive {
		return
	}
	name := m.Forge.SecretName() // nil-safe: returns DefaultForgeSecretName
	sec, ok := w.Secrets[name]
	if !ok {
		*diags = append(*diags, Diag{
			Code:     DiagForgeSecretUnknown,
			Severity: SeverityWarning,
			Field:    "forge.secret",
			Message:  fmt.Sprintf("forge secret %q has no matching declaration in the workflow secrets: block; the managed forge token would be unbound at runtime", name),
			Hint:     "declare `secrets: { " + name + ": { as: file, optional: true } }` in main.bot, or set forge.secret to an existing secret name",
		})
		return
	}
	if !sec.IsFile() {
		*diags = append(*diags, Diag{
			Code:     DiagForgeSecretNotFile,
			Severity: SeverityWarning,
			Field:    "forge.secret",
			Message:  fmt.Sprintf("forge secret %q is declared as %q, but managed forge tokens are bound as a file mount (as: file)", name, sec.As),
			Hint:     "set `as: file` on the secret declaration in main.bot",
		})
	}
}

// checkCapabilities flags manifest capabilities granted by no node (C220)
// and a frontmatter capabilities list that silently overrides a differing
// manifest one (C221).
func checkCapabilities(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow, fm *bundle.Frontmatter) {
	if w != nil && len(m.Capabilities) > 0 {
		granted := map[string]bool{}
		for _, c := range w.Capabilities {
			granted[c] = true
		}
		for _, n := range w.Nodes {
			if ln, ok := n.(ir.LLMNode); ok {
				for _, c := range ln.GetCapabilities() {
					granted[c] = true
				}
			}
		}
		for i, c := range m.Capabilities {
			if !granted[c] {
				*diags = append(*diags, Diag{
					Code:     DiagManifestCapNotInWorkflow,
					Severity: SeverityWarning,
					Field:    fmt.Sprintf("capabilities[%d]", i),
					Message:  fmt.Sprintf("manifest capability %q is granted by no workflow-level or node-level capabilities: list", c),
					Hint:     "add it to a node's capabilities: list, or drop it from the manifest (documentation-only otherwise)",
				})
			}
		}
	}

	if fm != nil && len(fm.Capabilities) > 0 && len(m.Capabilities) > 0 && !sameStringSet(fm.Capabilities, m.Capabilities) {
		*diags = append(*diags, Diag{
			Code:     DiagFrontmatterCapsOverride,
			Severity: SeverityWarning,
			Field:    "capabilities",
			Message:  "main.bot frontmatter capabilities silently override the manifest capabilities (they differ); discovery uses the frontmatter set",
			Hint:     "keep one source of truth — drop the frontmatter capabilities or align the two lists",
		})
	}
}

// checkBundleNameStability generalises the per-bot-memory invariant: a node
// using visibility: bot needs manifest name == workflow name == dir name so
// the bot's memory tree is keyed identically across CLI (workflow name) and
// dispatcher (bundle name) launches.
func checkBundleNameStability(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow, dirName string) {
	if w == nil || dirName == "" {
		return
	}
	if w.Name == dirName && m.Name == dirName {
		return // names already stable — nothing to flag regardless of memory
	}
	// Names disagree: only a problem if a node actually uses per-bot memory.
	// Check that last so the node walk is skipped on the common (stable) path.
	if !usesPerBotMemory(w) {
		return
	}
	*diags = append(*diags, Diag{
		Code:     DiagBundleNameTripleMismatch,
		Severity: SeverityError,
		Field:    "name",
		Message: fmt.Sprintf(
			"per-bot memory (`memory: visibility: bot` or `auto_memory: on`) requires manifest name == workflow name == bundle dir so the memory tree is stable across CLI, studio, dispatcher and cloud launches; got manifest=%q workflow=%q dir=%q",
			m.Name, w.Name, dirName,
		),
		Hint: "make all three identical (rename the bundle dir, the `workflow NAME:`, or the manifest name:)",
	})
}

// checkSkills flags routability problems in a bundle's skills/*.md: a skill
// the router can't discover (no name) or can't decide to select (no/terse
// description), and a name collision that clobbers on mirror. All warnings —
// a skill authoring gap should never fail `iterion validate`. Deliberately no
// prose-style rules: presence + minimal substance + uniqueness only.
func checkSkills(diags *[]Diag, skills []SkillDoc) {
	if len(skills) == 0 {
		return
	}
	// Iterate in Path order for deterministic output ahead of the final sort.
	ordered := append([]SkillDoc(nil), skills...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	firstByName := map[string]string{} // name → first Path that declared it
	for _, s := range ordered {
		name := strings.TrimSpace(s.Name)
		desc := strings.TrimSpace(s.Description)

		if name == "" {
			*diags = append(*diags, Diag{
				Code:     DiagSkillNameMissing,
				Severity: SeverityWarning,
				Field:    s.Path,
				Message:  "skill has no `name:` frontmatter; it is undiscoverable by name once mirrored into .claude/skills/",
				Hint:     "add `name: <kebab-case-id>` to the skill's frontmatter",
			})
		} else if prev, dup := firstByName[name]; dup {
			*diags = append(*diags, Diag{
				Code:     DiagSkillNameDuplicate,
				Severity: SeverityWarning,
				Field:    s.Path,
				Message:  fmt.Sprintf("skill name %q is already declared by %s; one silently clobbers the other when mirrored into .claude/skills/", name, prev),
				Hint:     "give each skill a unique name:",
			})
		} else {
			firstByName[name] = s.Path
		}

		switch {
		case desc == "":
			*diags = append(*diags, Diag{
				Code:     DiagSkillDescriptionMissing,
				Severity: SeverityWarning,
				Field:    s.Path,
				Message:  "skill has no `description:` frontmatter; the router has no signal for when to select it",
				Hint:     "add a `description:` saying what the skill is for and when it applies",
			})
		case len(desc) < minRoutableDescription:
			*diags = append(*diags, Diag{
				Code:     DiagSkillDescriptionTerse,
				Severity: SeverityWarning,
				Field:    s.Path,
				Message:  fmt.Sprintf("skill `description:` (%q) is too short to route on", desc),
				Hint:     "describe what the skill does and the situation it applies to, so the router can choose it",
			})
		}
	}
}

// checkDurableMemoryPromises holds a bundle's skills to what its own nodes
// can do with a DURABLE store (C274): a page naming the memory store's write
// tool, in a bundle where no node can put bytes in a memory tree, teaches
// every reader a contract the bot cannot honour.
//
// It is a REFERENCE check, which is why it lives here and not in checkSkills,
// whose contract forbids prose rules: the page must NAME the tool, and the
// vocabulary is read from toolcatalog rather than listed.
//
// The two halves are asymmetric, and the asymmetry is the question's own:
//
//   - what the page NAMES is the tool of the DURABLE store — the member of
//     toolcatalog.DurableWriteCanonicals the spelling table does not project
//     onto any backend roster, claw's own `memory_write`. `Write` and `bash`
//     name a write to the workspace, which no run keeps, so a page
//     documenting the DSL's `bash -c` recipe, or naming the writers a
//     permission gate DENIES, promises nothing — bots/whats-next ships both,
//     and both must stay quiet.
//   - what a node can SERVE is the whole durable-write vocabulary, shell
//     included: the memory tree is a directory under `$ITERION_HOME`, so a
//     node holding the shell keeps a durable file with no builtin at all —
//     which is exactly what a claude_code recipe once told a bot to do.
//
// The capability is read from the DECLARATION, the surface bundlelint judges,
// and from what the RUNTIME INSTALLS for it rather than from the presence of
// a block or a list entry (iterion#2832) — see anyNodeCanWriteDurably for the
// four arms. It is read GENEROUSLY where another diagnostic owns the defect:
// a `memory:` block on a backend that cannot serve it, and a permission mode
// no gate can parse, are ir's own diagnostics, and a warning speaking over
// them would name the wrong fix.
// bots.TestABundleSellsADurableBriefOnlyWhereItCanWriteOne is the corpus
// twin, which reads the RESOLVED routes an executor computes and asks THIS
// package's gate predicate (PermissionGateLeaves) so the two cannot drift.
//
// The question is asked of ONE workflow, not of a bundle's every entrypoint,
// because a skill is mirrored into the workspace of ONE run: a bundle's
// sibling entry is its own bot, and the page it loads is owed to the nodes
// that will read it there.
func checkDurableMemoryPromises(diags *[]Diag, skills []SkillDoc, w *ir.Workflow) {
	if w == nil || len(skills) == 0 {
		return
	}
	tools := durableMemoryWriteTools()
	// No durable store in the vocabulary, no question to ask.
	if len(tools) == 0 {
		return
	}
	// A bundle with no LLM node has no model reading a skill at all.
	readers := llmNodeIDs(w)
	if len(readers) == 0 {
		return
	}
	if anyNodeCanWriteDurably(w, readers) {
		return
	}
	ordered := append([]SkillDoc(nil), skills...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	for _, s := range ordered {
		named := namedTools(s.Body, tools)
		if len(named) == 0 {
			continue
		}
		*diags = append(*diags, Diag{
			Code:     DiagSkillDurableMemoryUnserved,
			Severity: SeverityWarning,
			Field:    s.Path,
			Message: fmt.Sprintf(
				"the page names %s, but no node of this bundle can put bytes in a durable memory tree: %s",
				strings.Join(quotedNames(named), ", "), describeNodes(w, readers),
			),
			Hint: "give a node a `memory:` block on `backend: \"claw\"` that KEEPS its write tool (not `write: false`, not the instance-wide `visibility: \"global\"`), or `auto_memory: on`, or a `tools:` list naming a writer its permission gate leaves — or scope the page to say this bundle has no memory tree",
		})
	}
}

// durableMemoryWriteTools are the durable-write concepts that name the
// durable STORE itself, derived from the shared vocabulary instead of listed:
// a member toolcatalog.DurableWriteCanonicals carries that its spelling table
// does not know is one no backend roster projects, which is exactly how that
// set describes claw's `memory_write`. A second store builtin added there is
// read here the day it lands.
//
// The projected members are left out of the NAMING half for a measured
// reason, not a tidy one: their names are also words. Read whole, the
// vocabulary fires on 57 of the 194 skill pages this repository ships,
// blaming `write`, `edit` and `bash` — the English verbs, and the DSL's own
// recipe vocabulary in a quick reference that names `run_command` as
// something that is NOT a tool. A store builtin is a tool identifier and
// nothing else, which is what makes a reference check precise here;
// TestAPageNamingAWorkspaceWriterPromisesNoDurableMemory and the catalogue
// gate (bots.TestCatalogBotsBundleConsistencyClean) are what keep it so.
func durableMemoryWriteTools() []string {
	rows := toolcatalog.CanonicalRows()
	var out []string
	for _, canonical := range toolcatalog.DurableWriteCanonicals() {
		if _, projected := rows[canonical]; !projected {
			out = append(out, canonical)
		}
	}
	sort.Strings(out)
	return out
}

// llmNodeIDs are the workflow's LLM node ids, sorted: the nodes a mirrored
// skill reaches, and the only ones that can hold a tool.
func llmNodeIDs(w *ir.Workflow) []string {
	var out []string
	for id, n := range w.Nodes {
		if _, ok := n.(ir.LLMNode); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// anyNodeCanWriteDurably reports whether one node of the workflow could put
// bytes in a durable memory tree. Four declarations are a path, and each is a
// different question:
//
//   - a `memory:` block whose WRITE TOOL a run gets — the block is not the
//     path, `memory_write` is (ir.Memory.InstallsWriteTool, the reading
//     model.installWorkspaceMemory performs). The backend is NOT asked: a
//     block on a backend that cannot serve it is ir's own diagnostic (C047),
//     and a warning speaking over another's defect would name the wrong fix;
//   - `auto_memory: on` — per-bot memory too, maintained with ORDINARY FILE
//     TOOLS the executor appends to a restricted claw list and folded back
//     into the store by the mirror, so the node keeps a durable tree with no
//     store builtin at all;
//   - an UNDECLARED `tools:` list is not an empty one — on the CLI backends
//     it means "no restriction", so the native roster keeps the shell and the
//     writers, while `tools: []` IS a declaration. The GATE still bounds that
//     roster, so it is asked about the roster rather than credited for the
//     absence of a list;
//   - a declared tool of the durable-write vocabulary its permission gate
//     leaves.
//
// Every arm reads what the RUNTIME INSTALLS, never the mere presence of a
// block or a list entry: that reading is what let `write: false`,
// `visibility: "global"` and a `permission: deny` gate silence the warning on
// bundles with no write path at all (iterion#2832).
func anyNodeCanWriteDurably(w *ir.Workflow, ids []string) bool {
	for _, id := range ids {
		llm, ok := w.Nodes[id].(ir.LLMNode)
		if !ok {
			continue
		}
		if llm.GetMemory().InstallsWriteTool() {
			return true
		}
		if autoMemoryServesNode(llm, w) {
			return true
		}
		if !toolcatalog.ToolsDeclared(llm.GetTools()) {
			if nativeDurableWriterLeft(llm, w) != "" {
				return true
			}
			continue
		}
		if declaredDurableWriter(llm, w) != "" {
			return true
		}
	}
	return false
}

// autoMemoryServesNode reports whether a node runs with auto-memory on a
// backend that serves it — the two questions the engine asks
// (model.ClawExecutor.autoMemoryMode and automemory.SupportsBackend), read
// from the declaration.
//
// The mode is resolved by the SHARED resolver, so the node's own value wins
// over the workflow's exactly as it does at dispatch; the run override and
// the env default are not the declaration's to know, and are left empty.
// An effective backend of "" is read generously: the resolver falls through
// to env and host credentials, so the declaration genuinely cannot say — the
// same reading C132 performs.
func autoMemoryServesNode(llm ir.LLMNode, w *ir.Workflow) bool {
	if !automemory.Resolve("", llm.GetAutoMemory(), w.AutoMemory, "").Enabled() {
		return false
	}
	backend := ir.SourceNodeBackendName(llm.GetLLMFields().Backend, w.DefaultBackend)
	return backend == "" || automemory.SupportsBackend(backend)
}

// nativeDurableWriterLeft returns the first durable writer an undeclared
// `tools:` list keeps through the gate, or "" when the gate removes them all.
//
// The roster is the shared vocabulary minus the store's own builtins (a CLI
// backend has no `memory_write`), asked of the SAME gate predicate a declared
// list is asked of: a node under `permission: deny` whose allow list names no
// writer runs none of them, however complete its native roster is.
func nativeDurableWriterLeft(llm ir.LLMNode, w *ir.Workflow) string {
	store := durableMemoryWriteTools()
	for _, canonical := range toolcatalog.DurableWriteCanonicals() {
		if slices.Contains(store, canonical) {
			continue
		}
		if PermissionGateLeaves(llm, w, canonical) {
			return canonical
		}
	}
	return ""
}

// declaredDurableWriter returns the first tool a node declares that could
// write a durable file, or "" for none. The vocabulary is the shared set —
// never a second list — folded through the spelling table, so `write_file`
// and `apply_patch` count exactly as `write` and `edit` do.
//
// The store's OWN builtins are excluded, and the exclusion is the heart of
// the question: a `memory:` block installs them, a `tools:` entry cannot —
// naming one there asks claw for a tool it does not have (C135). A bundle
// that read the skill page and answered it with `tools: [memory_write]` is
// precisely the confusion this diagnostic exists to name, so reading that
// list as a path would silence the warning on its own best example. Which
// members are the store's is the SAME derivation the naming half reads, from
// the same chokepoint.
func declaredDurableWriter(llm ir.LLMNode, w *ir.Workflow) string {
	writers := toolcatalog.DurableWriteCanonicals()
	store := durableMemoryWriteTools()
	for _, tool := range llm.GetTools() {
		canonical := toolcatalog.CanonicalToolName(tool)
		if !slices.Contains(writers, canonical) || slices.Contains(store, canonical) {
			continue
		}
		if !PermissionGateLeaves(llm, w, tool) {
			continue
		}
		return tool
	}
	return ""
}

// PermissionGateLeaves reports whether the runtime permission gate lets a tool
// the declaration granted RUN. The gate bounds what runs, independently of
// what exists, so every reader asking "can this node reach a writer" must ask
// it — bundlelint's durable-memory check and its corpus twin
// (bots.TestABundleSellsADurableBriefOnlyWhereItCanWriteOne) read THIS
// predicate, exported so the two cannot drift.
//
// The effective MODE decides which question the rule lists answer
// (pkg/backend/permission.Policy.Evaluate):
//
//   - off, or unset: the gate is disabled and the lists are inert — read as
//     "left", never guessed;
//   - a mode no gate can parse is C110's error; a warning that read it as a
//     bound would name the wrong fix, so it is read as off;
//   - `ask`: everything no rule denies pauses for the human, who may approve,
//     so the capability EXISTS. Only a rule bounding every call of the tool
//     removes it — a bare row, or a scope that leaves no call out
//     (`Write(**)`); `Write(docs/**)` bounds a subset, and the node writes
//     everywhere else;
//   - `deny`: the mode HARD-DENIES everything no allow row matches, so a
//     writer is left only where an allow row — or an ask row, which a deny
//     mode still pauses on (Policy.CanAsk) — covers the tool at all. A scoped
//     allow is enough: some calls pass, so the capability exists.
//
// Rules are read with the grammar that ENFORCES them (permission.RuleCovers /
// RuleRemovesTool), never a second spelling here: a class row (`class:shell`)
// and a tool glob bind exactly as they do at dispatch.
// A nil workflow is the node alone: every list falls back to what the node
// declares, the way ir.EffectiveAskRules reads one.
func PermissionGateLeaves(llm ir.LLMNode, w *ir.Workflow, tool string) bool {
	var wfMode string
	var wfAllow, wfDeny []string
	if w != nil {
		wfMode, wfAllow, wfDeny = w.Permission, w.PermissionAllow, w.PermissionDeny
	}
	raw := llm.GetPermission()
	if raw == "" {
		raw = wfMode
	}
	mode, err := permission.ParseMode(raw)
	if err != nil || mode == permission.ModeOff {
		return true
	}
	for _, rule := range ir.EffectivePermissionRules(llm.GetPermissionDeny(), wfDeny) {
		if permission.RuleRemovesTool(rule, tool) {
			return false
		}
	}
	if mode != permission.ModeDeny {
		return true
	}
	granted := append(
		ir.EffectivePermissionRules(llm.GetPermissionAllow(), wfAllow),
		ir.EffectiveAskRules(llm, w)...,
	)
	for _, rule := range granted {
		if permission.RuleCovers(rule, tool) {
			return true
		}
	}
	return false
}

// namedTools are the tools a page NAMES, in the order given, each at most
// once.
func namedTools(body string, tools []string) []string {
	if body == "" {
		return nil
	}
	lower := strings.ToLower(body)
	var out []string
	for _, tool := range tools {
		if namesTool(lower, strings.ToLower(tool)) {
			out = append(out, tool)
		}
	}
	return out
}

// namesTool reports whether a (lower-cased) page names a tool AS the tool:
// the token bounded by characters a tool identifier cannot carry, so
// `memory_write` and `memory_write(path, content)` both count while
// `memory_writer` and `team_memory_write` do not.
func namesTool(lowerBody, tool string) bool {
	for i := 0; i+len(tool) <= len(lowerBody); {
		j := strings.Index(lowerBody[i:], tool)
		if j < 0 {
			return false
		}
		at := i + j
		beforeOK := at == 0 || !identByte(lowerBody[at-1])
		end := at + len(tool)
		afterOK := end == len(lowerBody) || !identByte(lowerBody[end])
		if beforeOK && afterOK {
			return true
		}
		i = at + 1
	}
	return false
}

// identByte reports whether a byte can be part of a tool identifier.
func identByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// describeNodes says what each reading node declares, so the fix is read off
// the message instead of hunted in the .bot. Bounded: past the fourth node
// the list says how many more there are.
func describeNodes(w *ir.Workflow, ids []string) string {
	const shown = 4
	var parts []string
	for i, id := range ids {
		if i == shown {
			parts = append(parts, fmt.Sprintf("+%d more", len(ids)-shown))
			break
		}
		llm, ok := w.Nodes[id].(ir.LLMNode)
		if !ok {
			continue
		}
		backend := llm.GetLLMFields().Backend
		if backend == "" {
			backend = "the workflow default"
		}
		declared := "no `tools:` list"
		if toolcatalog.ToolsDeclared(llm.GetTools()) {
			declared = fmt.Sprintf("tools: [%s]", strings.Join(llm.GetTools(), ", "))
		}
		// Read, never assumed: a node reaches this message only where its
		// block installs no write tool, and saying WHICH declaration
		// withheld it is what makes the fix readable off the message —
		// `write: false` and the instance-wide `global` space are both
		// doors C319 sends an author through.
		block := "carries no `memory:` block"
		if mem := llm.GetMemory(); mem != nil {
			block = fmt.Sprintf("carries a `memory:` block that installs no write tool (write: %t, visibility: %q)", mem.Write, mem.Visibility)
		}
		parts = append(parts, fmt.Sprintf("node %q (backend %s) declares %s and %s", id, backend, declared, block))
	}
	return strings.Join(parts, "; ")
}

// quotedNames renders names for a message, each in Go quotes.
func quotedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}

// usesPerBotMemory reports whether any node opts into per-bot memory.
func usesPerBotMemory(w *ir.Workflow) bool {
	// `auto_memory:` is per-bot memory too — one space per (bot, repo), which
	// the run resolves from whichever name the launching surface happens to
	// know: the manifest's, the bundle directory's, or the workflow's. They
	// only ever refer to one space while the three agree, which is exactly
	// what this check exists to require. Without it a bot could carry
	// auto-memory with three divergent names and quietly keep three memories,
	// while a bot using the `memory:` block was protected.
	if strings.EqualFold(strings.TrimSpace(w.AutoMemory), "on") {
		return true
	}
	for _, n := range w.Nodes {
		ln, ok := n.(ir.LLMNode)
		if !ok {
			continue
		}
		if mem := ln.GetMemory(); mem != nil && mem.Visibility == "bot" {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(ln.GetAutoMemory()), "on") {
			return true
		}
	}
	return false
}

// sameStringSet reports whether a and b contain the same set of strings,
// order-insensitive.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, s := range a {
		set[s]++
	}
	for _, s := range b {
		set[s]--
		if set[s] < 0 {
			return false
		}
	}
	return true
}

// checkEngineRequirement holds the manifest's declared engine floor against
// the build validating it (C250/C251).
//
// It is the LOCAL half of a guard that also lives at the push admission
// (pkg/server) and at the launch (pkg/runner) — three surfaces, one predicate
// in pkg/bundle, so an author, an operator and a pod cannot read the same
// manifest three different ways.
// checkSyntaxFloor holds the manifest's engine floor against what the
// bundle asks for (C252): a profile above 1, `import`, a `contract`, a Claw
// tool alias, or a key the manifest itself declares (`category`/`tags`)
// needs a declared `requires.iterion` at or above the release that reads it
// (bundle.CheckSyntaxFloor, the predicate the push admission shares) — a
// floor declared but lower leaves every runner between the two admitting a
// bundle it cannot read. The remedy names that release, or this build when
// the release has none on record.
//
// What asks for the floor and what a build below it does are read from the
// verdict (ProfileFloor.Uses, .Consequence), which carries them from the
// one floors table: a floor added there is named here without an arm of
// its own, and cannot inherit a consequence that is false for it.
func checkSyntaxFloor(diags *[]Diag, m *bundle.Manifest, req bundle.SyntaxRequirements, build string) {
	pf := bundle.CheckSyntaxFloor(m, req)
	if pf.OK {
		return
	}
	floor := pf.Need
	if floor == "" {
		floor = "<the release that reads " + pf.Reason + ">"
		if v := strings.TrimPrefix(strings.SplitN(build, "+", 2)[0], "v"); v != "" {
			if _, ok := bundle.CompareVersions(v, "0"); ok {
				floor = v
			}
		}
	}
	uses, consequence := pf.Uses, pf.Consequence
	msg := fmt.Sprintf("the bundle uses %s but declares no engine floor: %s", uses, consequence)
	hint := fmt.Sprintf("declare `requires: { iterion: \">= %s\" }` in the manifest, so such a runner refuses the bundle at admission instead", floor)
	if pf.Declared != "" {
		msg = fmt.Sprintf("the bundle uses %s but requires.iterion %q does not reach %s, the release that reads it: %s", uses, pf.Declared, floor, consequence)
		hint = fmt.Sprintf("raise it to `requires: { iterion: \">= %s\" }`", floor)
	}
	*diags = append(*diags, Diag{Code: DiagProfileNeedsFloor, Severity: SeverityWarning, Field: "requires.iterion", Message: msg, Hint: hint})
}

// checkProfileUnread names the executable sources whose syntax profile
// could not be read (C253) — a subbot child in a sibling bundle or at an
// absolute path, a root entry or a child that is a link out of the
// collection — so a profile of 1 is never taken for "checked" when part of
// the executable sources was not.
func checkProfileUnread(diags *[]Diag, unread []string) {
	if len(unread) == 0 {
		return
	}
	*diags = append(*diags, Diag{
		Code: DiagProfileChildUnread, Severity: SeverityWarning,
		Field:   "requires.iterion",
		Message: fmt.Sprintf("executable source(s) not read for their syntax profile, resolving beyond the bundle's collection: %s", strings.Join(unread, ", ")),
		Hint:    "a source written in a newer profile than a runner reads fails at that runner's parse: declare `requires.iterion` for the newest profile among them, or make the source resolve inside the bundle",
	})
}

// checkProfileShort reports the executable sources whose unit a bound of
// the reader cut short (C256): the floor walk reads no file past the cut,
// so the syntax those files use is not known.
func checkProfileShort(diags *[]Diag, short []string) {
	if len(short) == 0 {
		return
	}
	*diags = append(*diags, Diag{
		Code: DiagProfileSourceShort, Severity: SeverityError,
		Field:   "requires.iterion",
		Message: fmt.Sprintf("executable source(s) not read whole, so the syntax they use is not known: %s", strings.Join(short, "; ")),
		Hint:    "raise " + parser.ReadBudgetEnv + " wherever the bot is read (the CLI, the server, the runner) — a bot's files share the budget, so splitting is no way past it — or shrink the source; the push admission refuses the bundle until it reads whole",
	})
}

func checkEngineRequirement(diags *[]Diag, m *bundle.Manifest, build string) {
	if strings.TrimSpace(build) == "" {
		return
	}
	verdict, reason := bundle.CheckManifestEngine(m, build)
	switch verdict {
	case bundle.EngineTooOld:
		*diags = append(*diags, Diag{
			Code: DiagEngineRequirementUnmet, Severity: SeverityError,
			Field: "requires.iterion", Message: reason,
			Hint: "upgrade iterion, or lower requires.iterion to a build that carries what the bot uses",
		})
	case bundle.EngineUnknown:
		*diags = append(*diags, Diag{
			Code: DiagEngineRequirementUnchecked, Severity: SeverityWarning,
			Field: "requires.iterion", Message: reason,
			Hint: "validate with a released build (or one built through `task build`, which injects the version) to check the requirement",
		})
	}
}

// checkHostEventMode cross-checks the two halves of a host-event gate: the
// manifest field the host writes into, and the DSL mode that declares the
// gate accepts it at all.
//
// A gate is only usable when both are present, but the two absences are not
// equally bad. A field with no mode WORKS — that is how the surface shipped,
// before the mode existed — so it is a warning nudging the author to make the
// capability visible in the .bot. A mode with no field is DEAD: the run parks
// on a gate advertising a standby, and nothing will ever be able to wake it.
func checkHostEventMode(diags *[]Diag, node ir.Node, decl bundle.ChatNode, id, fieldBase string) {
	if decl.Kind != bundle.ChatNodeHuman {
		return
	}
	declaresMode := ir.NodeInteraction(node) == ir.InteractionHumanOrHost
	switch {
	case declaresMode && decl.HostEventField == "":
		*diags = append(*diags, Diag{
			Code: DiagChatHostEventModeMismatch, Severity: SeverityError, Field: fieldBase + ".host_event_field",
			Message: fmt.Sprintf("workflow node %q declares interaction: human_or_host but the manifest gives it no host_event_field — nothing can ever deliver a host event to this gate", id),
			Hint:    "add host_event_field to the chat node (its output schema needs a matching json field), or drop interaction: human_or_host",
		})
	case !declaresMode && decl.HostEventField != "":
		*diags = append(*diags, Diag{
			Code: DiagChatHostEventModeMismatch, Severity: SeverityWarning, Field: fieldBase + ".host_event_field",
			Message: fmt.Sprintf("the manifest declares host_event_field on %q but the workflow node does not declare interaction: human_or_host — the gate works, but reading main.bot gives no hint that anything other than the operator can resume it", id),
			Hint:    "set interaction: human_or_host on the human node so the capability is visible in the graph",
		})
	}
}
