package runview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// ErrInlineImport is the refusal of an inline source that imports: text
// handed over without the directory it came from — an upload, a document
// with no bundle behind it — names fragments that did not travel with it,
// and compiling the main alone would be compiling a program with pieces
// missing. The file itself launches from its directory, where the
// fragments are read beside it.
var ErrInlineImport = errors.New("an inline source cannot import: its fragments did not travel with it (launch the .bot from its directory, where lib/ is read beside it)")

// CompiledSource is what a compile read, for the run to record and a
// resume to compare: the identity of the source and the files it was
// made of.
type CompiledSource struct {
	// Hash is the workflow identity: the main's bytes, the bundle's
	// prompts/*.md and presets/*.md, then — only when the unit has them —
	// its fragments and the files its {{include}} markers read. A
	// single-file bot without includes hashes exactly as it always has, so
	// no recorded run compares differently.
	Hash string
	// Main is the main's key in Files.
	Main string
	// Files maps each file of the unit, by slash path from its root, to
	// its text.
	Files map[string]string
	// Included lists the files the {{include}} markers read, by slash path
	// from the unit's root (absolute when outside it), sorted.
	Included []string
}

// workflowIdentity folds the unit's source into one digest, in an order
// that keeps every identity recorded so far: the main's bytes first, the
// bundle's resource directories next (a prompt body or a preset's vars
// change what the agent reads, so a bundle upgrade must invalidate a
// resume — `iterion resume` refuses a changed identity without --force),
// and only then the fragments the main imports and the include closure
// the compiler read, each framed by its path, sorted — so a fragment or
// an included file edited under a parked run is a source change the
// resume gate sees.
func workflowIdentity(u *unit.Unit, b *bundle.Bundle, includedFiles []string) (string, []string, error) {
	h := sha256.New()
	h.Write(u.Files[0].Source)
	if b != nil {
		hashBundleResourceDir(h, b.PromptsDir, "\x00bundle.prompt:")
		hashBundleResourceDir(h, b.PresetsDir, "\x00bundle.preset:")
	}
	fragments := append([]unit.File(nil), u.Files[1:]...)
	sort.Slice(fragments, func(i, j int) bool { return fragments[i].Rel < fragments[j].Rel })
	for _, f := range fragments {
		h.Write([]byte("\x00unit.file:"))
		h.Write([]byte(f.Rel))
		h.Write([]byte{0})
		h.Write(f.Source)
	}
	included := make([]string, 0, len(includedFiles))
	bodies := make(map[string][]byte, len(includedFiles))
	for _, full := range includedFiles {
		body, err := os.ReadFile(full)
		if err != nil {
			return "", nil, fmt.Errorf("cannot read included file %s: %w", full, err)
		}
		rel := includeRel(u.Root, full)
		included = append(included, rel)
		bodies[rel] = body
	}
	sort.Strings(included)
	for _, rel := range included {
		h.Write([]byte("\x00include:"))
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(bodies[rel])
	}
	return hex.EncodeToString(h.Sum(nil)), included, nil
}

// includeRel names an included file by its slash path from the unit's
// root, and by its absolute path when it lies outside — an include
// resolves beside the file that declares it, so only a unit with no root
// on disk gets there.
func includeRel(root, full string) string {
	if root == "" {
		return filepath.ToSlash(full)
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(full)
	}
	return filepath.ToSlash(rel)
}

// hashBundleResourceDir folds every *.md file under dir into h, in sorted
// name order, framed as `<tag><name>\x00<body>`. No-op for an empty/missing
// dir. Shared by computeWorkflowHash across bundle resource directories so
// the framing stays identical and a new resource dir is one call, not a copy.
func hashBundleResourceDir(h hash.Hash, dir, tag string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		h.Write([]byte(tag))
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(body)
	}
}

