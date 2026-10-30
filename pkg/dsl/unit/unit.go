// Package unit loads a bot's compilation unit: its main file and the
// fragments the file imports, transitively, merged into one ast.File
// (ADR-098 §3). A `.bot` that carries `import "lib/x.bot"` lines is not a
// program until its fragments are merged in — the compiler refuses one
// alone (C030) — and this package is the one place that merges them, for
// every surface that turns a path or a set of files into a workflow.
package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Reader reads one file of the unit by its slash path relative to the
// unit's root ("main.bot", "lib/a.bot"). It reports a wrapped
// os.ErrNotExist for a file that is not there, and ErrOutside for a path
// the unit may not read: one that resolves beyond the root through a
// symlink, or names a file the reader does not hold.
type Reader func(rel string) ([]byte, error)

// ErrOutside is what a Reader returns for a path beyond the unit's root.
var ErrOutside = errors.New("outside the unit")

// FragmentDir is the directory, under the unit's root, that holds every
// fragment: an import must resolve into it, and nothing under it is ever
// discovered as a workflow of its own.
const FragmentDir = "lib"

// File is one file of the unit, as read and as parsed.
type File struct {
	// Rel is the slash path from the unit's root: "main.bot", "lib/a.bot".
	Rel string
	// Name is what the parser was given as the file's name — the absolute
	// path on disk, or the key of a files map — and what every Span of
	// AST carries; an include resolves against it.
	Name    string
	Source  []byte
	AST     *ast.File
	Profile int
	// ProfileReads lists, for a file read as profile 1, the places
	// profile 2 would read otherwise (C144 is each file's own).
	ProfileReads []parser.ProfileRead
	// ParseDiagnostics is what the parser answered for THIS file's own
	// text. The unit's Diagnostics hold them mixed with the loader's — an
	// import it cannot follow, a cycle, a declaration made twice — each
	// positioned in the file that led to it, so attributing a parse
	// verdict by position would call a file broken for its importer's
	// mistake. Empty for a main the caller handed over parsed: its
	// diagnostics are the caller's.
	ParseDiagnostics []parser.Diagnostic
	// parseUnreported is how many errors of the parse ParseDiagnostics
	// does not hold: it reports at most parser.MaxReported of each code and
	// parser.MaxReportedOfASeverity of each severity.
	parseUnreported int
	// Refused is the bound this file's text was refused whole for —
	// parser.BoundBudget when the bot's read budget ran out in it,
	// parser.BoundSize past the size cap — BoundNone otherwise: when it was
	// read, and when it is no text the reader reads at all (not UTF-8,
	// E006), which no bound refused — its AST is nil. A refused file's AST
	// holds nothing of its text, so no document built from the unit holds
	// its declarations: a writer that rebuilt the file from one would write
	// it empty.
	Refused parser.Bound
}

// ParseErrorCount is how many errors the parse of this file found, reported
// or not — the count to show of ParseDiagnostics.
func (f File) ParseErrorCount() int {
	return f.parseUnreported + errorsIn(f.ParseDiagnostics)
}

// errorsIn counts the errors of diags.
func errorsIn(diags []parser.Diagnostic) int {
	n := 0
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			n++
		}
	}
	return n
}

// Unit is the loaded and merged compilation unit.
type Unit struct {
	// Root is the directory of the main file on disk, "" for a files map.
	Root string
	// Main is the main file's Rel.
	Main string
	// Files holds the main first, then the fragments in the order the
	// imports reach them (depth first).
	Files []File
	// Merged is the unit's program: the main's AST with every fragment's
	// declarations appended, its keyed blocks merged, its imports cleared
	// and its subbot sources made relative to the root. It is what the
	// compiler gets and what an inline launch writes out flat —
	// unparse.Unparse of it carries no import line, since it has none.
	Merged *ast.File
	// Digest identifies the unit's source: the files' relative paths and
	// contents, in a fixed order, and whether the load read them whole
	// (Whole). It changes when any file changes — and between a load a
	// bound of the reader cut short and a whole one of the same files: a
	// document built from what a short load read is never taken, under a
	// larger read budget, for one built from all of them.
	Digest string
	// Unread holds the Rel of every fragment an `import` names that the
	// load TRIED to read and could not — one that is not there, one that
	// resolves out of the unit through a symlink, one the reader refused.
	// Such a fragment is a file of NO list: Files holds what the load
	// reached and nothing of what it did not, while Diagnostics refuse on
	// it — so it is the only name a caller has for the file whose arrival
	// would change this load, and the repair the refusal asks for. Sorted
	// and without repeats: a function of the set, not of the order the
	// imports reached it. A path the loader refuses WITHOUT reading (one
	// outside the bot's lib/) is none of these: no write there changes
	// what this load reads.
	Unread []string
	// Diagnostics holds every file's parse diagnostics plus the loader's
	// own (an unreadable fragment, a cycle, a path outside lib/, a
	// declaration made twice), each positioned in its file. Each file's
	// parse reports at most parser.MaxReported of each code and
	// parser.MaxReportedOfASeverity of each severity, and so does the
	// loader of its own, the last one kept of a code saying how many more.
	Diagnostics []parser.Diagnostic
	// unreported is how many errors of the load Diagnostics does not hold.
	unreported int
	// refusedAt is 1 + the index in Diagnostics of the refusal that left a
	// file of the bot unread, 0 when the load read every file whole
	// (Whole, Refusal), and refusedBound the bound that refusal met.
	refusedAt    int
	refusedBound parser.Bound
}

// Whole reports whether the load read every file the bot's imports reach,
// each one whole. A bound of the reader leaves a unit short: the bot's read
// budget, which its files share, run out in a file (read as nothing,
// File.Refused) or before an import (the fragment and every file after it
// not read at all), or a file past the size cap — and so does a file that
// is no text the reader reads (not UTF-8, E006: refused before it lexes,
// its AST nil, its diagnostic the refusal), and a fragment that is there
// and is not read (its permissions, or the root's confinement: a symlink, a
// path resolving outside; its E046 the refusal). The unit then holds only
// what was read — its Merged is a salvage, a document built from it lacks
// what was not read, and no file of it the load did not read whole
// (ShortError.ReadWhole) may be written from one: the writer would write
// such a file without the declarations it holds. The verdict is the
// reader's, not the files': the same files read whole under a larger
// budget (parser.ReadBudgetEnv), which no revision of them shows. A fragment
// an import names that is not there, and a main the load cannot read at all,
// hold nothing to lose: each is an error (E046), not a short load — a main
// not read leaves no file and an empty document.
//
// Every loader answers a load cut short as a *ShortError, never as a unit:
// only Salvage hands a short unit over, so Whole is false only on a unit a
// caller asked for by that name.
func (u *Unit) Whole() bool { return u.refusedAt == 0 }

