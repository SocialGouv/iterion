// Package botscaffold renders a new bot bundle (main.bot +
// manifest.yaml + README.md + the bundle layout directories) from a
// builder Spec. It is the single engine behind both bot-creation
// surfaces — the studio's "New bot" flow and `iterion bots create` — and
// is deliberately server-importable (pkg/cli wraps it too; the server
// must not import pkg/cli).
//
// The generated workflow follows the house v2 shape: ONE adaptive agent
// carrying the whole mission (see docs/workflow_authoring_pitfalls.md —
// over-framing is an anti-pattern), with worktree/sandbox/permission/
// budget as opt-in workflow-level dials. Rendered output is never
// trusted: main.bot is parsed AND compiled before anything is written,
// and manifest.yaml is decoded through the same strict loader the
// runtime uses.
package botscaffold

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed templates/preset_example.md
var presetExample []byte

// bundleDirs is the .botz layout every scaffolded bot ships. They are
// created empty (with a .gitkeep so git carries them) because the bundle
// loader resolves each by convention: skills/ mirrors into the run
// workspace's .claude/skills/, prompts/<stem>.md becomes a named prompt,
// attachments/ holds default binary inputs, presets/ holds sous-bots.
var bundleDirs = []string{"skills", "prompts", "attachments", "presets"}

// bundleGitignore keeps local build output out of the author's repo. It
// mirrors what the pack walker already filters.
const bundleGitignore = "*.botz\n.iterion/\n"

// SlugRe is the accepted shape for a new bot's directory/technical name.
var SlugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// VarSpec declares one workflow input var for the generated bot.
type VarSpec struct {
	Name string `json:"name"`
	// Type is one of the DSL literal kinds: string, int, bool, float.
	Type string `json:"type"`
	// Default is the raw default value; empty means the type's zero
	// value ("", 0, false, 0.0) so the rendered declaration is always
	// complete and the bot always launches without mandatory inputs.
	Default string `json:"default,omitempty"`
	// Description becomes a `##` comment above the declaration.
	Description string `json:"description,omitempty"`
}

var knownVarTypes = map[string]bool{"string": true, "int": true, "bool": true, "float": true}

var varNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Spec is everything the builder form collects.
type Spec struct {
	// Slug is the bundle directory + technical name (SlugRe).
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	WhenToUse   string `json:"when_to_use,omitempty"`

	// Instructions is the mission of the single campaign agent — the
	// bot's system prompt body.
	Instructions string `json:"instructions"`

	// Model/Backend override the engine's auto-detection when set.
	Model   string `json:"model,omitempty"`
	Backend string `json:"backend,omitempty"`

	// Skills are library skill names attached to the agent (DSL
	// `skills:` field, resolved by pkg/skilllib at run time).
	Skills []string `json:"skills,omitempty"`

	// Capabilities are host capabilities granted to the agent (DSL
	// `capabilities:` field, e.g. board.read / board.move).
	Capabilities []string `json:"capabilities,omitempty"`

	Vars []VarSpec `json:"vars,omitempty"`

	// Advanced dials — all optional.
	Worktree   bool   `json:"worktree,omitempty"`
	Sandbox    bool   `json:"sandbox,omitempty"`
	Permission string `json:"permission,omitempty"` // "", "ask", "deny"

	MaxCostUSD  float64 `json:"max_cost_usd,omitempty"`
	MaxDuration string  `json:"max_duration,omitempty"` // Go duration string, e.g. "2h"

	// ScheduleCron adds a kind=schedule invocation with this
	// suggested_cron to the manifest so the bot home can offer a
	// one-click schedule trigger.
	ScheduleCron string `json:"schedule_cron,omitempty"`
}

// WorkflowName is the Slug as a DSL identifier — the DSL grammar has no
// hyphens in idents, so kebab-case slugs become snake_case here.
func (s Spec) WorkflowName() string {
	return strings.ReplaceAll(s.Slug, "-", "_")
}

// Result reports what Scaffold wrote.
type Result struct {
	Dir   string   `json:"dir"`
	Files []string `json:"files"`
}

var knownPermissions = map[string]bool{"": true, "ask": true, "deny": true}

// Validate rejects a malformed Spec with an explicit, field-naming error.
func (s *Spec) Validate() error {
	if !SlugRe.MatchString(s.Slug) {
		return fmt.Errorf("botscaffold: invalid slug %q (want %s)", s.Slug, SlugRe)
	}
	if strings.TrimSpace(s.Instructions) == "" {
		return fmt.Errorf("botscaffold: instructions must not be empty")
	}
	if !knownPermissions[s.Permission] {
		return fmt.Errorf("botscaffold: invalid permission %q (want ask or deny)", s.Permission)
	}
	seen := map[string]bool{}
	for i, v := range s.Vars {
		if !varNameRe.MatchString(v.Name) {
			return fmt.Errorf("botscaffold: vars[%d]: invalid name %q (want %s)", i, v.Name, varNameRe)
		}
		if seen[v.Name] {
			return fmt.Errorf("botscaffold: vars[%d]: duplicate name %q", i, v.Name)
		}
		seen[v.Name] = true
		if !knownVarTypes[v.Type] {
			return fmt.Errorf("botscaffold: vars[%d] %q: invalid type %q (want string, int, bool, or float)", i, v.Name, v.Type)
		}
		if v.Default != "" {
			if err := checkVarDefault(v.Type, v.Default); err != nil {
				return fmt.Errorf("botscaffold: vars[%d] %q: %w", i, v.Name, err)
			}
		}
	}
	for i, sk := range s.Skills {
		if strings.TrimSpace(sk) == "" {
			return fmt.Errorf("botscaffold: skills[%d] is empty", i)
		}
	}
	if s.ScheduleCron != "" {
		if fields := strings.Fields(s.ScheduleCron); len(fields) != 5 {
			return fmt.Errorf("botscaffold: schedule_cron %q must be a 5-field cron expression", s.ScheduleCron)
		}
	}
	return nil
}