// CompileWorkflow parses and compiles a workflow source file at path. It returns
// the compiled workflow or an error with the first parse / compile
// diagnostic encountered.
//
// MCP server resolution is finalised against the file's directory so
// relative `command` paths in `mcp_server` blocks resolve correctly.
func CompileWorkflow(path string) (*ir.Workflow, error) {
	wf, _, err := compileWith(path, "", false, nil)
	return wf, err
}

// CompileWorkflowWithHash is CompileWorkflow plus a SHA-256 hash of the
// raw source bytes. The hash is persisted in run.json so that resume
// can detect when the workflow file has changed under it (and require
// --force to proceed). Use this everywhere a workflow is loaded for
// execution; CompileWorkflow is for static-only callers (validate,
// diagram).
func CompileWorkflowWithHash(path string) (*ir.Workflow, string, error) {
	return compileWith(path, "", true, nil)
}

// CompileBundleWorkflow is CompileWorkflowWithHash specialised for bundle
// inputs: it merges the bundle's prompts/*.md into the AST File before
// compilation, so node-level prompt references resolve against bundle
// resources during static validation.
func CompileBundleWorkflow(path string, b *bundle.Bundle) (*ir.Workflow, string, error) {
	return compileWith(path, "", true, b)
}

// CompileSubbotWorkflow compiles a resolved child with its bundle context.
// bot:// supplies the bundle directly; local workflow paths are promoted when
// they live anywhere inside a directory bundle, including workflows/ exports.
func CompileSubbotWorkflow(path string, resolvedBundle *bundle.Bundle) (*ir.Workflow, string, *bundle.Bundle, error) {
	b := resolvedBundle
	if b == nil {
		var err error
		b, err = bundle.OpenForWorkflow(path)
		if err != nil {
			return nil, "", nil, err
		}
	}
	if b != nil {
		wf, hash, err := CompileBundleWorkflow(path, b)
		return wf, hash, b, err
	}
	wf, hash, err := CompileWorkflowWithHash(path)
	return wf, hash, nil, err
}

// CompileWorkflowFromSource is the cloud-mode entry point: the workflow
// content is supplied verbatim (uploaded by the studio SPA). Path is
// retained as a logical label for diagnostics + MCP relative-path
// resolution; when empty, MCP resolution falls back to the current
// working directory.
func CompileWorkflowFromSource(path, source string) (*ir.Workflow, string, error) {
	return compileWith(path, source, true, nil)
}

// compileForLaunch picks the right compile path for a Launch / Resume:
// inline source when supplied, on-disk file otherwise. Used by the
// cloud-mode publisher path so a missing FilePath isn't fatal as long
// as the caller uploaded the source.
//
// bundleDir, when non-empty, is a launch-materialized STORED bot bundle
// (LaunchSpec.BundleDir): its prompts/ merge into the AST before ir.Compile
// — the same treatment a baked bundle gets through CompileBundleWorkflow —
// so a stored bot's prompt references validate and hash identically. A
// bundle dir that fails to open is an explicit error, never a silent
// fall-through to a prompt-less compile.
//
// A path alone is compiled the way every path-driven surface compiles it
// (CompileWorkflowPath): a bundle's main.bot is promoted to its bundle —
// prompts/*.md in scope, the bundle's hash, the handle for the engine.
// This is the compile behind the dispatcher's service path and the trigger
// launcher; a bare compile here failed a bundle whose prompts live in
// prompts/ with C003 on those surfaces and hashed it unlike the CLI. The
// studio's file picker sends the file's SOURCE inline, materialised under
// the store as `<hash>-main.bot` — a name no promotion recognises — so it
// reaches the same bundle through bundleDir, stamped by the server from
// the path the operator named. The bundle the compile used is returned
// (nil for inline source without one, or a loose file) so the launch
// hands the engine the same handle it compiled against.
func compileForLaunch(path, source, bundleDir string) (*ir.Workflow, *CompiledSource, *bundle.Bundle, error) {
	if bundleDir != "" {
		b, err := bundle.OpenDir(bundleDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open stored bot bundle: %w", err)
		}
		wf, cs, err := compileUnit(path, source, true, b)
		return wf, cs, b, err
	}
	if source != "" {
		wf, cs, err := compileUnit(path, source, true, nil)
		return wf, cs, nil, err
	}
	b, err := bundleForPath(path)
	if err != nil {
		return nil, nil, nil, err
	}
	wf, cs, err := compileUnit(path, "", true, b)
	return wf, cs, b, err
}