// ShortError is a load a bound of the reader cut short (Unit.Whole), one
// that met a file the reader does not read as text (E006), or one that met a
// fragment that is there and was not read (its permissions, or the root's
// confinement): what it read is
// no unit of the bot — a file it did not read holds nothing in it — so every
// loader answers it in place of one, and the caller that writes, emits or
// decides has nothing short to act on. It unwraps to the refusal's
// *parser.ReadRefusal, positioned where the bot was cut short, its remedy
// the operator's read budget — or, for E006, the file's encoding, and for an
// unread fragment its permissions (the Bound then BoundNone).
type ShortError struct {
	salvage *Unit
}

// unreadHint is an unreadable file's remedy: the create-the-fragment hint
// for a file that is not there, the permissions' for one the OS refused to
// read — the repair the refusal asks for, not a guess about the shape of
// the path.
func unreadHint(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "the file is there but its permissions refuse the read: make it readable by the user this server runs as"
	}
	return parser.HintFor(parser.DiagImportUnreadable)
}

// ReadRefusal is the refusal that cut the load short — in the file the
// budget ran out in, at the import it ran out before, or at the head of a
// file past the size cap — as the load's diagnostics spell it: cut to the
// unit's names once RelDiagnostics has run.
func (e *ShortError) ReadRefusal() *parser.ReadRefusal {
	d, _ := e.salvage.Refusal()
	return &parser.ReadRefusal{Bound: e.salvage.refusedBound, Diagnostic: d}
}

func (e *ShortError) Error() string { return e.ReadRefusal().Error() }

// ReadWhole is the Rel of every file the load read whole before and after
// it was cut short, main first, in the order the imports reached them: the
// files a writer may replace from what the load showed. A file a bound
// refused (File.Refused), one that is no text the reader reads (E006), a
// fragment that is there and was not read, and every file past a budget's
// cut are not among them.
func (e *ShortError) ReadWhole() []string {
	var rels []string
	for _, f := range e.salvage.Files {
		if f.AST != nil && f.Refused == parser.BoundNone {
			rels = append(rels, f.Rel)
		}
	}
	return rels
}

// Unwrap is the refusal, for a caller that tells a short reading apart from
// any other failure with errors.As, a source's or a bot's alike.
func (e *ShortError) Unwrap() error { return e.ReadRefusal() }

// RelDiagnostics cuts the unit's root from the refusal (Unit.RelDiagnostics),
// for a refusal that crosses to a client.
func (e *ShortError) RelDiagnostics() { e.salvage.RelDiagnostics() }

// Root is the root of the load cut short (Unit.Root): what a boundary cuts
// from a text that quotes the refusal after it was wrapped.
func (e *ShortError) Root() string { return e.salvage.Root }

// Salvage is what a load read, whether it read the bot whole or not — the
// unit of a whole load, or, for a load cut short (*ShortError), what it read,
// Whole false and Refusal its cause — as a view (Salvaged). It is the opt-in
// into a partial reading, the one way to see a short load: a document built
// from one lacks what was not read, a file rebuilt from one is written
// without its declarations, a verdict drawn from one judges a bot it did not
// read — so no method of the view hands out a unit, a file's AST or the
// merged program, and no API that takes one of those accepts the view. The
// display JSON of a short view is marked a salvage, which ast.UnmarshalFile
// refuses wherever the decode runs (MarshalDocument). What the compiler does
// not hold — the view read through unsafe or reflect's pointer doors, a
// helper handed it — the guards hold, on this platform's build:
// every production reference to Salvage and every function that holds a
// view is listed with what it shows (TestEverySalvageOptInIsListed,
// TestEveryFunctionHoldingASalvageIsListed), none of them decodes a
// document, and unsafe, reflect.NewAt and reflect.Value's UnsafePointer,
// UnsafeAddr and Pointer are listed by file; a file another build compiles
// is held by its imports (TestNoGuardIsReadAroundByTheBuild). It takes a
// loader's two answers as they come: Salvage(LoadDir(path)).
func Salvage(u *Unit, err error) *Salvaged {
	var short *ShortError
	if errors.As(err, &short) {
		return &Salvaged{u: short.salvage}
	}
	if err != nil {
		panic(fmt.Sprintf("unit.Salvage: %v is no loader's answer — a loader fails only short", err))
	}
	return &Salvaged{u: u}
}

// Salvaged is a load's reading as an answer shows it: its diagnostics and
// refusal, its files' names, bytes and parse verdicts, its revision, and the
// display of its merged document — no method hands out the unit, a file's
// AST or the program, which are what a writer, an emitter or a verdict
// takes. Salvage is the one way to one.
type Salvaged struct{ u *Unit }

// SalvagedFile is one file of a load as an answer shows it: its names, the
// bytes read, its header's profile and import paths, its own parse's verdict
// — never its AST.
type SalvagedFile struct {
	// Rel is the slash path from the unit's root; Name what the parser was
	// given as the file's name (File.Name).
	Rel, Name string
	// Source is the file's bytes as the load read them: the stored text,
	// never a reading of it.
	Source []byte
	// Profile is the file's effective syntax profile, Imports its import
	// paths as written: zero and none for a file the reader refused.
	Profile int
	Imports []string
	// ParseErrors is how many errors its own parse found, reported or not
	// (File.ParseErrorCount), ParseDiagnostics the reported ones.
	ParseErrors      int
	ParseDiagnostics []parser.Diagnostic
	// Refused is the bound its text was refused whole for (File.Refused).
	Refused parser.Bound
	// ReadWhole reports whether the load read the file whole — the files an
	// answer issues a digest for (the open response's per-file digests, the
	// claim a save of one presents): a file a bound refused, one that is no
	// text the reader reads (E006), a fragment that is there and was not
	// read, and every file past a budget's cut are not among them
	// (ShortError.ReadWhole).
	ReadWhole bool
}

// Whole reports whether the load read the bot whole (Unit.Whole).
func (s *Salvaged) Whole() bool { return s.u.Whole() }

