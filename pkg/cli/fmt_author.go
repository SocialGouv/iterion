package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// ErrFmtToBaseline refuses --baseline with --to: a baseline lists what a
	// CHECK of canonical forms tolerates, and a conversion is not one.
	ErrFmtToBaseline = errors.New("fmt: --baseline does not apply to --to")
)

// canonicalDocument is the canonical form of an author document: the
// program it describes, written back by the author writer, proven to read
// as the same program before it is handed back. Refused (canon.ErrRefused),
// the bytes left the author's: a document that does not read; one that
// carries YAML comments — the writer keeps none, and a rewrite would lose
// them; one whose written form reads as another program.
func canonicalDocument(path string, src []byte) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	res := author.Parse(abs, src)
	if res.HasErrors() {
		return nil, fmt.Errorf("%w: does not read: %s", canon.ErrRefused, diagnosticErrors(res.Diagnostics))
	}
	if comments := author.Comments(src); len(comments) > 0 {
		return nil, fmt.Errorf("%w: carries %d YAML comment line(s) the writer does not keep (the first: %s)", canon.ErrRefused, len(comments), comments[0])
	}
	out, err := author.Write(res.File)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot be written as a document: %v", canon.ErrRefused, err)
	}
	if bytes.Equal(out, src) {
		return src, nil
	}
	// The proof before the write: the written document reads as the same
	// program — the .bot text of the two is one.
	back := author.Parse(abs, out)
	if back.HasErrors() || unparse.Unparse(back.File) != unparse.Unparse(res.File) {
		return nil, fmt.Errorf("%w: its written form reads as another program", canon.ErrRefused)
	}
	return out, nil
}

// runFmtConvert is `fmt --to bot|yaml`: each named file converted to its
// twin beside it — x.bot.yaml → x.bot, x.bot → x.bot.yaml — proven the same
// program before it is written. A destination already there and not what
// the source writes is refused without --force, one that is already it is
// a no-op, and --check reports what a write would do: the CI form of "is
// the .bot beside the document the one it writes?".
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
		isDoc := workflowfile.IsAuthorDocument(p)
		if (opts.To == "bot") != isDoc || (opts.To == "yaml" && !workflowfile.IsWorkflowFile(p)) {
			return res, fmt.Errorf("%w: %s", ErrFmtToKind, p)
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
			res.Refused = append(res.Refused, path+": "+strings.TrimPrefix(err.Error(), canon.ErrRefused.Error()+": ")+". Nothing written")
			res.RefusedPaths = append(res.RefusedPaths, canon.NormalizeBaselinePath(path))
			continue
		}
		res.Notices = append(res.Notices, notices...)
		f := FmtFile{Path: dest, From: path}
		existing, statErr := os.ReadFile(dest)
		switch {
		case statErr == nil && bytes.Equal(existing, out):
			// The destination is already what the source writes.
		case statErr == nil && !opts.Force && !opts.Check:
			res.Refused = append(res.Refused, dest+": is there and is not what "+path+" writes; --force overwrites it. Left as it is")
			res.RefusedPaths = append(res.RefusedPaths, canon.NormalizeBaselinePath(dest))
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

// convertTwin writes the twin of one file: the .bot an author document
// stands for, proven to read back as the program the document describes
// (unparse.Verify — the check `validate` reports as E054); or the author
// document of a .bot, from a text that parses without an error — never
// from one the parser recovered on — with a notice for what the document
// does not carry: the frontmatter keys beyond the four of `catalog:`, a
// frontmatter the catalog reader cannot read, the ordinary comments.
func convertTwin(to, path string, raw []byte) (dest string, out []byte, notices []string, err error) {
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		abs = path
	}
	switch to {
	case "bot":
		dest = path[:len(path)-len(".yaml")] // `x.bot.yaml` stands for `x.bot`
		res := author.Parse(abs, raw)
		if res.HasErrors() {
			return dest, nil, nil, fmt.Errorf("%w: does not read: %s", canon.ErrRefused, diagnosticErrors(res.Diagnostics))
		}
		for _, d := range res.Diagnostics {
			notices = append(notices, path+": "+d.Error())
		}
		text := unparse.Unparse(res.File)
		if verr := unparse.Verify(res.File, text); verr != nil {
			return dest, nil, nil, fmt.Errorf("%w: has no written .bot form: %v", canon.ErrRefused, verr)
		}
		return dest, []byte(text), notices, nil
	case "yaml":
		dest = path + ".yaml"
		pr := parser.Parse(abs, string(raw))
		if errs := diagnosticErrors(pr.Diagnostics); errs != "" {
			return dest, nil, nil, fmt.Errorf("%w: does not parse: %s — a document is written from a program, never from a text the parser recovered on", canon.ErrRefused, errs)
		}
		out, werr := author.Write(pr.File)
		if werr != nil {
			return dest, nil, nil, fmt.Errorf("%w: cannot be written as a document: %v", canon.ErrRefused, werr)
		}
		notices = append(notices, frontmatterNotices(path, pr.File)...)
		if n := commentsOutsideFrontmatter(pr.File); n > 0 {
			notices = append(notices, fmt.Sprintf("%s: %d comment line(s) are not represented in the document — a draft carries the catalog only, the .bot keeps them", path, n))
		}
		return dest, out, notices, nil
	}
	return "", nil, nil, fmt.Errorf("fmt: --to takes bot or yaml, not %q", to)
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

// frontmatterBlock is the head comments' `---` block: its inner lines and
// whether it closed. fm is the number of head comments the block spans.
func frontmatterBlock(head []*ast.Comment) (lines []string, closed bool, fm int) {
	if len(head) == 0 || strings.TrimSpace(head[0].Text) != workflowfile.FrontmatterFence {
		return nil, false, 0
	}
	for i, c := range head[1:] {
		if strings.TrimSpace(c.Text) == workflowfile.FrontmatterFence {
			return lines, true, i + 2
		}
		lines = append(lines, c.Text)
	}
	return lines, false, len(head)
}

// frontmatterNotices says what the document's `catalog:` does not carry of
// the .bot's frontmatter: a block that is not closed or not YAML the
// catalog reader reads (the document then carries no `catalog:`), and the
// keys beyond the four of the catalog identity.
func frontmatterNotices(path string, f *ast.File) []string {
	lines, closed, fm := frontmatterBlock(f.Comments)
	if fm == 0 {
		return nil
	}
	if !closed {
		return []string{path + ": the frontmatter block is not closed (no second `## ---`) — the document carries no `catalog:`"}
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &m); err != nil {
		return []string{path + ": the frontmatter is not YAML the catalog reader reads (" + err.Error() + ") — the document carries no `catalog:`"}
	}
	var extra []string
	for k := range m {
		switch k {
		case "name", "description", "triggers", "capabilities":
		default:
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	return []string{fmt.Sprintf("%s: %d key(s) of the frontmatter are not carried by `catalog:` (%s) — the document is a draft, the .bot keeps them", path, len(extra), strings.Join(extra, ", "))}
}

// commentsOutsideFrontmatter counts the comment lines of f the document
// does not represent: the head's beyond the frontmatter block (the
// strict-escape directive aside — the document says its profile with
// `dsl:`), and every comment a declaration or an edge carries.
func commentsOutsideFrontmatter(f *ast.File) int {
	_, _, fm := frontmatterBlock(f.Comments)
	n := 0
	for _, c := range f.Comments[fm:] {
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