// insideDir reports whether path lies under dir (both absolute), symlinks
// resolved on both sides when they can be (bundle.ResolvedPath): a path
// that is not there — the .bot an author document stands for, not written
// yet — lies where its directory resolves to.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(bundle.ResolvedPath(dir), bundle.ResolvedPath(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// hasFragmentDir reports whether a lib/ directory sits beside path.
func hasFragmentDir(path string) bool {
	info, err := os.Stat(filepath.Join(filepath.Dir(path), unit.FragmentDir))
	return err == nil && info.IsDir()
}

// besideBundleMain reports whether the workflow file at path (absolute),
// compiled in bundle b, has its fragments read beside the bundle's main
// rather than beside itself: a file outside its bundle with no lib/ of its
// own beside it. That is the store's materialised copy of a bundle's main,
// which a studio run records and resumes from: it is that bundle's main,
// and its fragments live beside the ORIGINAL. A file with a lib/ of its own
// is a unit of its own, wherever its bundle is.
func besideBundleMain(path string, b *bundle.Bundle) bool {
	return b != nil && !insideDir(path, b.Dir) && !hasFragmentDir(path)
}

// FragmentAnchor is the file beside which the fragments of the workflow
// file at path (absolute), compiled in bundle b, are read: the file itself,
// or the bundle's main (besideBundleMain). It is for a caller that loads
// the unit itself, such as an author document's diagram, whose main is the
// document's AST read as the .bot at path.
func FragmentAnchor(path string, b *bundle.Bundle) string {
	if besideBundleMain(path, b) {
		return b.IterPath
	}
	return path
}

// bundleForPath is the bundle a path-driven compile reads a workflow
// against: its bundle when it is a bundle's main.bot, else the nearest
// enclosing directory bundle of a workflow that lives inside one
// (workflows/ exports), else nil for a loose file.
func bundleForPath(path string) (*bundle.Bundle, error) {
	b, err := ResolveBundleFromFilePath(path)
	if err != nil {
		return nil, err
	}
	if b == nil && filepath.Base(path) != bundle.MainBotFile {
		b, err = bundle.OpenForWorkflow(path)
		if err != nil {
			return nil, fmt.Errorf("open workflow bundle: %w", err)
		}
	}
	return b, nil
}

// ResolveBundleFromFilePath inspects filePath and, when it is the
// canonical entrypoint of a directory bundle (named main.bot, in a parent
// dir that carries `skills/` or `manifest.yaml`), opens the parent as a
// bundle so the compile sees its prompts/*.md and the engine mirrors
// skills/, recipes/, attachments/ into the workspace at run time.
//
// (nil, nil) when filePath is empty, not the canonical name, or the parent
// carries no bundle marker — a loose .bot. A parent that IS a bundle by
// those markers and does not open (a manifest that does not decode) is an
// ERROR, never a silent fall-through to the bare file: the same state
// `iterion validate <dir>` refuses, so the file and directory forms give
// one verdict, and a run never quietly starts without its prompts and
// skills. What counts as a bundle is pkg/bundle's to decide (DirForMainBot).
//
// Mirrors the auto-promotion the CLI does in pkg/cli/run.go (F-NEW-4).
// Without this, studio launches of `iterion run bots/whats-next/main.bot`
// silently produce empty `.claude/skills/` and prompts that reference
// `repo-survey.md` fail with `no such file or directory`.
func ResolveBundleFromFilePath(filePath string) (*bundle.Bundle, error) {
	if filePath == "" {
		return nil, nil
	}
	dir := bundle.DirForMainBot(filePath)
	if dir == "" {
		return nil, nil
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		return nil, EntrypointBundleError(filePath+" is", dir, err)
	}
	return b, nil
}