// Refusal is the diagnostic that left the load short (Unit.Refusal).
func (s *Salvaged) Refusal() (parser.Diagnostic, bool) { return s.u.Refusal() }

// Diagnostics is the load's diagnostics, as Unit.Diagnostics: a copy.
func (s *Salvaged) Diagnostics() []parser.Diagnostic {
	return append([]parser.Diagnostic(nil), s.u.Diagnostics...)
}

// ErrorCount and HasErrors are the load's (Unit.ErrorCount, Unit.HasErrors).
func (s *Salvaged) ErrorCount() int { return s.u.ErrorCount() }
func (s *Salvaged) HasErrors() bool { return s.u.HasErrors() }

// RelDiagnostics cuts the unit's root from its diagnostics, for an answer
// that crosses to a client (Unit.RelDiagnostics).
func (s *Salvaged) RelDiagnostics() { s.u.RelDiagnostics() }

// RelError cuts the unit's root from an error's text (Unit.RelError).
func (s *Salvaged) RelError(err error) error { return s.u.RelError(err) }

// Main, Root, Digest and Unread are the load's (Unit's fields of the same
// names): the main's Rel, the root on disk, the revision — a short load's
// is another than any whole load's — and the fragments named but not read.
func (s *Salvaged) Main() string     { return s.u.Main }
func (s *Salvaged) Root() string     { return s.u.Root }
func (s *Salvaged) Digest() string   { return s.u.Digest }
func (s *Salvaged) Unread() []string { return append([]string(nil), s.u.Unread...) }

// Files is the load's files as an answer shows them, main first.
func (s *Salvaged) Files() []SalvagedFile {
	out := make([]SalvagedFile, 0, len(s.u.Files))
	for _, f := range s.u.Files {
		sf := SalvagedFile{Rel: f.Rel, Name: f.Name, Source: f.Source, ParseErrors: f.ParseErrorCount(), ParseDiagnostics: append([]parser.Diagnostic(nil), f.ParseDiagnostics...), Refused: f.Refused, ReadWhole: f.AST != nil && f.Refused == parser.BoundNone}
		if f.AST != nil {
			sf.Profile = f.AST.Profile
			for _, im := range f.AST.Imports {
				sf.Imports = append(sf.Imports, im.Path)
			}
		}
		out = append(out, sf)
	}
	return out
}

// MarshalDocument is the merged document's JSON with each declaration's file
// named from root (ast.MarshalFileWithProvenance) — what an editor shows and
// marks non-bindable when the load was short. It is display: when the load
// was short it is not the bot, and it carries the mark of a salvage
// (ast.MarshalSalvageWithProvenance: "not read whole" and the refusal's
// code), which ast.UnmarshalFile refuses wherever the decode runs — handed
// as bytes, as any or as a type parameter to a function no guard lists, it
// decodes into no program. A load read whole is the bot: its document
// carries no mark.
func (s *Salvaged) MarshalDocument(root string) ([]byte, error) {
	if d, short := s.u.Refusal(); short {
		return ast.MarshalSalvageWithProvenance(s.u.Merged, root, "not read whole: "+string(d.Code))
	}
	return ast.MarshalFileWithProvenance(s.u.Merged, root)
}

// whole is a loader's answer for the load u: u when it read the bot whole,
// its *ShortError otherwise.
func whole(u *Unit) (*Unit, error) {
	if !u.Whole() {
		return nil, &ShortError{salvage: u}
	}
	return u, nil
}

// Refusal is the diagnostic that left the unit short (Whole): positioned in
// the file the budget ran out in, at the import it ran out before, or at
// the head of a file past the size cap, its Hint the remedy — as the unit's
// Diagnostics spell it, so a root cut (RelDiagnostics) reaches it. ok is
// false for a whole unit.
func (u *Unit) Refusal() (d parser.Diagnostic, ok bool) {
	if u.refusedAt == 0 {
		return parser.Diagnostic{}, false
	}
	return u.Diagnostics[u.refusedAt-1], true
}

// ErrorCount is how many errors the load found, reported or not — the
// count to show of Diagnostics: every file's parse errors and the
// loader's own.
func (u *Unit) ErrorCount() int {
	return u.unreported + errorsIn(u.Diagnostics)
}

// HasErrors reports whether any diagnostic is an error.
func (u *Unit) HasErrors() bool {
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// LoadDir loads the unit whose main file is at mainPath on disk. The
// unit's root is the main's directory; a fragment is read only when it
// resolves under that root through no symlink (the cloud snapshot never
// follows one either, so a unit valid here launches from a snapshot).
// Files are parsed under their absolute path, so an include inside a
// fragment resolves beside the fragment. A load a bound of the reader cuts
// short answers nil and a *ShortError (Unit.Whole) — every loader does.
func LoadDir(mainPath string) (*Unit, error) {
	return whole(loadDir(mainPath, "", nil, nil, 0, false))
}

// LoadDirStaged is LoadDir with some files' contents supplied — keyed by
// slash path from the main's directory — in place of what is on disk: a
// save checks, before it writes, that every importer of a rewritten
// fragment still loads against the staged text.
func LoadDirStaged(mainPath string, staged map[string][]byte) (*Unit, error) {
	return whole(loadDir(mainPath, "", staged, nil, 0, false))
}

// LoadDirWithMain is LoadDir with the main's text supplied — an editor's
// document, saved or not — while the fragments are read beside the main
// on disk. The main is parsed under mainName when given (the name the
// caller compiles the document as; an include in it resolves beside that
// name), the fragments under their own absolute paths.
func LoadDirWithMain(mainPath, mainName string, source []byte) (*Unit, error) {
	return whole(loadDir(mainPath, mainName, map[string][]byte{filepath.Base(mainPath): source}, nil, 0, false))
}

// LoadDirWithMainAST is LoadDirWithMain with the main already PARSED — an
// author document (pkg/dsl/author) read into an AST whose positions are
// the document's own — while the fragments its imports name are read
// beside mainPath on disk and parsed as ever. The three identities of such
// a unit are the caller's to name: mainPath is the `.bot` the document
// stands for, whose directory is the unit's root (`lib/` under it holds
// the fragments) and whose name is the main's Rel; mainName is the name
// every position of the main carries — the document's own path — and what
// its diagnostics point at; source is the document's text, which the
// unit's Digest covers in place of a `.bot`'s. The main is the unit's main
// whatever it declares: a document is never read as a fragment. f is read,
// never written: the unit's Merged is a copy of it with the fragments'
// declarations appended and the keyed blocks rebuilt, so the caller may go
// on reading, compiling or writing f, and may hand it over again. spent
// is what the main's own reading took of the read budget (author.Result's
// Spent): the fragments share what is left of it.
func LoadDirWithMainAST(mainPath, mainName string, f *ast.File, source []byte, spent int) (*Unit, error) {
	return whole(loadDir(mainPath, mainName, map[string][]byte{filepath.Base(mainPath): source}, f, spent, false))
}

// MembersDir is the Rel of every file the bot whose main is at mainPath
// reaches through its `import` lines — the main first, then the fragments
// in the order the imports reach them: the membership LoadDirStaged(mainPath,
// staged) reads, each file read where and as that load reads it. Only each
// file's head is parsed — the lines above its first declaration, where
// every import it can carry stands (one below is refused, E044, and
// followed by no load) — so no read budget the bot's declarations spend
// moves the answer: it is what a caller needs to tell whether a bot it
// cannot read whole reaches a file. The heads share a read budget of their
// own; one past it is refused like any reading (*ShortError).
func MembersDir(mainPath string, staged map[string][]byte) ([]string, error) {
	u, err := whole(loadDir(mainPath, "", staged, nil, 0, true))
	if err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(u.Files))
	for _, f := range u.Files {
		rels = append(rels, f.Rel)
	}
	return rels, nil
}

