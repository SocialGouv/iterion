package botscaffold

import (
	"fmt"
	"strings"
)

// DefaultTemplateID is the gallery entry used when a caller names none.
const DefaultTemplateID = "blank"

// Overrides are the fields a caller may set on top of a template's Spec.
// An empty string or a nil pointer keeps the template's own value, so a
// caller only states what the operator actually asked for.
type Overrides struct {
	Slug         string
	DisplayName  string
	Description  string
	Instructions string
	Model        string
	Backend      string
	Worktree     *bool
	Sandbox      *bool
}

// TemplateByID looks up one gallery entry.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Templates() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// TemplateIDs lists the gallery's entry IDs, in display order.
func TemplateIDs() []string {
	tpls := Templates()
	ids := make([]string, 0, len(tpls))
	for _, t := range tpls {
		ids = append(ids, t.ID)
	}
	return ids
}

// SpecFromTemplate resolves a gallery template and applies overrides on
// top. It lives here rather than in a single caller so every creation
// surface (the CLI, and any future server-side template endpoint) shares
// one definition of "start from a template" — the studio currently does
// this merge in TypeScript, which is exactly the drift this prevents.
func SpecFromTemplate(id string, ov Overrides) (Spec, error) {
	if strings.TrimSpace(id) == "" {
		id = DefaultTemplateID
	}
	tpl, ok := TemplateByID(strings.TrimSpace(id))
	if !ok {
		return Spec{}, fmt.Errorf("botscaffold: unknown template %q (available: %s)",
			id, strings.Join(TemplateIDs(), ", "))
	}

	spec := tpl.Spec
	overrideString(&spec.Slug, ov.Slug)
	overrideString(&spec.DisplayName, ov.DisplayName)
	overrideString(&spec.Description, ov.Description)
	overrideString(&spec.Instructions, ov.Instructions)
	overrideString(&spec.Model, ov.Model)
	overrideString(&spec.Backend, ov.Backend)
	if ov.Worktree != nil {
		spec.Worktree = *ov.Worktree
	}
	if ov.Sandbox != nil {
		spec.Sandbox = *ov.Sandbox
	}
	return spec, nil
}

func overrideString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// Template is one entry of the builder's "start from a template"
// gallery. Everything is a pre-filled Spec fragment — the form stays
// fully editable after picking one.
type Template struct {
	ID          string `json:"id"`
	Icon        string `json:"icon"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Spec is the pre-filled builder form (Slug left empty — the user
	// names their bot).
	Spec Spec `json:"spec"`
}

// Templates returns the curated gallery, "blank" first. The slice is
// freshly built per call so callers can't mutate the catalog.
func Templates() []Template {
	return []Template{
		{
			ID:          "blank",
			Icon:        "📝",
			Name:        "Blank bot",
			Description: "Start from scratch: one agent, your instructions.",
			Spec: Spec{
				Instructions: "You are a helpful engineering agent. Describe your mission here:\nwhat to investigate, what to produce, and how to verify it before\nreporting mission_complete=true.",
			},
		},
		{
			ID:          "daily-digest",
			Icon:        "📰",
			Name:        "Daily digest",
			Description: "Summarize what changed in the repo since yesterday, on a schedule.",
			Spec: Spec{
				Description:  "Posts a daily summary of repository activity.",
				WhenToUse:    "Use for a recurring, read-only digest of recent repository activity.",
				Instructions: "Produce a concise digest of what changed in this repository over the\nlast {{vars.window}}: read `git log --since` (commits, authors, themes),\nnotable diffs, and any TODO/FIXME newly introduced. Group by theme,\nlead with the most impactful change, keep it under one screen. Write\nthe digest to {{vars.report_path}} and include it in your summary.",
				Vars: []VarSpec{
					{Name: "window", Type: "string", Default: "24 hours", Description: "Lookback window handed to git log --since."},
					{Name: "report_path", Type: "string", Default: "digest.md", Description: "Where the digest markdown lands (relative to the workspace)."},
				},
				ScheduleCron: "0 7 * * 1-5",
			},
		},
		{
			ID:          "code-reviewer",
			Icon:        "🔍",
			Name:        "Code reviewer",
			Description: "Review the current branch's diff for bugs and risky changes.",
			Spec: Spec{
				Description:  "Reviews the working tree / branch diff and reports findings.",
				WhenToUse:    "Use to review pending changes before a merge — read-only, no fixes.",
				Instructions: "Review this repository's pending changes for CORRECTNESS: run\n`git add -N . && git diff HEAD` (or `git diff {{vars.base}}` when a base\nis given) and hunt for real bugs — logic errors, missed edge cases,\nsecurity issues, races. Read enough surrounding code to judge each\nfinding; discard style nits. Report each confirmed finding with\nfile:line, the failure scenario, and a suggested fix. Do NOT edit any\nfile: you are read-only.",
				Vars: []VarSpec{
					{Name: "base", Type: "string", Default: "", Description: "Optional base ref to diff against (empty = working tree vs HEAD)."},
				},
			},
		},
		{
			ID:          "docs-writer",
			Icon:        "📚",
			Name:        "Docs writer",
			Description: "Write or refresh documentation for a part of the codebase.",
			Spec: Spec{
				Description:  "Writes documentation grounded in the actual code.",
				WhenToUse:    "Use to document a module or align an existing doc with the code.",
				Instructions: "Document {{vars.target}} in this repository. Read the code first —\nevery claim must be grounded in what the code actually does today, not\nwhat it should do. Follow the repo's existing doc style and location\nconventions. Docs follow code: never edit source to match the doc; when\nyou find a genuine code bug, note it in your summary instead. Commit\nyour doc changes with a semantic `docs(scope):` message.",
				Vars: []VarSpec{
					{Name: "target", Type: "string", Default: "the public API", Description: "What to document (module, package, feature…)."},
				},
				Worktree: true,
			},
		},
		{
			ID:          "issue-triager",
			Icon:        "🏷️",
			Name:        "Issue triager",
			Description: "Triage new board cards: label, prioritize, route to the right bot.",
			Spec: Spec{
				Description:  "Triages inbox cards on the native board.",
				WhenToUse:    "Use to keep the board inbox triaged (labels, priority, routing).",
				Instructions: "Triage the untriaged cards on this workspace's native board: read each\ncard in {{vars.inbox_state}}, add the labels that fit the workspace's\nlabel vocabulary, set a sensible priority, and when a card is clearly\nactionable by a catalog bot, assign that bot. Leave a one-line comment\nexplaining each triage decision. Do not close or delete cards.",
				Vars: []VarSpec{
					{Name: "inbox_state", Type: "string", Default: "inbox", Description: "Board state holding untriaged cards."},
				},
				Capabilities: []string{"board.read", "board.label", "board.assign", "board.comment", "board.move"},
			},
		},
	}
}
