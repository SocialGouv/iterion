package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// FmtOptions drive `iterion fmt`.
type FmtOptions struct {
	// Paths are `.bot` files, bundle directories or directories to walk.
	Paths []string
	// Check writes nothing and fails when a file would change or is
	// refused (a CI gate).
	Check bool
	// Printer receives the report; nil is silent.
	Printer *Printer
}

// FmtFile is one file's outcome.
type FmtFile struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Written bool   `json:"written"`
}

// FmtResult is the whole run's outcome.
type FmtResult struct {
	Files []FmtFile `json:"files"`
	// Refused lists the files fmt would not rewrite, with why; each is left
	// as it was.
	Refused []string `json:"refused,omitempty"`
}

var (
	// ErrFmtWouldChange is RunFmt's error under Check when a file is not
	// in its canonical form.
	ErrFmtWouldChange = errors.New("fmt: a file would change")
	// ErrFmtRefused is RunFmt's error when a file could not be rewritten
	// without changing it; the others were formatted all the same.
	ErrFmtRefused = errors.New("fmt: a file was refused")
)

// RunFmt rewrites every `.bot` under opts.Paths in its canonical form — the
// text the studio saves (pkg/dsl/unparse), proven before it is written to
// read as the same program (unparse.Verify: same parse, same profile, same
// compiled workflow and diagnostics, prompt bodies in their canonical
// form). A file it cannot rewrite without changing it is refused by name
// and left as it is — one that does not parse, one whose comments the
// writer would move or lose — and the others are formatted all the same: a
// refusal is that file's, not the tree's. Under Check nothing is written.
func RunFmt(opts FmtOptions) (FmtResult, error) {
	var res FmtResult
	// An archive is not formatted in place: refused by name, never read as
	// text. The walk never meets one (it is not a workflow FILE), so the
	// paths named by the caller are the place to say it.
	var paths []string
	for _, p := range opts.Paths {
		if kind, err := bundle.Detect(p); err == nil && kind == bundle.KindBundle {
			res.Refused = append(res.Refused, p+": an archive is not formatted in place — unpack it, format the sources, pack it again")
			continue
		}
		paths = append(paths, p)
	}
	files, err := collectBotFiles("fmt", paths)
	if err != nil {
		return res, err
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		text, why := canonicalText(path, string(raw))
		if why != "" {
			res.Refused = append(res.Refused, path+": "+why)
			continue
		}
		f := FmtFile{Path: path, Changed: text != string(raw)}
		if f.Changed && !opts.Check {
			info, err := os.Stat(path)
			if err != nil {
				return res, fmt.Errorf("fmt: %w", err)
			}
			if err := os.WriteFile(path, []byte(text), info.Mode().Perm()); err != nil {
				return res, fmt.Errorf("fmt: %w", err)
			}
			f.Written = true
		}
		res.Files = append(res.Files, f)
	}
	reportFmt(opts, res)
	if len(res.Refused) > 0 {
		return res, ErrFmtRefused
	}
	if opts.Check {
		for _, f := range res.Files {
			if f.Changed {
				return res, ErrFmtWouldChange
			}
		}
	}
	return res, nil
}

// canonicalText is the canonical form of one file's source, or the reason
// the file cannot be rewritten without changing it.
func canonicalText(path, src string) (text, refused string) {
	pr := parser.Parse(path, src)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return "", "does not parse: " + strings.Join(errs, "; ")
	}
	// The writer keeps the leading comments and no other: a comment after
	// the first declaration would move to the head, or be lost (a comment
	// inside a block never reaches the AST). Until comments survive a
	// rewrite (#1282) such a file is refused, not quietly rearranged.
	if line, first := commentAfterHead(path, src); line > 0 {
		return "", fmt.Sprintf("a comment at line %d follows the first declaration (line %d): the writer keeps only the leading comments and would move or lose it — move it above the declarations, or leave the file as it is", line, first)
	}
	text, err := provenText(pr.File)
	if err != nil {
		return "", "cannot be rewritten without changing the program: " + err.Error()
	}
	return text, ""
}

// provenText is the writer's text for f, proven to read as the same
// program (unparse.Verify) — or the reason it cannot be. From parsed text
// the proof holds by the round-trip the writer keeps; it is what stands
// between a document the writer cannot carry and a file that means
// something else.
func provenText(f *ast.File) (string, error) {
	text := unparse.Unparse(f)
	if err := unparse.Verify(f, text); err != nil {
		return "", err
	}
	return text, nil
}

// commentAfterHead finds the first comment placed after the file's head —
// the leading comments, the `dsl:` header and the import lines — and the
// line the first declaration opens on; 0, 0 when every comment leads, or
// nothing is declared.
func commentAfterHead(path, src string) (line, first int) {
	tokens := parser.NewLexer(path, src).All()
	i := 0
head:
	for i < len(tokens) {
		switch tokens[i].Type {
		case parser.TokenNewline, parser.TokenComment:
			i++
		case parser.TokenDSL, parser.TokenImport:
			for i < len(tokens) && tokens[i].Type != parser.TokenNewline && tokens[i].Type != parser.TokenEOF {
				i++
			}
		default:
			break head
		}
	}
	if i >= len(tokens) || tokens[i].Type == parser.TokenEOF {
		return 0, 0
	}
	first = tokens[i].Line
	for _, t := range tokens[i:] {
		if t.Type == parser.TokenComment {
			return t.Line, first
		}
	}
	return 0, 0
}

func reportFmt(opts FmtOptions, res FmtResult) {
	p := opts.Printer
	if p == nil {
		return
	}
	if p.Format == OutputJSON {
		p.JSON(res)
		return
	}
	for _, f := range res.Files {
		switch {
		case !f.Changed:
			p.Line("%s: already canonical", f.Path)
		case f.Written:
			p.Line("formatted %s", f.Path)
		default:
			p.Line("would format %s", f.Path)
		}
	}
	for _, r := range res.Refused {
		p.Line("refused: %s", r)
	}
}