// headOf is src cut after its head: the lines from the top that are blank,
// a comment, or open with `dsl` or `import` (the trimmed line read), up to
// the first that does not. The parser's head — what it reads before the
// first declaration, where an `import` is followed — is never longer: a
// line that opens otherwise is a declaration, an indentation or an error to
// it, and each ends the head. A cut that runs past the parser's head reads
// the extra lines as the parser does in place — after the head, where an
// import is refused (E044) — so the head's imports read the same off the
// cut as off the whole text.
//
// Lines are not tokens: a raw string (a backtick literal, the one token that
// spans lines) opened on a head line runs past the line the cut stops at,
// and the head read off that cut would end inside it and drop the imports
// below its close. A cut that lands inside a token is no head, and the text
// is read whole instead — as the load reads it, its budget held.
func headOf(src string) string {
	src = strings.TrimPrefix(src, "\ufeff")
	head := lineHeadOf(src)
	if len(head) < len(src) && cutsAToken(head) {
		return src
	}
	return head
}

// cutsAToken reports whether the text ends inside a token: its lexing meets
// a literal it does not close.
func cutsAToken(head string) bool {
	for _, t := range parser.NewLexer("head", head).All() {
		if t.Type == parser.TokenError && t.Code == parser.DiagUnterminatedStr {
			return true
		}
	}
	return false
}

// lineHeadOf is headOf's cut on lines alone.
func lineHeadOf(src string) string {
	src = strings.TrimPrefix(src, "\ufeff")
	end := 0
	for end < len(src) {
		next := strings.IndexByte(src[end:], '\n')
		line := src[end:]
		if next >= 0 {
			line = src[end : end+next]
		}
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "dsl") && !strings.HasPrefix(t, "import") {
			break
		}
		if next < 0 {
			end = len(src)
			break
		}
		end += next + 1
	}
	return src[:end]
}

// FragmentOf reports whether the file at path, whose text is text, is a
// fragment of a bot: a file that declares no workflow below a directory
// named lib/. It returns the root of that bot — the parent of the nearest
// lib/ ancestor, where the bot's mains live — and the file's slash path
// from it. A text the reader refuses whole is no fragment it can tell.
func FragmentOf(path string, text []byte) (root, rel string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}
	return fragmentAlone(abs, text, nil)
}

func loadDir(mainPath, mainName string, staged map[string][]byte, mainAST *ast.File, mainSpent int, heads bool) *Unit {
	abs, err := filepath.Abs(mainPath)
	if err != nil {
		abs = mainPath
	}
	root := filepath.Dir(abs)
	mainRel := filepath.Base(abs)
	// A file that declares no workflow below a lib/ ancestor is a fragment
	// of the bot whose root holds that lib/: loaded alone — validated,
	// opened in the editor — it is read from that root under its lib/
	// name, so a sibling it imports by bare name resolves as it does
	// through the main, held to the same rule. A main — a file with a
	// workflow — stays where it is, whatever its directory is called, and
	// so does the confinement of its imports. A main handed over parsed is
	// held to the same rule, read off its AST instead of its text.
	if fragRoot, rel, ok := fragmentAlone(abs, staged[mainRel], mainAST); ok {
		root, mainRel = fragRoot, rel
		if len(staged) > 0 {
			prefix := path.Dir(rel)
			rebased := make(map[string][]byte, len(staged))
			for key, src := range staged {
				rebased[path.Join(prefix, key)] = src
			}
			staged = rebased
		}
	}
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	read := func(rel string) ([]byte, error) {
		if src, ok := staged[rel]; ok {
			return src, nil
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		if rel == mainRel {
			// The file the caller named is read as named — a symlink
			// followed, as every reader of a .bot always has; the
			// confinement below is for the files an import reaches.
			return os.ReadFile(full) // #nosec G304 -- the path the caller named
		}
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrOutside, rel)
		}
		real, err := filepath.EvalSymlinks(full)
		if err != nil {
			return nil, err
		}
		if !within(real, realRoot) {
			// The import is named as written, never by the resolved path:
			// the refusal travels to clients (a studio 422 names it), and a
			// host path outside the unit discloses the server's filesystem
			// layout (#1918).
			return nil, fmt.Errorf("%w: %s resolves outside the unit", ErrOutside, rel)
		}
		return os.ReadFile(real)
	}
	name := func(rel string) string {
		if mainName != "" && rel == mainRel {
			return mainName
		}
		return filepath.Join(root, filepath.FromSlash(rel))
	}
	u := load(read, mainRel, name, mainAST, mainSpent, heads)
	u.Root = root
	return u
}

