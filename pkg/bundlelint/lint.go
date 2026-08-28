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
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
}

// SkillDoc is one bundle skill file's routability-relevant frontmatter. Path
// is the bundle-relative path used for diagnostic attribution (e.g.
// "skills/repo-survey.md").
type SkillDoc struct {
	Path        string
	Name        string
	Description string
}

// CheckConsistency cross-checks a bot's manifest against its compiled
// workflow. A nil Manifest skips all manifest-side checks; a nil Workflow
// disables the checks that resolve names against the workflow (var-map,
// forge-secret, capability checks). Returned diagnostics are deterministically
// ordered by (Code, Field).
func CheckConsistency(in Input) []Diag {
	m := in.Manifest
	if m == nil {
		return nil
	}
	var diags []Diag
	checkVarMaps(&diags, m, in.Workflow)
	checkChatSurface(&diags, m, in.Workflow)
	checkForgeSecret(&diags, m, in.Workflow)
	checkCapabilities(&diags, m, in.Workflow, in.Frontmatter)
	checkBundleNameStability(&diags, m, in.Workflow, in.DirName)
	checkSkills(&diags, in.Skills)

	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Field < diags[j].Field
	})
	return diags
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