// EntrypointBundleError is the refusal of a bundle's entrypoint whose
// bundle does not open, with the remedy a bare main.bot beside a manifest
// needs. subject names the entrypoint and how it stands to the bundle —
// `x/main.bot is`, or an author document that `stands for` it — so a .bot
// and the document of that .bot are refused in one set of words.
func EntrypointBundleError(subject, dir string, err error) error {
	return fmt.Errorf("%s the entrypoint of bundle %s, which does not open: %w (a main.bot beside an iterion manifest or a skills/ is that bundle: fix the manifest, or give main.bot a directory of its own if this is not its bundle)", subject, dir, err)
}

func compileWith(path, inline string, withHash bool, b *bundle.Bundle) (*ir.Workflow, string, error) {
	wf, cs, err := compileUnit(path, inline, withHash, b)
	if err != nil {
		return nil, "", err
	}
	return wf, cs.Hash, nil
}

// compileUnit loads the unit the source belongs to, compiles its merged
// program and reports what it read. A path is a unit on disk: the file
// and the fragments its imports reach, read beside it. Inline text with a
// bundle behind it — the studio launching a document — is that bundle's
// unit with the document as its main, the fragments read beside the
// bundle's main. Inline text alone is a unit of one file, and one that
// imports is refused (ErrInlineImport): its fragments did not travel.
func compileUnit(path, inline string, withHash bool, b *bundle.Bundle) (*ir.Workflow, *CompiledSource, error) {
	if workflowfile.IsAuthorDocument(path) {
		// The floor under every compile entry: a draft never becomes a
		// program here, whatever door named it — and it is refused by
		// name, never as a parse error of a text that was YAML.
		return nil, nil, bundle.AuthorDocumentError(path)
	}
	parserPath := path
	if parserPath == "" {
		parserPath = "<inline>"
	} else if abs, err := filepath.Abs(parserPath); err == nil {
		// An include resolves beside the file named in full, never against
		// the process working directory — the compiler refuses a relative
		// name — and the CLI hands the path over as typed.
		parserPath = abs
	}
	var u *unit.Unit
	switch {
	case inline != "" && b != nil:
		u = unit.LoadDirWithMain(b.IterPath, parserPath, []byte(inline))
	case besideBundleMain(parserPath, b):
		src, err := os.ReadFile(parserPath) // #nosec G304 -- the path the caller named
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read file: %w", err)
		}
		u = unit.LoadDirWithMain(b.IterPath, parserPath, src)
	case inline != "":
		u = unit.LoadMap(map[string]string{parserPath: inline}, parserPath)
		if len(u.Files) > 0 && u.Files[0].AST != nil && len(u.Files[0].AST.Imports) > 0 {
			return nil, nil, fmt.Errorf("%s: %w", parserPath, ErrInlineImport)
		}
	default:
		u = unit.LoadDir(parserPath)
		if len(u.Files) == 0 {
			// The main itself was not read: the error names the file the
			// caller asked for, wrapped, as it always has.
			if _, err := os.ReadFile(parserPath); err != nil {
				return nil, nil, fmt.Errorf("cannot read file: %w", err)
			}
		}
	}
	return compileLoadedUnit(u, path, parserPath, withHash, b)
}