// fragmentAlone reports whether the file at abs — its AST when the caller
// parsed it, else read from text when given, from disk otherwise — is a
// fragment loaded on its own: a file that declares no workflow below a
// directory named lib/. It returns the root of the bot that lib/ belongs
// to, the parent of the NEAREST lib/ ancestor, and the file's slash path
// from it. A file the reader refuses whole declares nothing it can see,
// workflow or not: it is loaded where it stands, and the load refuses it
// there.
func fragmentAlone(abs string, text []byte, parsed *ast.File) (root, rel string, ok bool) {
	root, rel, ok = LibRootOf(abs)
	if !ok {
		return "", "", false
	}
	if parsed == nil {
		if text == nil {
			b, err := os.ReadFile(abs) // #nosec G304 -- the path the caller named
			if err != nil {
				return "", "", false
			}
			text = b
		}
		pr, err := parser.Parse(abs, string(text))
		if err != nil {
			return "", "", false
		}
		parsed = pr.File
	}
	if parsed == nil || len(parsed.Workflows) > 0 {
		return "", "", false
	}
	return root, rel, true
}

// LibRootOf is where every unit that may import the file at abs (an
// absolute path) is rooted — the parent of the file's nearest ancestor
// named lib/, whatever the file holds — and the file's slash path from it:
// an import resolves only under a unit's lib/, so a file below no lib/ is
// no unit's fragment (ok false).
func LibRootOf(abs string) (root, rel string, ok bool) {
	for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
		if filepath.Base(d) == FragmentDir {
			root = filepath.Dir(d)
			break
		}
		if filepath.Dir(d) == d {
			return "", "", false
		}
	}
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return "", "", false
	}
	return root, filepath.ToSlash(r), true
}

// LoadMap loads the unit from a files map keyed by slash path relative to
// the root — a bot source, a snapshot — with main as the main's key.
// Files are parsed under their key.
func LoadMap(files map[string]string, main string) (*Unit, error) {
	return Load(mapReader(files), main, func(rel string) string { return rel })
}

// mapReader reads a files map keyed by slash path from the root.
func mapReader(files map[string]string) Reader {
	return func(rel string) ([]byte, error) {
		src, ok := files[rel]
		if !ok {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, rel)
		}
		return []byte(src), nil
	}
}

// Load loads the unit through read, starting at main, naming each file for
// the parser with name.
func Load(read Reader, main string, name func(rel string) string) (*Unit, error) {
	return whole(load(read, main, name, nil, 0, false))
}

// load is Load with the main's AST supplied, when the caller parsed it
// (LoadDirWithMainAST) and its reading spent mainSpent of the read budget;
// the main's text is still read for the digest.
func load(read Reader, main string, name func(rel string) string, mainAST *ast.File, mainSpent int, heads bool) *Unit {
	l := &loader{read: read, name: name, u: &Unit{Main: path.Clean(main)}, state: map[string]int{}, seenUnread: map[string]struct{}{}, mainAST: mainAST, budget: parser.NewUnitBudget(mainSpent), heads: heads}
	l.fragmentPrefix = fragmentPrefixOf(l.u.Main)
	l.visit(l.u.Main, nil, "")
	l.u.Files = l.files
	l.u.Unread = l.unread
	sort.Strings(l.u.Unread)
	l.merge()
	l.noteOwn()
	l.u.Digest = Digest(l.u.Files)
	if !l.u.Whole() {
		l.u.Digest = shortDigest(l.u.Digest)
	}
	return l.u
}

// shortDigest is the Digest of a load cut short (Unit.Whole) whose files
// digest to whole: another revision than any whole load's.
func shortDigest(whole string) string {
	sum := sha256.Sum256([]byte("not read whole\x00" + whole))
	return hex.EncodeToString(sum[:])
}

