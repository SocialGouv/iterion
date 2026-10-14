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
	// botNamed is that same .bot named as the caller named the document —
	// `fmt --to bot`'s destination, derived from the path as given: the one
	// name a surface says, so a result reads its `file` and its `bot_path`
	// in one form.
	botNamed  string
	src       []byte
	res       *author.Result
	text      string // the .bot text the program writes as, where its frontmatter is read
	verifyErr error  // E054: the program has no written .bot form
	// twinText is the .bot beside the document as it was read at the
	// document's opening and twinThere whether one was there at all: the
	// text E056 judges.
	twinText  []byte
	twinThere bool
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
	// `x.bot.yaml` stands for `x.bot`: the destination `fmt --to bot` writes
	// (twinPath — the `.yaml` off, whatever its case), off the absolute path
	// for every reader, and off the path as given for every sentence.
	botPath := twinPath("bot", abs)
	botNamed := twinPath("bot", path)
	// A .bot path `fmt --to bot` will not write over whatever --force says —
	// a directory, a file that cannot be read — is one the document stands
	// for in vain: nothing a surface could read, draw or launch. The .bot is
	// read once, here: the text E056 judges is the one found readable.
	existing, there, why := twinAt(botPath)
	if why != "" {
		return nil, nil, fmt.Errorf("%s stands for %s, which %s: no .bot can be written there", path, botNamed, why)
	}
	doc := &authorDocument{
		parsePath: abs,
		botPath:   botPath,
		botNamed:  botNamed,
		src:       src,
		res:       author.Parse(abs, src),
		twinText:  existing,
		twinThere: there,
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
	} else if dir := bundle.DirForEntry(doc.botPath); dir != "" {
		// The twin of a SIBLING entry is the bundle's too, validated as its
		// .bot would be — `validate bots/x/extend.bot.yaml` gets what
		// `validate bots/x/extend.bot` gets.
		if b, err = bundle.OpenDirWithMain(dir, doc.botPath); err != nil {
			return nil, nil, runview.EntryBundleError(path+" stands for", dir, err)
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

// twinAnotherProgram is E056 when the .bot beside the document is there and
// is not the program the document describes — another program, the
// difference named; or no program at all: a text the parser only recovers
// on, a fragment it imports that cannot be read, a text the compiler
// refuses on its own account — and nil otherwise.
// Programs are compared, never texts (ir.SameProgram): a .bot carries comment
// lines a document has no form for and is written by another writer, so
// whether the two files are byte-for-byte is the question
// `fmt --to bot --check` answers, not this one. Each side is read as a
// launcher reads it — the document through the unit validate itself
// reports on, the .bot through its own, loaded off the bytes read when the
// document was opened — and everything the two sides SHARE is read once for
// both of them: the prompts of the bundle, which is the one object the two
// files share, so a launcher of either merges the same prompts and a node
// reading one is no difference between them; and the fragments their
// `import` lines name, the bytes the document's own unit read
// (theBotsUnit). A read per side would make a `prompts/*.md` or a
// `lib/*.bot` saved while the verdict is taken a difference between two
// files nobody touched. What the COMPILER reads is outside that freeze: a
// file an {{include}} inside a prompt body names is read once per compile,
// so one saved between the two compiles can still be named here — that
// read is the compiler's to share. Which of the two files is current the
// author alone knows, so it is a warning and the verdict stands. Nothing is
// said where `fmt --to bot` would write no .bot at all — a document that
// does not read, or one whose program has no written form (E054) — nor
// where the program question has no subject: a document the load or the
// compile refuses on its OWN account describes no program to compare, and a
// side compiling to no workflow and carrying no refusal of its own is a
// fragment (isAFragment).
func (doc *authorDocument) twinAnotherProgram(documentUnit *unit.Unit, b *bundle.Bundle) (*parser.Diagnostic, error) {
	if !doc.twinThere || doc.res.HasErrors() || doc.verifyErr != nil {
		return nil, nil
	}
	// The bundle's prompts: one read, for the two sides together.
	prompts, promptsDir, err := runview.BundlePromptFiles(b)
	if err != nil {
		return nil, err
	}
	theDocument := theProgramOf(documentUnit, prompts, promptsDir)
	theBot := theProgramOf(doc.theBotsUnit(documentUnit), prompts, promptsDir)
	if refused := theBot.errorsOfItsOwn(theDocument, doc.botNamed); refused != "" {
		return doc.twinWarning(noProgramBeside + refused), nil
	}
	// A document the load or the compile refuses on its OWN account describes
	// no program either: the verdict carries that refusal, at its position in
	// the document, and there is nothing to hold the .bot to. Saying it twice
	// would also blame the sound side — the hint names two conversions that
	// replace their destination whole, and the .bot is the one copy of the
	// program here.
	if theDocument.hasErrorsOfItsOwn(theBot) {
		return nil, nil
	}
	if theBot.isAFragment() || theDocument.isAFragment() {
		return nil, nil
	}
	// The compiler's own oracle on the two compiled units, which names the
	// workflow's difference wherever both sides compiled one. The order is
	// the frame of what it names: a node of the .bot this document has not
	// "appeared", one of the document the .bot has not "is missing" — the
	// sentence's subject being that .bot.
	if why := ir.SameProgram(theDocument.program, theBot.program); why != "" {
		return doc.twinWarning(" is there and describes another program than this document: " + why), nil
	}
	return nil, nil
}

// theBotsUnit is the unit of the .bot beside the document: that .bot as its
// main, from the bytes read when the document was opened, under its own
// absolute path — where an {{include}} of it resolves — and the fragments
// its imports name as the DOCUMENT's unit read them, so each is read once
// for the two sides E056 compares.
//
// A fragment is staged under its name FROM THIS LOAD'S OWN DIRECTORY — the
// .bot's — which is the name this load asks it for: every fragment of
// either side lives under that directory, the parser refusing an `import`
// path that leaves the file's own (E045), and the loader rebases a staged
// name exactly as it rebases the main's when it reads the file from a root
// ABOVE (a file declaring no `workflow:` below a lib/ is read from the root
// that lib/ belongs to, under its lib/ name). So a staged name answers for
// the file the document's unit read AT THE SAME absolute path, whichever of
// the two sides the loader re-roots — and the document's own file, whatever
// name that unit gave it, is never staged over the .bot's frozen text.
func (doc *authorDocument) theBotsUnit(documentUnit *unit.Unit) *unit.Unit {
	start := filepath.Dir(doc.botPath)
	staged := map[string][]byte{filepath.Base(doc.botPath): doc.twinText}
	for _, f := range documentUnit.Files {
		if f.Rel == documentUnit.Main {
			continue
		}
		rel, err := filepath.Rel(start, filepath.Join(documentUnit.Root, filepath.FromSlash(f.Rel)))
		if err != nil {
			// Two absolute paths that cannot be related: there is no name
			// this load could ask the fragment for, so it is read from
			// disk, as a load with nothing staged reads it.
			continue
		}
		staged[filepath.ToSlash(rel)] = f.Source
	}
	return unit.LoadDirStaged(doc.botPath, staged)
}

// theProgramALauncherRuns is one side of E056's comparison: a file read the
// way the engine reads it — its unit, the fragments its `import` lines name
// read beside it and merged (pkg/dsl/unit, the one place that merges them)
// — and the compile of the program that unit holds. Both sides are read
// this way, so the loader and the compiler a launcher runs give the answer
// and nothing here models what either of them reads.
type theProgramALauncherRuns struct {
	// unit holds what the load says — a fragment it cannot read, a
	// declaration two files make — and program what the compile of
	// unit.Merged says. A file is refused by either, so both are read.
	unit    *unit.Unit
	program *ir.CompileResult
}

// theProgramOf compiles a loaded unit with the bundle's prompts: the caller
// loads the unit as the surface it stands for loads one (unit.LoadDir*) and
// reads those prompts once for both sides (runview.BundlePromptFiles) — a
// node's `system:` names one, so they are part of what a launcher compiles.
// The merge is by name and a declaration in place wins, so a unit the
// caller merges the same prompts into afterwards is left as it is.
func theProgramOf(u *unit.Unit, prompts map[string]string, promptsDir string) theProgramALauncherRuns {
	runview.MergePromptFiles(u.Merged, prompts, promptsDir)
	return theProgramALauncherRuns{unit: u, program: ir.Compile(u.Merged)}
}

// isAFragment is whether this side compiles to no workflow once its
// imports are merged: a file under lib/, a file of prompts or schemas
// alone. There is no program there to compare — such a pair's question is
// about its texts, which `fmt --to bot --check` reads. Asked after
// errorsOfItsOwn: a .bot the load or the compile refuses on its own account
// compiles to no workflow BECAUSE of that refusal, and is named for it.
//
// This states the rule; the oracle agrees with it where it fires. A compile
// that finds no workflow stops at that one diagnostic (ir.compile), so two
// files that both compile to none carry the same code and ir.SameProgram
// answers "one program" for them, while a pair where only ONE side compiles
// none is already named by errorsOfItsOwn, whose code that diagnostic is.
// The rule is held here so a reader finds it, and by the compiler so no
// model of it can drift: removing either leaves every witness green.
func (p theProgramALauncherRuns) isAFragment() bool {
	return p.program.Workflow == nil
}

// errorsOfItsOwn is what the load and the compile refuse p for and not
// other, as a sentence: the first such refusal at its position in the file
// it is in, and a count of the rest — and "" when p carries no refusal of
// its own. mainNamed is p's main file as the sentence names it, so a
// position inside it is not a second form of the subject's own name; a
// fragment keeps its unit-relative name.
func (p theProgramALauncherRuns) errorsOfItsOwn(other theProgramALauncherRuns, mainNamed string) string {
	return theFirstOf(p.refusalsOfItsOwn(other, mainNamed))
}

// hasErrorsOfItsOwn is the same question where the answer is read and no
// sentence is printed: a DOCUMENT its own load or compile refuses describes
// no program to hold the .bot to, and the verdict names that refusal at its
// position already. One predicate, so the two readings cannot drift.
func (p theProgramALauncherRuns) hasErrorsOfItsOwn(other theProgramALauncherRuns) bool {
	return len(p.refusalsOfItsOwn(other, p.unit.Main)) > 0
}

// refusalsOfItsOwn is every error the load — or, where the load refuses
// nothing, the compile — refuses p for and other is not refused for, each
// named as the sentence carrying it names files. A code BOTH sides carry is
// none of p's: a fragment neither of them can read is neither's fault, and
// two files that declare no workflow have no program on either side (C006).
// The `import` lines are merged before either side is compiled, so the
// refusal of a file read ALONE (C030) is one neither side can carry.
func (p theProgramALauncherRuns) refusalsOfItsOwn(other theProgramALauncherRuns, mainNamed string) []string {
	theOthers := other.errorCodes()
	var errs []string
	for _, d := range p.unit.Diagnostics {
		if d.Severity != parser.SeverityError || theOthers[string(d.Code)] {
			continue
		}
		d.File = p.fileNamed(d.File, mainNamed)
		errs = append(errs, d.Error())
	}
	if len(errs) > 0 {
		// The load refused it already, and what the compiler then says of
		// a text it was handed in pieces counts one cause twice: a file
		// the parser only recovered on declares no workflow (C006)
		// BECAUSE of that.
		return errs
	}
	for _, d := range p.program.Diagnostics {
		if d.Severity != ir.SeverityError || theOthers[string(d.Code)] {
			continue
		}
		// A diagnostic the compiler attributes to no declaration carries
		// no position, and is named by its code and its message alone.
		if d.Line > 0 {
			errs = append(errs, fmt.Sprintf("%s:%d:%d: %s", p.fileNamed(d.File, mainNamed), d.Line, d.Column, d.Error()))
			continue
		}
		errs = append(errs, d.Error())
	}
	return errs
}

// fileNamed is the file a diagnostic is in, named as the sentence carrying
// it names things: p's main under mainNamed — the name the subject of the
// sentence already goes by — and any other file of the unit under its
// unit-relative name, which is where the reader opens it.
func (p theProgramALauncherRuns) fileNamed(file, mainNamed string) string {
	if rel := p.unit.RelName(file); rel != p.unit.Main {
		return rel
	}
	return mainNamed
}

// theFirstOf is one diagnostic for a reader, and a count of the rest: a
// refusal cascades — a brace left open refuses every declaration after it —
// and the position to go to is the first. `iterion validate` on the file
// itself prints them all.
func theFirstOf(errs []string) string {
	switch len(errs) {
	case 0:
		return ""
	case 1:
		return errs[0]
	}
	return fmt.Sprintf("%s (and %d more)", errs[0], len(errs)-1)
}

// errorCodes is the code of every error the load and the compile hold.
func (p theProgramALauncherRuns) errorCodes() map[string]bool {
	codes := map[string]bool{}
	for _, d := range p.unit.Diagnostics {
		if d.Severity == parser.SeverityError {
			codes[string(d.Code)] = true
		}
	}
	for _, d := range p.program.Diagnostics {
		if d.Severity == ir.SeverityError {
			codes[string(d.Code)] = true
		}
	}
	return codes
}

// noProgramBeside opens what E056 says of a .bot that is no program at all,
const noProgramBeside = " is there and is no program this document could describe: "

// twinWarning is E056 on the document's first line: a warning, since which
// of the two files is current the author alone knows, saying of the .bot
// beside the document — named as the document was named, the sentence's
// subject — what it is.
func (doc *authorDocument) twinWarning(said string) *parser.Diagnostic {
	return &parser.Diagnostic{
		Code: parser.DiagAuthorTwinProgram, Severity: parser.SeverityWarning,
		File: doc.parsePath, Line: 1, Column: 1,
		Message: doc.botNamed + said,
		Hint:    parser.HintFor(parser.DiagAuthorTwinProgram),
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
	// The .bot named as the caller named the document (doc.botNamed): the
	// MCP stage reads its configuration beside it, and names it as
	// `diagram x.bot` does.
	wf, err := runview.CompileLoadedUnit(u, doc.botNamed, doc.parsePath, b)
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
