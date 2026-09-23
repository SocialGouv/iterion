package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// authorDocument is an author document opened for validation: the program it
// describes, every position of it on the document, and the .bot it stands
// for — beside it, written or not.
type authorDocument struct {
	parsePath string // the document's absolute path: the name every position carries
	botPath   string // the .bot the document stands for
	src       []byte
	res       *author.Result
	text      string // the .bot text the program writes as, where its frontmatter is read
	verifyErr error  // E054: the program has no written .bot form
}

// openAuthorDocument reads an author document and opens the bundle of the
// .bot it stands for, when there is one. The path is absolutised first: an
// include in a prompt resolves beside the document, and the compiler refuses
// a relative source name rather than read beside the process's directory.
func openAuthorDocument(path string) (*authorDocument, *bundle.Bundle, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read file: %w", err)
	}
	// `x.bot.yaml` stands for `x.bot`: the `.yaml` off, whatever its case.
	botPath := abs[:len(abs)-len(".yaml")]
	// A .bot path `fmt --to bot` will not write over whatever --force says —
	// a directory, a file that cannot be read — is one the document stands
	// for in vain: nothing a surface could read, draw or launch.
	if _, _, why := twinAt(botPath); why != "" {
		return nil, nil, fmt.Errorf("%s stands for %s, which %s: no .bot can be written there", path, botPath, why)
	}
	doc := &authorDocument{
		parsePath: abs,
		botPath:   botPath,
		src:       src,
		res:       author.Parse(abs, src),
	}
	// The .bot text the program writes as — where its frontmatter and its
	// syntax are read, errors or not — and, when the document reads without
	// an error, the text that has to read back as the same program: when it
	// does not, the program has no written form (E054).
	doc.text = unparse.Unparse(doc.res.File)
	if !doc.res.HasErrors() {
		doc.verifyErr = unparse.Verify(doc.res.File, doc.text)
	}
	var b *bundle.Bundle
	if dir := bundle.DirForMainBot(doc.botPath); dir != "" {
		// The bundle of the .bot the document stands for, that .bot written
		// or not: its prompts/, skills/, presets/ and manifest are in scope,
		// as they are for `validate bots/x/main.bot`.
		if b, err = bundle.OpenDirWithMain(dir, doc.botPath); err != nil {
			return nil, nil, runview.EntrypointBundleError(path+" stands for", dir, err)
		}
	}
	return doc, b, nil
}

// loadUnit is the unit of the program the document describes: the
// document's AST as its main, every position the document's own, the
// fragments its imports name read beside anchor. validate reports on it
// with the .bot as anchor, where `validate x.bot` reads a .bot's fragments;
// diagram draws it with runview.FragmentAnchor's, where `diagram x.bot`
// reads them. One construction, anchored as each surface's .bot is.
func (doc *authorDocument) loadUnit(anchor string) *unit.Unit {
	return unit.LoadDirWithMainAST(anchor, doc.parsePath, doc.res.File, doc.src)
}

// noWrittenForm is E054 when the document reads and the program it
// describes has no written .bot form — the text the writer produces reads
// back as another program, so `fmt --to bot` refuses to write it — and nil
// otherwise.
func (doc *authorDocument) noWrittenForm() *parser.Diagnostic {
	if doc.verifyErr == nil {
		return nil
	}
	return &parser.Diagnostic{
		Code: parser.DiagAuthorNoWrittenForm, Severity: parser.SeverityError,
		File: doc.parsePath, Line: 1, Column: 1,
		Message: "the document has no written .bot form: " + doc.verifyErr.Error(),
		Hint:    parser.HintFor(parser.DiagAuthorNoWrittenForm),
	}
}

// compileAuthorDocument compiles the program an author document describes
// for a read-only surface that needs the workflow and nothing more — the
// diagram of the .bot the document stands for — as `diagram x.bot` compiles
// that .bot (runview.CompileWorkflowPath): in the bundle it reads (a
// bundle's main, else the bundle any other workflow file belongs to, a
// manifest export naming the .bot being that .bot, written or not), the
// fragments read where it reads them (runview.FragmentAnchor), and every
// stage after the unit is loaded its own (runview.CompileLoadedUnit): the
// unit's errors, the bundle's prompts, the compile, the MCP servers. Before
// those, what only a document has refuses it: its own errors, and the
// program having no written .bot form (E054). Nothing is written.
func compileAuthorDocument(path string) (*ir.Workflow, *authorDocument, error) {
	doc, b, err := openAuthorDocument(path)
	if err != nil {
		return nil, nil, err
	}
	if errs := diagnosticErrors(doc.res.Diagnostics); errs != "" {
		return nil, nil, fmt.Errorf("parse error: %s", errs)
	}
	if d := doc.noWrittenForm(); d != nil {
		return nil, nil, fmt.Errorf("parse error: %s", d.Error())
	}
	if b == nil && filepath.Base(doc.botPath) != bundle.MainBotFile {
		if b, err = bundle.OpenForWorkflowHandedOver(doc.botPath); err != nil {
			return nil, nil, fmt.Errorf("open workflow bundle: %w", err)
		}
	}
	u := doc.loadUnit(runview.FragmentAnchor(doc.botPath, b))
	// The .bot named as the caller named the document: the MCP stage reads
	// its configuration beside it, and names it as `diagram x.bot` does.
	named := path[:len(path)-len(".yaml")]
	wf, err := runview.CompileLoadedUnit(u, named, doc.parsePath, b)
	if err != nil {
		return nil, nil, err
	}
	return wf, doc, nil
}

// frontmatterText is the text a bundle's frontmatter is read from: a .bot's
// own bytes, or the .bot text an author document writes as.
func frontmatterText(doc *authorDocument, src []byte) []byte {
	if doc != nil {
		return []byte(doc.text)
	}
	return src
}

// mainPathOf is the .bot a validation is about: the file itself, or the .bot
// an author document stands for.
func mainPathOf(doc *authorDocument, path string) string {
	if doc != nil {
		return doc.botPath
	}
	return path
}

// documentNameOf is the name of the document's own file in its unit — the
// one file the .bot edits `fix` plans never apply to — or "" for a .bot.
func documentNameOf(doc *authorDocument) string {
	if doc != nil {
		return doc.parsePath
	}
	return ""
}
