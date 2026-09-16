package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/fix"
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
			if err := os.WriteFile(path, out.Fixed, info.Mode().Perm()); err != nil {
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