func checkVarDefault(typ, def string) error {
	switch typ {
	case "int":
		if _, err := strconv.ParseInt(def, 10, 64); err != nil {
			return fmt.Errorf("default %q is not an int", def)
		}
	case "bool":
		if def != "true" && def != "false" {
			return fmt.Errorf("default %q is not a bool (want true or false)", def)
		}
	case "float":
		if _, err := strconv.ParseFloat(def, 64); err != nil {
			return fmt.Errorf("default %q is not a float", def)
		}
	}
	return nil
}

// Scaffold renders the bundle into dir (created; must not already
// contain a main.bot). The rendered main.bot is parsed and compiled and
// the manifest decoded BEFORE any file lands, so a generation bug can
// never write a broken bundle.
func Scaffold(dir string, s Spec) (Result, error) {
	if err := s.Validate(); err != nil {
		return Result{}, err
	}

	mainBot, err := renderMainBot(s)
	if err != nil {
		return Result{}, err
	}
	// Parse + compile the generated workflow with the runtime's own
	// pipeline; any error-severity diagnostic aborts the scaffold.
	pr := parser.Parse(s.Slug+"/main.bot", mainBot)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return Result{}, fmt.Errorf("botscaffold: generated main.bot does not parse: %s", d.Error())
		}
	}
	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return Result{}, fmt.Errorf("botscaffold: generated main.bot has no workflow")
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity != ir.SeverityError {
			continue
		}
		// C018 (no model/backend and no auto-detectable credential) is an
		// ENVIRONMENT verdict, not a scaffolding defect: the zero-config
		// template deliberately omits model/backend so a run auto-detects
		// the host's credential. A credential-less env (CI, a fresh
		// machine) would otherwise make Scaffold fail even though the
		// generated bot is structurally sound — the missing credential is
		// surfaced later at run/validate time, not here.
		if d.Code == ir.DiagMissingModelOrBackend {
			continue
		}
		return Result{}, fmt.Errorf("botscaffold: generated main.bot does not compile: %s", d.Error())
	}

	manifest, err := renderTemplate("manifest.yaml.tmpl", s)
	if err != nil {
		return Result{}, err
	}
	if _, err := bundle.DecodeManifest(manifest, s.Slug+"/manifest.yaml"); err != nil {
		return Result{}, fmt.Errorf("botscaffold: generated manifest invalid: %w", err)
	}

	readme, err := renderTemplate("README.md.tmpl", s)
	if err != nil {
		return Result{}, err
	}

	if _, err := os.Stat(filepath.Join(dir, "main.bot")); err == nil {
		return Result{}, fmt.Errorf("botscaffold: %s already contains a main.bot", dir)
	}
	for _, sub := range bundleDirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return Result{}, fmt.Errorf("botscaffold: mkdir %s: %w", sub, err)
		}
	}

	files := map[string][]byte{
		"main.bot":      []byte(mainBot),
		"manifest.yaml": manifest,
		"README.md":     readme,
		".gitignore":    []byte(bundleGitignore),
		// A starter sous-bot so authors see the preset format.
		filepath.Join("presets", "example.md"): presetExample,
	}
	order := []string{"main.bot", "manifest.yaml", "README.md", ".gitignore", filepath.Join("presets", "example.md")}
	// .gitkeep makes the empty layout dirs survive `git add`; presets/
	// ships example.md instead, so it needs none.
	for _, sub := range bundleDirs {
		if sub == "presets" {
			continue
		}
		name := filepath.Join(sub, ".gitkeep")
		files[name] = []byte{}
		order = append(order, name)
	}

	res := Result{Dir: dir}
	for _, name := range order {
		path := filepath.Join(dir, name)
		if err := store.WriteFileAtomic(path, files[name], 0o644); err != nil {
			return Result{}, fmt.Errorf("botscaffold: write %s: %w", path, err)
		}
		res.Files = append(res.Files, path)
	}
	return res, nil
}

func renderMainBot(s Spec) (string, error) {
	out, err := renderTemplate("main.bot.tmpl", s)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func renderTemplate(name string, s Spec) ([]byte, error) {
	tmpl, err := template.New(name).Funcs(template.FuncMap{
		"quote":    strconv.Quote,
		"indent":   indentLines,
		"varValue": varValue,
	}).ParseFS(templateFS, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("botscaffold: parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, s); err != nil {
		return nil, fmt.Errorf("botscaffold: render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// indentLines prefixes every line of body with n spaces — used to place
// free text (instructions, descriptions) inside an indented DSL block.
func indentLines(n int, body string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// varValue renders a VarSpec's default as a DSL literal of its type,
// substituting the type's zero value when the default is empty.
func varValue(v VarSpec) string {
	def := v.Default
	switch v.Type {
	case "string":
		return strconv.Quote(def)
	case "int":
		if def == "" {
			return "0"
		}
		return def
	case "bool":
		if def == "" {
			return "false"
		}
		return def
	case "float":
		if def == "" {
			return "0.0"
		}
		return def
	}
	// Unreachable after Validate; keep the value visible if it ever is.
	return strconv.Quote(def)
}
