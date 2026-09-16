package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/canon"
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

// RunFmt rewrites every `.bot` under opts.Paths in its canonical form
// (pkg/dsl/canon): the text the studio saves, proven the same program
// before it is written, on the file's own bytes — BOM and line endings
// kept. A file canon refuses is left as it is and said by name, while the
// files beside it are formatted: a refusal is that file's, not the tree's.
// Under Check nothing is written. An archive is not a workflow file: named,
// it is an error; met in a walk, it is passed over like any other file.
func RunFmt(opts FmtOptions) (FmtResult, error) {
	var res FmtResult
	files, err := collectBotFiles("fmt", opts.Paths)
	if err != nil {
		return res, err
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		out, err := canon.Bytes(path, raw)
		if err != nil {
			res.Refused = append(res.Refused, path+": "+strings.TrimPrefix(err.Error(), canon.ErrRefused.Error()+": "))
			continue
		}
		f := FmtFile{Path: path, Changed: !bytes.Equal(out, raw)}
		if f.Changed && !opts.Check {
			info, err := os.Stat(path)
			if err != nil {
				return res, fmt.Errorf("fmt: %w", err)
			}
			if err := writeFileAtomic(path, out, info.Mode().Perm()); err != nil {
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
