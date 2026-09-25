package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/canon"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

var (
	// ErrFmtToNeedsFiles refuses `--to` on a directory: a conversion writes
	// a file BESIDE the one it reads, and a walk would write trees.
	ErrFmtToNeedsFiles = errors.New("fmt: --to converts named files, not directories")
	// ErrFmtToKind refuses a file of the wrong kind for the direction asked.
	ErrFmtToKind = errors.New("fmt: --to bot converts an author document (x.bot.yaml), --to yaml a .bot")
	// ErrFmtToDestination refuses a conversion whose destination its
	// readers do not take by name: `UPPER.BOT.YAML` stands for `UPPER.BOT`,
	// which `fmt`, a walk and `--to yaml` do not read (a workflow file's
	// suffix is `.bot`, lower-case) — written, it would be a .bot only its
	// launcher reads, and no twin.
	ErrFmtToDestination = errors.New("fmt: --to would write a file its readers do not take by name")
	// ErrFmtToBaseline refuses --baseline with --to: a baseline lists what a
	// CHECK of canonical forms tolerates, and a conversion is not one.
	ErrFmtToBaseline = errors.New("fmt: --baseline does not apply to --to")
)

// writeDocument renders a program as an author document — author.Write; a
// seam for the test that holds convertTwin's read-back proof against a
// writer defect, which no known program triggers.
var writeDocument = author.Write

// twinPath is the file a conversion writes: the .bot an author document
// stands for (`x.bot.yaml` → `x.bot`, the `.yaml` off whatever its case),
// or the author document of a .bot (`x.bot` → `x.bot.yaml`).
func twinPath(to, path string) string {
	if to == "bot" {
		return path[:len(path)-len(".yaml")]
	}
	return path + ".yaml"
}

// refuse records one file fmt would not write, with why: for the report,
// and by path for a baseline.
func (r *FmtResult) refuse(path, why string) {
	r.Refused = append(r.Refused, path+": "+why)
	r.RefusedPaths = append(r.RefusedPaths, canon.NormalizeBaselinePath(path))
}

// runFmtConvert is `fmt --to bot|yaml`: each named file converted to its
// twin beside it — x.bot.yaml → x.bot, x.bot → x.bot.yaml — proven the same
// program before it is written. A destination already there and not what
// the source writes is refused without --force, one that is already it is
// a no-op, one that is there and is not a file (a directory) or cannot be
// read is refused whatever --force says (twinAt) while the files named
// beside it are converted, and --check reports what a write would do — the
// refusal included: the CI form of "is the .bot beside the document the one
// it writes?".
func runFmtConvert(opts FmtOptions) (FmtResult, error) {
	var res FmtResult
	if opts.Baseline != "" {
		return res, ErrFmtToBaseline
	}
	if opts.To != "bot" && opts.To != "yaml" {
		return res, fmt.Errorf("fmt: --to takes bot or yaml, not %q", opts.To)
	}
	for _, p := range opts.Paths {
		info, err := os.Stat(p)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		if info.IsDir() {
			return res, fmt.Errorf("%w: %s", ErrFmtToNeedsFiles, p)
		}
		if !info.Mode().IsRegular() {
			return res, fmt.Errorf("fmt: %s is not a regular file", p)
		}
		isDoc := workflowfile.IsAuthorDocument(p)
		if (opts.To == "bot") != isDoc || (opts.To == "yaml" && !workflowfile.IsWorkflowFile(p)) {
			return res, fmt.Errorf("%w: %s", ErrFmtToKind, p)
		}
		if dest, ok := twinNameReadBack(opts.To, p); !ok {
			return res, fmt.Errorf("%w: %s", ErrFmtToDestination, twinNameRefusal("fmt", p, dest))
		}
	}
	changed := false
	for _, path := range opts.Paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		dest, out, notices, err := convertTwin(opts.To, path, raw)
		if err != nil {
			res.refuse(path, strings.TrimPrefix(err.Error(), canon.ErrRefused.Error()+": ")+". Nothing written")
			continue
		}
		res.Notices = append(res.Notices, notices...)
		existing, there, why := twinAt(dest)
		if why != "" {
			res.refuse(dest, why+"; nothing is written there. Left as it is")
			continue
		}
		f := FmtFile{Path: dest, From: path}
		switch {
		case there && bytes.Equal(existing, out):
			// The destination is already what the source writes.
		case there && !opts.Force:
			// The same under --check: a check says what the write would
			// do, and without --force the write refuses.
			res.refuse(dest, "is there and is not what "+path+" writes; --force overwrites it. Left as it is")
			continue
		default:
			if there {
				// --force replaces the file whole: what it carried and the
				// text replacing it does not is said, not dropped in silence
				// (under --check too — a check says what the write would do).
				if note := forceNote(opts.To, dest, path, existing, out); note != "" {
					res.Notices = append(res.Notices, note)
				}
			}
			f.Changed = true
			changed = true
			if !opts.Check {
				perm := os.FileMode(0o644)
				if info, err := os.Stat(path); err == nil {
					perm = info.Mode().Perm()
				}
				if err := writeFileAtomic(dest, out, perm); err != nil {
					return res, fmt.Errorf("fmt: %w", err)
				}
				f.Written = true
			}
		}
		res.Files = append(res.Files, f)
	}
	reportFmt(opts, res)
	if len(res.Refused) > 0 {
		return res, ErrFmtRefused
	}
	if opts.Check && changed {
		return res, ErrFmtWouldChange
	}
	return res, nil
}