// Digest is the unit's source identity: sha256 over the files' relative
// paths and their contents' digests, sorted by path.
func Digest(files []File) string {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })
	h := sha256.New()
	for _, f := range sorted {
		sum := sha256.Sum256(f.Source)
		h.Write([]byte(f.Rel))
		h.Write([]byte{0})
		h.Write(sum[:])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

const (
	stateLoading = -1
)

type loader struct {
	read  Reader
	name  func(rel string) string
	u     *Unit
	files []File
	// state maps a rel to its index in files once loaded, stateLoading
	// while its imports are being followed (a cycle meets it there), and
	// is absent for a file never reached.
	state map[string]int
	// unread collects the rel of every fragment whose read was attempted
	// and failed, in the ORDER the imports reached them, and seenUnread
	// keeps it a set: the same missing fragment imported from two files is
	// one file to write. A slice rather than a map drained at the end,
	// because load sorts it: a value built from a map's iteration already
	// arrives in a different order on every run, so a test for that sort
	// could only fail by chance.
	unread     []string
	seenUnread map[string]struct{}
	stack      []string
	// fragmentPrefix is where this unit's fragments live, from the root.
	fragmentPrefix string
	// mainAST is the main's AST when the caller parsed the main itself;
	// nil when the loader parses it like every other file.
	mainAST *ast.File
	// own is the report of the loader's own diagnostics, and ownAt where
	// in the unit's list each one it kept stands (add).
	own   parser.Report
	ownAt []int
	// budget is the read budget the unit's files share: every file the
	// load parses spends it, so a bot in ten files costs what it does in
	// one. Once it is out (out), no further file is read, and the bot is
	// refused once, where it ran out — in the file whose reading spent the
	// last of it, or at the import of the first file it had none left for
	// (read to know it is there, never parsed).
	budget *parser.Budget
	out    bool
	// heads reads each file's head alone (headOf): the membership a
	// MembersDir answers, with no declaration read.
	heads bool
}

// add records one of the loader's own diagnostics: kept by its report, it
// joins the unit's list where the load met it — after the parse
// diagnostics of the file that led to it — and the last one kept of a
// code learns at the end how many more there were (noteOwn).
func (l *loader) add(d parser.Diagnostic) bool {
	if !l.own.Add(d) {
		return false
	}
	l.u.Diagnostics = append(l.u.Diagnostics, d)
	l.ownAt = append(l.ownAt, len(l.u.Diagnostics)-1)
	return true
}

// noteOwn puts the loader's report back in the unit's list — the same
// diagnostics, the last one kept of each code past the cap saying how many
// more there were — and counts the errors the list does not hold.
func (l *loader) noteOwn() {
	kept := l.own.Diagnostics()
	for i, d := range kept {
		l.u.Diagnostics[l.ownAt[i]] = d
	}
	l.u.unreported = l.own.Count(parser.SeverityError) - errorsIn(kept)
	for _, f := range l.u.Files {
		l.u.unreported += f.parseUnreported
	}
}

// recordUnread notes a fragment whose read was attempted and failed, once
// and in the order the imports reach it.
func (l *loader) recordUnread(rel string) {
	if _, seen := l.seenUnread[rel]; seen {
		return
	}
	l.seenUnread[rel] = struct{}{}
	l.unread = append(l.unread, rel)
}

// fragmentPrefixOf is the slash prefix under which the fragments of the
// bot whose main is mainRel live: `lib/` under the main's directory — or,
// for a main that is itself a fragment below a lib/, that same lib/ (the
// nearest one above it).
func fragmentPrefixOf(mainRel string) string {
	dir := path.Dir(mainRel)
	for d := dir; d != "." && d != "/"; d = path.Dir(d) {
		if path.Base(d) == FragmentDir {
			dir = path.Dir(d)
			break
		}
	}
	if dir == "." {
		return FragmentDir + "/"
	}
	return path.Join(dir, FragmentDir) + "/"
}

func (l *loader) visit(rel string, from *ast.ImportDecl, fromName string) {
	if st, seen := l.state[rel]; seen {
		if st == stateLoading {
			l.diag(parser.DiagImportCycle, fromName, from, fmt.Sprintf("import cycle: %s → %s", strings.Join(l.stack, " → "), rel))
		}
		return
	}
	isMain := from == nil
	if !isMain && !strings.HasPrefix(rel, l.fragmentPrefix) {
		l.diag(parser.DiagBadImportPath, fromName, from, fmt.Sprintf("import %q resolves to %s, outside the bot's `%s/` directory where every fragment lives", from.Path, rel, FragmentDir))
		return
	}
	if !isMain && l.out {
		// The budget ran out already, and the bot is refused where it did:
		// no further file is read.
		return
	}
	src, err := l.read(rel)
	if err == nil && !isMain && l.budget.Left() <= 0 {
		// The fragment is there, and its reading would spend a record the
		// files before it left none of — its end at least: the bot is
		// refused at its import, once. Read first, so an import naming no
		// file is said as that, never as a budget it did not spend.
		l.out = true
		d := parser.Bounded(parser.Diagnostic{Code: parser.DiagUnexpectedToken, Severity: parser.SeverityError, File: fromName, Message: parser.UnitBudgetMessage(l.budget.Limit()), Hint: parser.ReadBudgetHint})
		if from != nil {
			d.Line, d.Column = from.Span.Start.Line, from.Span.Start.Column
		}
		l.refuse(d, parser.BoundBudget)
		return
	}
	if err != nil {
		if !isMain {
			// The read was attempted and failed: this fragment's arrival —
			// or its repair — is what would change this load. Recorded
			// here, where the loader KNOWS, since nothing downstream can
			// tell it from a fragment the unit simply does not import.
			l.recordUnread(rel)
		}
		switch {
		case isMain:
			l.add(parser.Bounded(parser.Diagnostic{Code: parser.DiagImportUnreadable, Severity: parser.SeverityError, File: l.name(rel), Line: 1, Column: 1, Message: fmt.Sprintf("cannot read %s: %v", rel, err), Hint: unreadHint(err)}))
		case errors.Is(err, os.ErrNotExist):
			l.diag(parser.DiagImportUnreadable, fromName, from, fmt.Sprintf("import %q: no such fragment (%s)", from.Path, rel))
		default:
			// The fragment is there and is not read — the root's
			// confinement refuses it (a symlink, a path resolving outside),
			// or its permissions do: what it declares reaches no document,
			// so a full load is short, its refusal this diagnostic (Whole).
			// One that is not there declares nothing to lose; a membership
			// (heads) answers the files it can read.
			msg := fmt.Sprintf("import %q: cannot read %s: %v", from.Path, rel, err)
			if errors.Is(err, ErrOutside) {
				msg = fmt.Sprintf("import %q is not read: %v", from.Path, err)
			}
			d := l.diagAt(parser.DiagImportUnreadable, fromName, from, msg)
			d.Hint = unreadHint(err)
			if l.heads {
				l.add(d)
			} else {
				l.refuse(d, parser.BoundNone)
			}
		}
		return
	}
	name := l.name(rel)
	l.state[rel] = stateLoading
	l.stack = append(l.stack, rel)
	var pr *parser.ParseResult
	if isMain && l.mainAST != nil {
		// Parsed by the caller: its diagnostics are the caller's, and a
		// document has no profile-1 reads of its own text.
		pr = &parser.ParseResult{File: l.mainAST}
	} else {
		text := string(src)
		if l.heads {
			text = headOf(text)
		}
		var err error
		pr, err = parser.ParseSpending(name, text, l.budget)
		var refused *parser.ReadRefusal
		if errors.As(err, &refused) {
			// The file is read as nothing: no declaration of it reaches the
			// unit, and its one diagnostic is the refusal — the load is short
			// (refusedAt), and only Salvage's view shows what it read.
			pr = &parser.ParseResult{File: &ast.File{}, Diagnostics: []parser.Diagnostic{refused.Diagnostic}, ErrorCount: 1, Refused: refused.Bound}
		}
		l.out = l.out || pr.Refused == parser.BoundBudget
	}
	if (pr.Refused != parser.BoundNone || pr.File == nil) && l.u.refusedAt == 0 && len(pr.Diagnostics) > 0 {
		// The file is read as nothing — refused whole by a bound, or no
		// text the reader reads (not UTF-8, E006: no File) — and its one
		// diagnostic is the refusal: the load did not read it, so it is
		// short.
		l.u.refusedAt, l.u.refusedBound = len(l.u.Diagnostics)+1, pr.Refused
	}
	l.u.Diagnostics = append(l.u.Diagnostics, pr.Diagnostics...)
	f := File{Rel: rel, Name: name, Source: src, AST: pr.File, ProfileReads: pr.ProfileReads, ParseDiagnostics: pr.Diagnostics, parseUnreported: pr.ErrorCount - errorsIn(pr.Diagnostics), Refused: pr.Refused}
	if pr.File != nil {
		f.Profile = pr.File.EffectiveProfile()
	}
	l.files = append(l.files, f)
	index := len(l.files) - 1
	if pr.File != nil {
		base := path.Dir(rel)
		for _, im := range pr.File.Imports {
			child := path.Clean(path.Join(base, im.Path))
			l.visit(child, im, name)
		}
	}
	l.stack = l.stack[:len(l.stack)-1]
	l.state[rel] = index
}

// diag records a loader diagnostic at the import that led to it.
func (l *loader) diag(code parser.DiagCode, file string, at *ast.ImportDecl, msg string) {
	l.add(l.diagAt(code, file, at, msg))
}

// diagAt is a loader diagnostic positioned at the import that led to it.
func (l *loader) diagAt(code parser.DiagCode, file string, at *ast.ImportDecl, msg string) parser.Diagnostic {
	d := parser.Bounded(parser.Diagnostic{Code: code, Severity: parser.SeverityError, File: file, Message: msg, Hint: parser.HintFor(code)})
	if at != nil {
		d.Line, d.Column = at.Span.Start.Line, at.Span.Start.Column
	}
	return d
}

// refuse records d as the refusal that leaves the load short (Whole), with
// the bound it met. The first one is the load's Refusal: it joins the
// unit's list outside the loader's report, so no cap on its code drops it —
// and no clip of the report bounds it either: it is bounded here, at the
// door it passes (parser.Bounded, #2801); a later one is a diagnostic like
// any.
func (l *loader) refuse(d parser.Diagnostic, bound parser.Bound) {
	d = parser.Bounded(d)
	if l.u.refusedAt != 0 {
		l.add(d)
		return
	}
	l.u.Diagnostics = append(l.u.Diagnostics, d)
	l.u.refusedAt, l.u.refusedBound = len(l.u.Diagnostics), bound
}

// ---- merging ----

// merge builds Merged: the main's AST, then every fragment's declarations
// appended in the order the files were reached. Every slice field of
// ast.File is concatenated by reflection — a kind added to the AST later
// is merged without anyone remembering to list it — and the four keyed
// blocks (vars, presets, attachments, secrets) merge by key. A name
// declared in two files, or a key declared twice anywhere, is E010 naming
// both places; the compiler's own uniqueness checks then see no cross-file
// duplicate and report the rest as ever.
func (l *loader) merge() {
	if len(l.files) == 0 || l.files[0].AST == nil {
		l.u.Merged = &ast.File{}
		return
	}
	main := l.files[0]
	merged := *main.AST
	merged.Imports = nil
	mv := reflect.ValueOf(&merged).Elem()
	// Fresh slices, so appending never writes into the main's own arrays.
	for i := 0; i < mv.NumField(); i++ {
		if fv := mv.Field(i); fv.Kind() == reflect.Slice && fv.Len() > 0 {
			fresh := reflect.MakeSlice(fv.Type(), 0, fv.Len())
			fv.Set(reflect.AppendSlice(fresh, fv))
		}
	}
	for _, f := range l.files[1:] {
		if f.AST == nil {
			continue
		}
		fv := reflect.ValueOf(f.AST).Elem()
		for i := 0; i < mv.NumField(); i++ {
			field := mv.Type().Field(i)
			if field.Name == "Imports" || mv.Field(i).Kind() != reflect.Slice {
				continue
			}
			mv.Field(i).Set(reflect.AppendSlice(mv.Field(i), fv.Field(i)))
		}
	}
	l.mergeKeyedBlocks(&merged)
	dedupeInlinePrompts(&merged)
	l.canonicaliseSubbots(&merged)
	l.checkNamedDuplicates(&merged)
	l.u.Merged = &merged
}

// dedupeInlinePrompts keeps one of the inline prompts several files wrote
// with the same text: the same text is one prompt (parser.InlinePromptName),
// declared once per file under the name its body gives it. The first —
// the main's, or the first fragment's — stays with its provenance; a save
// by provenance puts one back in every file whose nodes refer to it. A
// different body under the same name stays, for E010 to name.
func dedupeInlinePrompts(merged *ast.File) {
	seen := map[string]string{}
	kept := merged.Prompts[:0]
	for _, p := range merged.Prompts {
		if p.Inline {
			if body, ok := seen[p.Name]; ok && body == p.Body {
				continue
			}
			seen[p.Name] = p.Body
		}
		kept = append(kept, p)
	}
	merged.Prompts = kept
}

// mergeKeyedBlocks merges the at-most-one blocks by key: a fragment's block
// joins the main's (or becomes it); a key declared twice, in one block or
// across files, is E010 naming both.
func (l *loader) mergeKeyedBlocks(merged *ast.File) {
	// vars
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.VarField
		var block *ast.VarsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Vars == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Vars
				block = &b
			}
			for _, v := range f.AST.Vars.Fields {
				if l.dup(seen, "var", v.Name, v.Span.Start) {
					continue
				}
				fields = append(fields, v)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Vars = block
		}
	}
	// presets
	{
		seen := map[string]ast.Pos{}
		var entries []*ast.Preset
		var block *ast.PresetsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Presets == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Presets
				block = &b
			}
			for _, p := range f.AST.Presets.Entries {
				if l.dup(seen, "preset", p.Name, p.Span.Start) {
					continue
				}
				entries = append(entries, p)
			}
		}
		if block != nil {
			block.Entries = entries
			merged.Presets = block
		}
	}
	// attachments
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.AttachmentField
		var block *ast.AttachmentsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Attachments == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Attachments
				block = &b
			}
			for _, a := range f.AST.Attachments.Fields {
				if l.dup(seen, "attachment", a.Name, a.Span.Start) {
					continue
				}
				fields = append(fields, a)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Attachments = block
		}
	}
	// secrets
	{
		seen := map[string]ast.Pos{}
		var fields []*ast.SecretField
		var block *ast.SecretsBlock
		for _, f := range l.files {
			if f.AST == nil || f.AST.Secrets == nil {
				continue
			}
			if block == nil {
				b := *f.AST.Secrets
				block = &b
			}
			for _, s := range f.AST.Secrets.Fields {
				if l.dup(seen, "secret", s.Name, s.Span.Start) {
					continue
				}
				fields = append(fields, s)
			}
		}
		if block != nil {
			block.Fields = fields
			merged.Secrets = block
		}
	}
}