// CompileLoadedUnit runs, on a unit the caller loaded, every stage a
// path-driven compile of the workflow file at path runs after loading its
// unit: the unit's errors refused, a workflow required, the bundle's
// prompts merged, the compile, the MCP servers prepared against path's
// directory. The source's identity, which only a launch records, is not
// taken. name is the file a refusal names. It is for a caller that loads
// the unit itself — an author document's diagram, whose main is the
// document's AST read as the .bot at path — so that what refuses the .bot
// refuses the document read as it.
func CompileLoadedUnit(u *unit.Unit, path, name string, b *bundle.Bundle) (*ir.Workflow, error) {
	wf, _, err := compileLoadedUnit(u, path, name, false, b)
	return wf, err
}

// compileLoadedUnit is every stage of compileUnit after the unit is loaded;
// name is the file a refusal names.
func compileLoadedUnit(u *unit.Unit, path, name string, withHash bool, b *bundle.Bundle) (*ir.Workflow, *CompiledSource, error) {
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, nil, fmt.Errorf("parse error: %s", d.Error())
		}
	}
	file := u.Merged
	if file == nil || len(file.Workflows) == 0 {
		return nil, nil, fmt.Errorf("no workflow found in %s", name)
	}

	// Bundle prompts must merge into the AST before ir.Compile so the
	// validator sees them when resolving node-level prompt references.
	if b != nil {
		if err := MergeBundlePrompts(file, b); err != nil {
			return nil, nil, err
		}
	}

	cr := ir.Compile(file)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				return nil, nil, fmt.Errorf("compile error: %s", d.Error())
			}
		}
	}
	cs := &CompiledSource{Main: u.Main, Files: make(map[string]string, len(u.Files))}
	for _, f := range u.Files {
		cs.Files[f.Rel] = string(f.Source)
	}
	// The identity is taken after the compile — the include closure is what
	// the compiler read — and after the bundle merge, so a bundle upgrade
	// that swaps a prompt body invalidates `iterion resume`'s change
	// detection as surely as an edit to the .bot does.
	if withHash {
		hash, included, err := workflowIdentity(u, b, cr.IncludedFiles)
		if err != nil {
			return nil, nil, err
		}
		cs.Hash, cs.Included = hash, included
	}
	mcpDir := "."
	if path != "" {
		mcpDir = filepath.Dir(path)
	}
	if err := mcp.PrepareWorkflow(cr.Workflow, mcpDir); err != nil {
		return nil, nil, err
	}

	return cr.Workflow, cs, nil
}

// BundleNameForPath is the declared id of the bundle a workflow path belongs
// to, or "" when the path is not a bundle entrypoint.
//
// It outranks anything derived from the path itself: a `.botz` archive is
// extracted into a cache slot named after its CONTENT HASH, so a path-derived
// identity would key the bot's memory on a name that changes with every edit
// to the bundle — orphaning what it had learned on each version bump, and
// disagreeing with the same bundle opened in directory form.
func BundleNameForPath(filePath string) string {
	// A bundle that does not open has no name here; the error surfaces
	// where the same path is compiled or launched, which every caller of
	// this helper also does.
	b, _ := ResolveBundleFromFilePath(filePath)
	return b.Name()
}

// CompileWorkflowPath compiles the workflow at path the way a launch
// does: a bare main.bot inside a bundle directory is promoted to its
// bundle (prompts/*.md in scope, the bundle's hash), any other file
// compiles alone. The promoted bundle is returned (nil for a plain
// file) so the caller can hand it to the run — the promotion is the
// WHOLE bundle, skills/ included, not the compile alone. Every surface
// that derives a run's hash from a PATH — the dispatcher's engine path,
// rewind, the export, a recipe's file — goes through it, so a run
// launched on one surface resumes on another without `--force`.
func CompileWorkflowPath(path string) (*ir.Workflow, string, *bundle.Bundle, error) {
	b, err := bundleForPath(path)
	if err != nil {
		return nil, "", nil, err
	}
	if b != nil {
		wf, hash, err := CompileBundleWorkflow(path, b)
		return wf, hash, b, err
	}
	wf, hash, err := CompileWorkflowWithHash(path)
	return wf, hash, nil, err
}
