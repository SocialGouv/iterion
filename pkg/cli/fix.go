package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/fix"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// FixOptions drive `iterion fix`.
type FixOptions struct {
	// Paths are `.bot` files, bundle directories or directories to walk.
	Paths []string
	// DryRun lists every edit and writes nothing.
	DryRun bool
	// Printer receives the report; nil is silent.
	Printer *Printer
}

// FixedFile is one file's outcome.
type FixedFile struct {
	Path    string     `json:"path"`
	Changed bool       `json:"changed"`
	Written bool       `json:"written"`
	Edits   []fix.Edit `json:"edits,omitempty"`
	Left    []fix.Left `json:"left,omitempty"`
}

// FixResult is the whole run's outcome.
type FixResult struct {
	Files []FixedFile `json:"files"`
	// Refused lists the files the fixer would not rewrite, with why.
	Refused []string `json:"refused,omitempty"`
}

// ErrFixRefused is RunFix's error when a file was refused; the others were
// fixed all the same.
var ErrFixRefused = errors.New("fix: a file was refused")

// RunFix applies the mechanical remedies of compile diagnostics to every
// `.bot` under opts.Paths (pkg/dsl/fix): each file is rewritten on its own
// bytes and proven — it parses, and compiles to the same diagnostics minus
// the fixed — before it is written. What has no mechanical remedy is listed
// by file, for the author. Under DryRun nothing is written.
func RunFix(opts FixOptions) (FixResult, error) {
	var res FixResult
	files, err := collectBotFiles("fix", opts.Paths)
	if err != nil {
		return res, err
	}
	files = unitFiles(files)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("fix: %w", err)
		}
		out, err := fix.Bytes(path, raw)
		if err != nil {
			res.Refused = append(res.Refused, err.Error())
			continue
		}
		f := FixedFile{Path: path, Changed: out.Changed, Edits: out.Applied, Left: out.Left}
		if out.Changed && !opts.DryRun {
			info, err := os.Stat(path)
			if err != nil {
				return res, fmt.Errorf("fix: %w", err)
			}
			if err := writeFileAtomic(path, out.Fixed, info.Mode().Perm()); err != nil {
				return res, fmt.Errorf("fix: %w", err)
			}
			f.Written = true
		}
		res.Files = append(res.Files, f)
	}
	reportFix(opts, res)
	if len(res.Refused) > 0 {
		return res, ErrFixRefused
	}
	return res, nil
}

// unitFiles adds to files the fragments of every main among them: fixing a
// bot fixes its unit, each file on its own bytes. A fragment already listed
// (a walked bundle) is not listed twice.
func unitFiles(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		if abs, err := filepath.Abs(f); err == nil {
			seen[abs] = true
		}
	}
	out := append([]string(nil), files...)
	for _, f := range files {
		u := unit.LoadDir(f)
		if u == nil || len(u.Files) < 2 || u.Files[0].Rel != filepath.Base(f) {
			continue // not a main with fragments
		}
		for _, fr := range u.Files[1:] {
			if seen[fr.Name] {
				continue
			}
			seen[fr.Name] = true
			p := fr.Name
			if cwd, err := os.Getwd(); err == nil {
				if r, err := filepath.Rel(cwd, fr.Name); err == nil && !strings.HasPrefix(r, "..") {
					p = r
				}
			}
			out = append(out, p)
		}
	}
	return out
}

func reportFix(opts FixOptions, res FixResult) {
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
			p.Line("%s: nothing mechanical to fix", f.Path)
		case f.Written:
			p.Line("fixed %s (%d edit(s))", f.Path, len(f.Edits))
		default:
			p.Line("would fix %s (%d edit(s))", f.Path, len(f.Edits))
		}
		if opts.DryRun || !f.Changed {
			for _, e := range f.Edits {
				p.Line("  %d:%d [%s] %s → %s", e.Line, e.Column, e.Code, e.From, e.To)
			}
		}
		for _, l := range f.Left {
			p.Line("  left [%s] %s — %s", l.Code, l.Message, l.Why)
		}
	}
	for _, r := range res.Refused {
		p.Line("refused: %s", r)
	}
}