// dup records name's first position and reports (E010) a second one, in
// the same file or another. It returns true for the duplicate.
func (l *loader) dup(seen map[string]ast.Pos, kind, name string, at ast.Pos) bool {
	first, ok := seen[name]
	if !ok {
		seen[name] = at
		return false
	}
	l.add(parser.Bounded(parser.Diagnostic{
		Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
		File: at.File, Line: at.Line, Column: at.Column,
		Message: fmt.Sprintf("%s %q is declared twice: here and at %s:%d", kind, name, l.relOf(first.File), first.Line),
		Hint:    parser.HintFor(parser.DiagDuplicateDecl),
	}))
	return true
}

// relOf is the unit-relative path of a parse name, for messages.
func (l *loader) relOf(name string) string {
	for _, f := range l.files {
		if f.Name == name {
			return f.Rel
		}
	}
	return name
}

// nodeKinds are the declaration kinds that share the node namespace
// (C041: a node ID is unique across all kinds); every other named kind has
// a namespace of its own — a group is a macro and a `use` prefix names
// `<prefix>.<node>` ids, so neither collides with a node, in one file or
// in two. Names are the ast.File field names; TestSplitPreservesAcceptance
// holds the list to the compiler's own rule.
var nodeKinds = map[string]bool{
	"Agents": true, "Judges": true, "Routers": true, "Humans": true, "Tools": true, "Computes": true,
	"Emits": true, "Waits": true, "AwaitAnswers": true, "Fails": true, "Subbots": true,
}

