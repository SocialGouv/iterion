// Package botscaffold renders a new bot bundle (main.bot +
// manifest.yaml + README.md + the bundle layout directories) from a
// builder Spec. It is the single engine behind both bot-creation
// surfaces — the studio's "New bot" flow and `iterion bots create` — and
// is deliberately server-importable (pkg/cli wraps it too; the server
// must not import pkg/cli).
//
// By default the generated workflow follows the house v2 shape: ONE
// adaptive agent carrying the whole mission (see
// docs/workflow_authoring_pitfalls.md — over-framing is an anti-pattern),
// with worktree/sandbox/permission/budget as opt-in workflow-level dials.
// A Spec.Shape renders one of the gallery's shapes instead (shapes.go): a
// complete, commented workflow of a form the catalog bots are made of,
// with the annex files the shape ships. Rendered output is never trusted:
// every .bot is parsed AND compiled before anything is written, and
// manifest.yaml is decoded through the same strict loader the runtime
// uses.
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
	"github.com/SocialGouv/iterion/pkg/store"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed templates/preset_example.md
var presetExample []byte

// bundleGitignore keeps local build output out of the author's repo.
// Every entry must be something the packer already excludes — asserted
// by TestBundleGitignore_MatchesPackerSkips rather than by comment, so
// the two cannot drift silently.
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

	// Shape selects the GRAPH the bundle is rendered from: empty is the
	// single-agent workflow of templates/main.bot.tmpl; a shape id names
	// a gallery shape under templates/gallery/<shape>/ — a complete,
	// commented workflow (a bounded campaign loop, a reviewer fan-out, a
	// verified action, …) whose main.bot and annex files (a child
	// worker.bot, prompts/*.md, skills/*.md) are rendered from the same
	// Spec. The list is Shapes(); the studio and the CLI pass it through
	// from the template they started from.
	Shape string `json:"shape,omitempty"`
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

// Validate normalizes the Spec in place, then rejects a malformed one
// with an explicit, field-naming error. Normalizing here (rather than on
// each surface) is what keeps the CLI and the studio from drifting: a
// caller that forgets to trim would otherwise get a baffling
// `invalid slug " foo"`.
func (s *Spec) Validate() error {
	s.Slug = strings.TrimSpace(s.Slug)
	if !SlugRe.MatchString(s.Slug) {
		return fmt.Errorf("botscaffold: invalid slug %q (want %s)", s.Slug, SlugRe)
	}
	s.Shape = strings.TrimSpace(s.Shape)
	if s.Shape != "" && !hasShape(s.Shape) {
		return fmt.Errorf("botscaffold: unknown shape %q (available: %s)", s.Shape, strings.Join(Shapes(), ", "))
	}
	if s.Shape != "" {
		// The shape's files reference vars by name; the vars block is
		// rendered from the Spec. A var the template needs and the Spec
		// dropped (a deleted form row) is named here, not as a compiler
		// diagnostic behind a 500.
		declared := map[string]bool{}
		for _, v := range s.Vars {
			declared[v.Name] = true
		}
		var missing []string
		for _, name := range shapeVarRefs(s.Shape) {
			if !declared[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("botscaffold: shape %q references the var(s) %s, which the spec must declare (its template keeps them; re-add the row)", s.Shape, strings.Join(missing, ", "))
		}
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

	mainBot, annexes, err := renderShape(s)
	if err != nil {
		return Result{}, err
	}
	if err := checkAnnexPaths(annexes); err != nil {
		return Result{}, err
	}
	// Parse + compile every generated workflow with the runtime's own
	// pipeline — main.bot with the shape's prompt annexes in scope, and a
	// child .bot the shape ships on its own; any error-severity diagnostic
	// aborts the scaffold.
	if err := compileGuard(s.Slug+"/main.bot", mainBot, annexes); err != nil {
		return Result{}, err
	}
	for _, rel := range sortedAnnexes(annexes) {
		if strings.HasSuffix(rel, ".bot") {
			// No annexes: a subbot child is compiled as a bare file at
			// launch (runview/subbot.go, CompileWorkflowWithHash on the
			// child path — only main.bot is promoted to its bundle), so the
			// bundle's prompts/*.md are not in ITS scope either. Should the
			// child launch ever merge them, this guard passes the annexes
			// too, or it refuses a child the launch would accept.
			if err := compileGuard(s.Slug+"/"+rel, string(annexes[rel]), nil); err != nil {
				return Result{}, err
			}
		}
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
	// The full .botz layout, so `bundle pack` works on the result as-is
	// and each resource kind has an obvious home.
	for _, sub := range bundle.LayoutDirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return Result{}, fmt.Errorf("botscaffold: mkdir %s: %w", sub, err)
		}
	}

	type file struct {
		name string
		data []byte
	}
	files := []file{
		{"main.bot", []byte(mainBot)},
		{"manifest.yaml", manifest},
		{"README.md", readme},
		{".gitignore", []byte(bundleGitignore)},
		{filepath.Join(bundle.DirPresets, "example.md"), presetExample},
	}
	// A shape's annexes land at their bundle-relative path; a layout dir
	// one of them fills needs no placeholder.
	filled := map[string]bool{}
	for _, rel := range sortedAnnexes(annexes) {
		files = append(files, file{filepath.FromSlash(rel), annexes[rel]})
		filled[strings.SplitN(rel, "/", 2)[0]] = true
	}
	// .gitkeep makes an otherwise-empty layout dir survive `git add`;
	// presets/ ships example.md instead, so it needs none.
	for _, sub := range []string{bundle.DirSkills, bundle.DirPrompts, bundle.DirAttachments} {
		if !filled[sub] {
			files = append(files, file{filepath.Join(sub, ".gitkeep"), nil})
		}
	}
	res := Result{Dir: dir}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Result{}, fmt.Errorf("botscaffold: mkdir for %s: %w", path, err)
		}
		if err := store.WriteFileAtomic(path, f.data, 0o644); err != nil {
			return Result{}, fmt.Errorf("botscaffold: write %s: %w", path, err)
		}
		res.Files = append(res.Files, path)
	}
	return res, nil
}

// templateFuncs are the helpers every template — the shared files and
// the gallery shapes alike — renders with.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"quote":    strconv.Quote,
		"indent":   indentLines,
		"varValue": varValue,
	}
}

func renderTemplate(name string, s Spec) ([]byte, error) {
	tmpl, err := template.New(name).Funcs(templateFuncs()).ParseFS(templateFS, "templates/"+name)
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
