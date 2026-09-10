package botscaffold

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// The gallery shapes: one directory per shape under templates/gallery/,
// holding a main.bot.tmpl and any annex the shape ships (a child
// worker.bot.tmpl, prompts/*.md, skills/*.md). Every file is rendered
// through the same Spec as main.bot, and a trailing `.tmpl` is stripped
// from the written name. The delimiters are `<% %>` so the DSL's own
// {{refs}} stay literal in the shapes; partials.tmpl holds the fragments
// they share (the vars block, an LLM node's model lines, the dials).
//
//go:embed templates/gallery
var galleryFS embed.FS

const galleryRoot = "templates/gallery"

// Shapes lists the gallery's shape ids, sorted — the values Spec.Shape
// accepts besides the empty one.
func Shapes() []string {
	entries, err := fs.ReadDir(galleryFS, galleryRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func hasShape(id string) bool {
	for _, s := range Shapes() {
		if s == id {
			return true
		}
	}
	return false
}

// renderShape renders the bundle's main.bot and, for a gallery shape, its
// annex files keyed by their bundle-relative slash path.
func renderShape(s Spec) (mainBot string, annexes map[string][]byte, err error) {
	if s.Shape == "" {
		out, err := renderTemplate("main.bot.tmpl", s)
		if err != nil {
			return "", nil, err
		}
		return string(out), nil, nil
	}
	dir := path.Join(galleryRoot, s.Shape)
	annexes = map[string][]byte{}
	err = fs.WalkDir(galleryFS, dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		out, err := renderGalleryFile(p, s)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if rel == "main.bot.tmpl" {
			mainBot = string(out)
			return nil
		}
		annexes[strings.TrimSuffix(rel, ".tmpl")] = out
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if mainBot == "" {
		return "", nil, fmt.Errorf("botscaffold: shape %q has no main.bot.tmpl", s.Shape)
	}
	return mainBot, annexes, nil
}

// renderGalleryFile renders one file of a shape, with the shared partials
// in scope.
func renderGalleryFile(p string, s Spec) ([]byte, error) {
	name := path.Base(p)
	tmpl, err := template.New(name).Delims("<%", "%>").Funcs(templateFuncs()).
		ParseFS(galleryFS, path.Join(galleryRoot, "partials.tmpl"), p)
	if err != nil {
		return nil, fmt.Errorf("botscaffold: parse %s: %w", p, err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, s); err != nil {
		return nil, fmt.Errorf("botscaffold: render %s: %w", p, err)
	}
	return buf.Bytes(), nil
}

// fixedFiles are the bundle files Scaffold writes from its own templates;
// a shape annex of one of these names would overwrite vetted content — the
// main.bot outside the compile guard — so it is refused by name.
var fixedFiles = map[string]bool{
	"main.bot":      true,
	"manifest.yaml": true,
	"README.md":     true,
	".gitignore":    true,
	path.Join(bundle.DirPresets, "example.md"): true,
}

// checkAnnexPaths refuses an annex that would land outside the bundle or
// on a fixed file. The embedded FS cannot produce either today; the next
// shape author gets the refusal, not a silent overwrite.
func checkAnnexPaths(annexes map[string][]byte) error {
	for _, rel := range sortedAnnexes(annexes) {
		if rel == "" || path.IsAbs(rel) || rel != path.Clean(rel) || strings.HasPrefix(rel, "../") || rel == ".." {
			return fmt.Errorf("botscaffold: shape annex %q escapes the bundle", rel)
		}
		if fixedFiles[rel] {
			return fmt.Errorf("botscaffold: shape annex %q would overwrite a file the scaffold writes itself", rel)
		}
	}
	return nil
}

var varRefRe = regexp.MustCompile(`\{\{\s*vars\.([a-z_][a-z0-9_]*)`)

// shapeVarRefs lists, sorted, the vars a shape's files reference as
// {{vars.<name>}} — the vars its Spec MUST declare, since the vars block is
// rendered from the Spec and a reference to an undeclared var is a compile
// error (C033) the operator would meet only after the form. Derived from
// the templates themselves, so a shape gaining a reference cannot drift
// from the check.
func shapeVarRefs(shape string) []string {
	dir := path.Join(galleryRoot, shape)
	seen := map[string]bool{}
	_ = fs.WalkDir(galleryFS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// A child workflow the shape ships (worker.bot) declares its own
		// vars; only main.bot and the prompts read the Spec's.
		if name := strings.TrimSuffix(d.Name(), ".tmpl"); name != "main.bot" && strings.HasSuffix(name, ".bot") {
			return nil
		}
		raw, err := galleryFS.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range varRefRe.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedAnnexes returns the annex paths in a stable order.
func sortedAnnexes(annexes map[string][]byte) []string {
	out := make([]string, 0, len(annexes))
	for rel := range annexes {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// mergeAnnexPrompts declares a shape's prompts/*.md annexes on the parsed
// file through the ONE rule every launch surface merges bundle prompts by
// (runview.MergePromptFiles, the same function MergeBundlePrompts reads a
// bundle on disk into) — so the compile guard judges the workflow the way
// it will run: a `system: mission` that lives in prompts/mission.md
// compiles here, a stem nothing ships is refused, and a change to the
// rule cannot make the guard and the launch disagree.
func mergeAnnexPrompts(f *ast.File, annexes map[string][]byte) {
	files := make(map[string]string, len(annexes))
	for rel, body := range annexes {
		files[rel] = string(body)
	}
	runview.MergePromptFiles(f, files, "")
}

// GeneratedError is a generated workflow the runtime's own pipeline
// refuses — a parse or compile error in the rendered .bot. It is the
// OPERATOR's to fix when the cause is their text (a `{{vars.x}}` in the
// mission with no such var), so the creation surfaces answer it as a
// client error with the diagnostic, not as a server failure.
type GeneratedError struct {
	Name   string // the bundle-relative file, e.g. "my-bot/main.bot"
	Detail string // the diagnostic, as the compiler printed it
}

func (e *GeneratedError) Error() string {
	return "botscaffold: generated " + e.Name + " " + e.Detail
}

// compileGuard parses and compiles one generated .bot the way the runtime
// will, with the shape's prompt annexes in scope, and refuses any error
// but C018. C018 (no model/backend and no auto-detectable credential) is
// an ENVIRONMENT verdict, not a scaffolding defect: a template omits
// model/backend on purpose so a run auto-detects the host's credential,
// and a credential-less host (CI, a fresh machine) would otherwise refuse
// a structurally sound bot — the missing credential is surfaced at
// run/validate time, not here.
func compileGuard(name, src string, annexes map[string][]byte) error {
	pr := parser.Parse(name, src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return &GeneratedError{Name: name, Detail: "does not parse: " + d.Error()}
		}
	}
	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return &GeneratedError{Name: name, Detail: "has no workflow"}
	}
	mergeAnnexPrompts(pr.File, annexes)
	for _, d := range ir.Compile(pr.File).Diagnostics {
		if d.Severity != ir.SeverityError || d.Code == ir.DiagMissingModelOrBackend {
			continue
		}
		return &GeneratedError{Name: name, Detail: "does not compile: " + d.Error()}
	}
	return nil
}