// twinAt is what is at the path of a twin — the .bot an author document
// stands for, or the document of a .bot: a readable file's bytes, nothing
// (there false), or why no twin can be written there whatever --force says:
// a path that cannot be looked at, one that is there and is not a file (a
// directory, a device), a file that cannot be read. fmt refuses to write
// over such a path — that file's alone, the files named beside it are
// converted all the same — and a surface reading a document refuses it as
// the .bot it stands for, in the same words.
func twinAt(dest string) (existing []byte, there bool, why string) {
	info, err := os.Stat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, ""
	case err != nil:
		return nil, false, "cannot be looked at (" + err.Error() + ")"
	case !info.Mode().IsRegular():
		kind := "special file"
		if info.IsDir() {
			kind = "directory"
		}
		return nil, true, "is there and is a " + kind + ", not a file"
	}
	existing, err = os.ReadFile(dest)
	if err != nil {
		return nil, true, "is there and cannot be read (" + err.Error() + ")"
	}
	return existing, true, ""
}

// twinNameReadBack is the twin of path — the .bot a document stands for, or
// the document of a .bot — and whether it is a name the surfaces that read
// it take back: a document's suffix is read case-folded, a workflow file's
// is not, so `UPPER.BOT.YAML` stands for `UPPER.BOT`, a name no .bot surface
// reads as a workflow file.
func twinNameReadBack(to, path string) (dest string, ok bool) {
	dest = twinPath(to, path)
	return dest, workflowfile.IsWorkflowFile(dest) || workflowfile.IsAuthorDocument(dest)
}

// twinNameRefusal says why surface refuses path, whose twin dest is a name
// twinNameReadBack does not take.
func twinNameRefusal(surface, path, dest string) string {
	return fmt.Sprintf("%s stands for %s, a name `%s` would not read back (a workflow file ends in `.bot`, lower-case) — rename the document first", path, dest, surface)
}