// checkNamedDuplicates reports (E010) a name declared in two DIFFERENT
// files of the unit — walking every slice field of ast.File whose elements
// carry a Name (or, for a `use`, a Prefix) — and a second workflow. A
// duplicate inside one file is left to the compiler, which reports it
// today; two files, it could not have named.
func (l *loader) checkNamedDuplicates(merged *ast.File) {
	type first struct {
		file string
		line int
	}
	seen := map[string]first{} // namespace + "\x00" + name
	if len(merged.Workflows) > 1 {
		w0, w1 := merged.Workflows[0], merged.Workflows[1]
		l.add(parser.Bounded(parser.Diagnostic{
			Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
			File: w1.Span.Start.File, Line: w1.Span.Start.Line, Column: w1.Span.Start.Column,
			Message: fmt.Sprintf("a unit has one workflow: %q here and %q at %s:%d", w1.Name, w0.Name, l.relOf(w0.Span.Start.File), w0.Span.Start.Line),
			Hint:    parser.HintFor(parser.DiagDuplicateDecl),
		}))
	}
	mv := reflect.ValueOf(merged).Elem()
	for i := 0; i < mv.NumField(); i++ {
		field := mv.Type().Field(i)
		fv := mv.Field(i)
		if fv.Kind() != reflect.Slice || field.Name == "Workflows" || field.Name == "Comments" {
			continue
		}
		namespace := field.Name
		if nodeKinds[field.Name] {
			namespace = "node"
		}
		for j := 0; j < fv.Len(); j++ {
			el := fv.Index(j)
			if el.Kind() == reflect.Pointer {
				el = el.Elem()
			}
			if el.Kind() != reflect.Struct {
				continue
			}
			name, pos, ok := namedAt(el)
			if !ok || name == "" {
				continue
			}
			key := namespace + "\x00" + name
			if prev, dup := seen[key]; dup {
				if prev.file != pos.File {
					l.add(parser.Bounded(parser.Diagnostic{
						Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
						File: pos.File, Line: pos.Line, Column: pos.Column,
						Message: fmt.Sprintf("%s %q is declared in two files: here and at %s:%d", kindWord(field.Name), name, l.relOf(prev.file), prev.line),
						Hint:    parser.HintFor(parser.DiagDuplicateDecl),
					}))
				}
				continue
			}
			seen[key] = first{pos.File, pos.Line}
		}
	}
}

// namedAt reads a declaration's name (its Name field, or the Prefix of a
// `use`) and its start position.
func namedAt(el reflect.Value) (string, ast.Pos, bool) {
	nameField := el.FieldByName("Name")
	if !nameField.IsValid() || nameField.Kind() != reflect.String {
		nameField = el.FieldByName("Prefix")
	}
	if !nameField.IsValid() || nameField.Kind() != reflect.String {
		return "", ast.Pos{}, false
	}
	span := el.FieldByName("Span")
	if !span.IsValid() {
		return nameField.String(), ast.Pos{}, true
	}
	s, ok := span.Interface().(ast.Span)
	if !ok {
		return nameField.String(), ast.Pos{}, true
	}
	return nameField.String(), s.Start, true
}

// kindWord turns an ast.File field name into the word a message uses:
// "Agents" → "agent", "AwaitAnswers" → "await_answers", "MCPServers" →
// "mcp_server".
func kindWord(field string) string {
	switch field {
	case "MCPServers":
		return "mcp_server"
	case "AwaitAnswers":
		return "await_answers"
	}
	var b strings.Builder
	for i, r := range field {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.TrimSuffix(strings.ToLower(b.String()), "s")
}

// canonicaliseSubbots rewrites, on the merged copy only, the `source:` of
// every subbot declared in a fragment so it is relative to the unit's root
// — the base every host resolves a child against — instead of the
// fragment's own directory, which is what the author wrote it against.
// The fragment's AST keeps the text as written, for the save by provenance.
func (l *loader) canonicaliseSubbots(merged *ast.File) {
	relByName := map[string]string{}
	for _, f := range l.files {
		relByName[f.Name] = f.Rel
	}
	for i, sb := range merged.Subbots {
		rel, ok := relByName[sb.Span.Start.File]
		if !ok || rel == l.u.Main || sb.Source == "" || filepath.IsAbs(sb.Source) || strings.HasPrefix(sb.Source, "/") {
			continue
		}
		cp := *sb
		cp.Source = CanonicalSubbotSource(rel, sb.Source)
		merged.Subbots[i] = &cp
	}
}

// CanonicalSubbotSource is a subbot's `source:` as every host resolves it
// — relative to the unit's root — given the file it is declared in (a
// slash path from the root) and the source as written there, relative to
// that file's directory. An absolute source is returned as is.
func CanonicalSubbotSource(ownerRel, source string) string {
	if source == "" || filepath.IsAbs(source) || strings.HasPrefix(source, "/") {
		return source
	}
	return path.Clean(path.Join(path.Dir(ownerRel), filepath.ToSlash(source)))
}

// AuthoredSubbotSource is the inverse of CanonicalSubbotSource: the source
// to write in ownerRel for a root-relative one — what a save by provenance
// puts back in a fragment. For a file at the root the two are the same.
func AuthoredSubbotSource(ownerRel, canonical string) string {
	if canonical == "" || filepath.IsAbs(canonical) || strings.HasPrefix(canonical, "/") {
		return canonical
	}
	dir := path.Dir(ownerRel)
	if dir == "." {
		return canonical
	}
	rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(canonical))
	if err != nil {
		return canonical
	}
	return filepath.ToSlash(rel)
}

// within reports whether p lies under root (both resolved).
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
