package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/canon"
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
// .bot carries the reading); or the author document of a .bot, from a text
// that parses without an error — never from one the parser recovered on —
// with a notice for what the document does not carry: a frontmatter the
// catalog reader does not read, the keys beyond the four of `catalog:`, the
// ordinary comments.
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
		return dest, []byte(text), notices, nil
	case "yaml":
		pr := parser.Parse(abs, string(raw))
		if errs := diagnosticErrors(namedAs(pr.Diagnostics, path)); errs != "" {
			return dest, nil, nil, fmt.Errorf("%w: does not parse: %s — a document is written from a program, never from a text the parser recovered on", canon.ErrRefused, errs)
		}
		out, werr := author.Write(pr.File)
		if werr != nil {
			return dest, nil, nil, fmt.Errorf("%w: cannot be written as a document: %v", canon.ErrRefused, werr)
		}
		// Whether the document carries a `catalog:` is read off the bytes
		// written: the notes state what they hold, not what the writer meant.
		carries := documentCarriesCatalog(out)
		notices = append(notices, frontmatterNotices(path, pr.File, carries)...)
		if n := commentsOutsideFrontmatter(pr.File, carries); n > 0 {
			notices = append(notices, fmt.Sprintf("%s: %d comment line(s) are not represented in the document — a draft carries the catalog only, the .bot keeps them", path, n))
		}
		return dest, out, notices, nil
	}
	return "", nil, nil, fmt.Errorf("fmt: --to takes bot or yaml, not %q", to)
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
		notes = append(notes, fmt.Sprintf("%s: %d key(s) of the frontmatter are not carried by `catalog:` (%s) — the document is a draft, the .bot keeps them", path, len(extra), strings.Join(extra, ", ")))
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