// convertTwin writes the twin of one file: the .bot an author document
// stands for, proven to read back as the program the document describes
// (unparse.Verify — the check `validate` reports as E054), with a notice
// for what the .bot reads otherwise than the document wrote it (E053: the
// .bot carries the reading) and one for the document's YAML comments, which
// the .bot is not written with; or the author document of a .bot, from a text
// that parses without an error — never from one the parser recovered on —
// proven to read back as the same program (ir.SameProgram, declaration by
// declaration where nothing compiles to a workflow), with a notice
// for what the document does not carry: a frontmatter the catalog reader
// does not read, the keys beyond the four of `catalog:`, the ordinary
// comments.
func convertTwin(to, path string, raw []byte) (dest string, out []byte, notices []string, err error) {
	dest = twinPath(to, path)
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		abs = path
	}
	switch to {
	case "bot":
		res := author.Parse(abs, raw)
		if res.HasErrors() {
			return dest, nil, nil, fmt.Errorf("%w: does not read: %s", canon.ErrRefused, diagnosticErrors(namedAs(res.Diagnostics, path)))
		}
		for _, d := range namedAs(res.Diagnostics, path) {
			notices = append(notices, d.Error())
		}
		text := unparse.Unparse(res.File)
		if verr := unparse.Verify(res.File, text); verr != nil {
			return dest, nil, nil, fmt.Errorf("%w: has no written .bot form: %v", canon.ErrRefused, verr)
		}
		if comments := author.Comments(raw); len(comments) > 0 {
			notices = append(notices, fmt.Sprintf("%s: %d YAML comment line(s) are not carried into the .bot (the first: %s) — the .bot is written from the program; a ` #` inside a plain value starts one: quote the value when the `#` is part of it", path, len(comments), comments[0]))
		}
		return dest, []byte(text), notices, nil
	case "yaml":
		pr := parser.Parse(abs, string(raw))
		if errs := diagnosticErrors(namedAs(pr.Diagnostics, path)); errs != "" {
			return dest, nil, nil, fmt.Errorf("%w: does not parse: %s — a document is written from a program, never from a text the parser recovered on", canon.ErrRefused, errs)
		}
		out, werr := writeDocument(pr.File)
		if werr != nil {
			return dest, nil, nil, fmt.Errorf("%w: cannot be written as a document: %v", canon.ErrRefused, werr)
		}
		// The proof before the write: the document reads back — a value the
		// writer spells and the reader refuses is refused here, not by the
		// next validate — as the same program. It is read under the .bot's
		// own name: the document is not on disk yet, and an {{include}}
		// resolves only beside a file that is (promptSourceDir); the .bot
		// is, in the same directory. Its diagnostics are named as dest.
		back := author.Parse(abs, out)
		if back.HasErrors() {
			return dest, nil, nil, fmt.Errorf("%w: cannot be written as a document: the document written does not read back: %s", canon.ErrRefused, diagnosticErrors(namedAs(back.Diagnostics, dest)))
		}
		ca, cb := ir.Compile(pr.File), ir.Compile(back.File)
		why := ir.SameProgram(ca, cb)
		if why == "" && (ca.Workflow == nil || cb.Workflow == nil) {
			// No compiled program to compare — a fragment under lib/, a
			// file of schemas or prompts, a bot that does not compile yet:
			// SameProgram compares diagnostic codes only there.
			why = sameDeclarations(pr.File, back.File)
		}
		if why != "" {
			return dest, nil, nil, fmt.Errorf("%w: cannot be written as a document: the document written reads back as another program: %s", canon.ErrRefused, why)
		}
		// Whether the document carries a `catalog:` is read off the bytes
		// written: the notes state what they hold, not what the writer meant.
		carries := documentCarriesCatalog(out)
		notices = append(notices, frontmatterNotices(path, pr.File, carries)...)
		if n := commentsOutsideFrontmatter(pr.File, carries); n > 0 {
			notices = append(notices, fmt.Sprintf("%s: %d comment line(s) are not represented in the document — a draft carries the catalog only; the .bot keeps them until `fmt --to bot --force` replaces it from the document", path, n))
		}
		return dest, out, notices, nil
	}
	return "", nil, nil, fmt.Errorf("fmt: --to takes bot or yaml, not %q", to)
}

// sameDeclarations compares, declaration by declaration, a .bot and the
// program its document reads back as, when there is no compiled program to
// compare: their span-free mirrors (ast.MarshalFileWithoutComments — the
// document carries no comment), the profile as the text reads it, the
// inline prompts by name (named after their bodies, an order that says
// nothing) and every literal by its value (the writer spells `01.5` as
// `1.5`, `010` as `10`). "" when they are one.
func sameDeclarations(bot, back *ast.File) string {
	a, err := declarationMirror(bot)
	if err != nil {
		return "the .bot cannot be compared: " + err.Error()
	}
	b, err := declarationMirror(back)
	if err != nil {
		return "the document cannot be compared: " + err.Error()
	}
	if bytes.Equal(a, b) {
		return ""
	}
	return unparse.FirstJSONDifference(a, b)
}

// declarationMirror is f's mirror as sameDeclarations compares it, taken
// off a copy: the JSON transport is the deep copy, comments left out.
func declarationMirror(f *ast.File) ([]byte, error) {
	raw, err := ast.MarshalFileWithoutComments(f)
	if err != nil {
		return nil, err
	}
	c, err := ast.UnmarshalFile(raw)
	if err != nil {
		return nil, err
	}
	c.Profile = f.EffectiveProfile()
	c.Prompts = unparse.InlinePromptsLast(c.Prompts)
	literalsByValue(reflect.ValueOf(c))
	return ast.MarshalFile(c)
}

var literalType = reflect.TypeOf(ast.Literal{})

// literalsByValue drops the spelling of every literal under v — a walk
// over every exported field, so a literal the AST gains later is compared
// by its value without anyone listing it.
func literalsByValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			literalsByValue(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == literalType {
			if v.CanSet() {
				v.FieldByName("Raw").SetString("")
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				literalsByValue(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // bytes (a json.RawMessage) hold no literal
		}
		for i := 0; i < v.Len(); i++ {
			literalsByValue(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			literalsByValue(v.MapIndex(k))
		}
	}
}

// namedAs gives the diagnostics the file name the command was given —
// the parse carried the absolute path, so an include resolves beside the
// file — so a refusal or a note names the file once, as the user wrote it.
func namedAs(diags []parser.Diagnostic, name string) []parser.Diagnostic {
	out := make([]parser.Diagnostic, len(diags))
	for i, d := range diags {
		d.File = name
		out[i] = d
	}
	return out
}

// diagnosticErrors joins the error-severity diagnostics, "" when none.
func diagnosticErrors(diags []parser.Diagnostic) string {
	var errs []string
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	return strings.Join(errs, "; ")
}

// frontmatterNotices says what the document written carries of the .bot's
// frontmatter, and what it does not. carries is whether the document
// written has a `catalog:` at all (documentCarriesCatalog), never
// re-derived here; when it does not, the reason is the one reading's —
// the block parser.Frontmatter takes off the head and
// workflowfile.DecodeFrontmatter decodes, as the writer and the catalogue
// do: a block that is not closed, one the reading does not read, one with
// none of the four keys. The keys beyond the four are said by name.
func frontmatterNotices(path string, f *ast.File, carries bool) []string {
	block := parser.Frontmatter(f)
	if !block.Found {
		return nil
	}
	var (
		ident *workflowfile.Frontmatter
		extra []string
		err   error
	)
	if block.Closed {
		ident, extra, err = workflowfile.DecodeFrontmatter(strings.Join(block.Lines, "\n"))
	}
	var notes []string
	if !carries {
		why := "the writer wrote none"
		switch {
		case !block.Closed:
			why = "the frontmatter block is not closed (no second `## ---`)"
		case err != nil:
			why = "the frontmatter is not YAML the catalog reader reads (" + strings.Join(strings.Fields(err.Error()), " ") + ")"
		case ident.Empty():
			why = "the frontmatter carries none of the four keys of `catalog:` (" + strings.Join(workflowfile.FrontmatterKeys, ", ") + ")"
		}
		notes = append(notes, path+": "+why+" — the document carries no `catalog:`")
	}
	if len(extra) > 0 {
		notes = append(notes, fmt.Sprintf("%s: %d key(s) of the frontmatter are not carried by `catalog:` (%s) — the document is a draft; the .bot keeps them until `fmt --to bot --force` replaces it from the document", path, len(extra), strings.Join(extra, ", ")))
	}
	return notes
}

// documentCarriesCatalog reports whether the document written has a
// top-level `catalog:` — read off the bytes that were written, so a note
// about them states what they hold rather than what the writer meant.
func documentCarriesCatalog(doc []byte) bool {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return false
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "catalog" {
			return true
		}
	}
	return false
}

// forceNote is the note a --force write owes the file it replaces, "" when
// nothing is lost: the file goes whole, so what it carried that the text
// replacing it does not is named. A .bot's comment lines are compared by
// their text, its frontmatter by key — the writer spells the frontmatter
// its own way, and a value it carries is not lost — and a document's YAML
// comments by their text. A target the reader cannot read whole — a .bot
// the parser recovers on, a document that is not one YAML document — is
// said to be replaced uncounted.
func forceNote(to, dest, from string, existing, out []byte) string {
	lead := dest + ": --force replaces it whole — "
	switch to {
	case "bot":
		pr := parser.Parse(dest, string(existing))
		if diagnosticErrors(pr.Diagnostics) != "" {
			return lead + "it does not parse as a .bot, so what it carries, its comments included, is not counted"
		}
		had, hadKeys := botCarries(pr.File)
		kept, keptKeys := botCarries(parser.Parse(dest, string(out)).File)
		n, first := lostLines(had, kept)
		var keys []string
		for _, k := range hadKeys {
			if !slices.Contains(keptKeys, k) {
				keys = append(keys, k)
			}
		}
		var lost []string
		if n > 0 {
			lost = append(lost, fmt.Sprintf("%d comment line(s)", n))
		}
		if len(keys) > 0 {
			lost = append(lost, "the frontmatter key(s) "+strings.Join(keys, ", "))
		}
		if len(lost) == 0 {
			return ""
		}
		note := lead + strings.Join(lost, " and ") + " it carries are not in what " + from + " writes"
		if n > 0 {
			note += " (the first comment: " + first + ")"
		}
		return note
	case "yaml":
		if !oneYAMLDocument(existing) {
			return lead + "it does not read as one YAML document, so what it carries, its comments included, is not counted"
		}
		n, first := lostLines(author.Comments(existing), author.Comments(out))
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("%s%d comment line(s) it carries are not in what %s writes (the first: %s)", lead, n, from, first)
	}
	return ""
}

// oneYAMLDocument reports whether text is what author.Comments reads whole:
// UTF-8 holding one YAML document, or nothing at all. Comments alone, a
// second document, a text that is not YAML are not.
func oneYAMLDocument(text []byte) bool {
	if !utf8.Valid(text) {
		return false
	}
	if strings.TrimSpace(strings.TrimPrefix(string(text), "\ufeff")) == "" {
		return true
	}
	dec := yaml.NewDecoder(bytes.NewReader(text))
	if dec.Decode(&yaml.Node{}) != nil {
		return false
	}
	return errors.Is(dec.Decode(&yaml.Node{}), io.EOF)
}

// lostLines counts the lines of had that kept does not hold, each line of
// kept matching one of had, and names the first.
func lostLines(had, kept []string) (int, string) {
	left := map[string]int{}
	for _, c := range kept {
		left[c]++
	}
	n, first := 0, ""
	for _, c := range had {
		if left[c] > 0 {
			left[c]--
			continue
		}
		if n == 0 {
			first = c
		}
		n++
	}
	return n, first
}

// botCarries is what a parsed .bot carries beside its program: the text of
// every comment — the file's own lines and those written around its
// declarations and edges, the strict-escape directive aside (the writer
// decides that one) — and the keys of a frontmatter block the catalogue
// reader can read, sorted. A block it cannot read (not closed, not YAML)
// counts as comment lines.
func botCarries(f *ast.File) (comments, keys []string) {
	add := func(cs []*ast.Comment) {
		for _, c := range cs {
			if !parser.IsStrictEscapeDirective(c.Text) {
				comments = append(comments, strings.TrimSpace(c.Text))
			}
		}
	}
	head := f.Comments
	if block := parser.Frontmatter(f); block.Found && block.Closed {
		if blockKeys, blockComments, ok := frontmatterByKey(strings.Join(block.Lines, "\n")); ok {
			keys = blockKeys
			comments = append(comments, blockComments...)
			head = f.Comments[block.Span:]
		}
	}
	add(head)
	for _, c := range ast.CommentCarriers(f) {
		if c.Comments != nil {
			add(*c.Comments)
		}
		for _, e := range c.Edges {
			add(e.Comments)
		}
	}
	return comments, keys
}

// frontmatterByKey reads a frontmatter block as the --force note compares
// it: one YAML document holding one mapping — its keys, sorted, a key named
// twice counted once as the catalogue reads it, and the block's own YAML
// comments, which the writer does not keep. Any other block is not read
// (ok false), and is compared line by line.
func frontmatterByKey(text string) (keys, comments []string, ok bool) {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var doc yaml.Node
	if dec.Decode(&doc) != nil || !errors.Is(dec.Decode(&yaml.Node{}), io.EOF) {
		return nil, nil, false
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, false
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i].Value; !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, author.Comments([]byte(text)), true
}

// commentsOutsideFrontmatter counts the comment lines of f the document
// does not represent: the head's — beyond the frontmatter block when the
// block became the document's `catalog:` (carries), the block's own lines
// included when it did not: not closed, not read, none of the four keys,
// they are lost with the rest — the strict-escape directive aside (the
// document says its profile with `dsl:`), and every comment a
// declaration or an edge carries.
func commentsOutsideFrontmatter(f *ast.File, carries bool) int {
	represented := 0
	if carries {
		represented = parser.Frontmatter(f).Span
	}
	n := 0
	for _, c := range f.Comments[represented:] {
		if !parser.IsStrictEscapeDirective(c.Text) {
			n++
		}
	}
	for _, c := range ast.CommentCarriers(f) {
		if c.Comments != nil {
			n += len(*c.Comments)
		}
		for _, e := range c.Edges {
			n += len(e.Comments)
		}
	}
	return n
}
