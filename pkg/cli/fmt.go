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
	// Baseline names a file listing the paths this tree already knows its
	// canonical form refuses (pkg/dsl/canon: one path per line, `#`
	// comments ignored). With it, Check is green while the refusals are
	// exactly the ones listed, and red — naming the difference — when a
	// file newly becomes unformattable or when the baseline names one
	// nothing refuses any more. Without it, any refusal is an error.
	Baseline string
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
	// RefusedPaths is the same list, paths alone — what a baseline is
	// compared against.
	RefusedPaths []string `json:"refused_paths,omitempty"`
	// NewlyRefused and NoLongerRefused are the two directions a baseline
	// can be stale in, by name. In the report itself, not only in the
	// printed lines: a JSON consumer got the error and no file.
	NewlyRefused    []string `json:"newly_refused,omitempty"`
	NoLongerRefused []string `json:"no_longer_refused,omitempty"`
}

var (
	// ErrFmtWouldChange is RunFmt's error under Check when a file is not
	// in its canonical form.
	ErrFmtWouldChange = errors.New("fmt: a file would change")
	// ErrFmtRefused is RunFmt's error when a file could not be rewritten
	// without changing it; the others were formatted all the same.
	ErrFmtRefused = errors.New("fmt: a file was refused")
	// ErrFmtNothingToCheck is RunFmt's error under Check when the paths
	// given hold no `.bot` at all. A gate that checked nothing is green
	// for the one reason a gate must never be: a path renamed out from
	// under it, a walk that stopped finding files.
	ErrFmtNothingToCheck = errors.New("fmt: --check found no .bot file")
	// ErrFmtBaselineStale is RunFmt's error when the refusals and the
	// baseline disagree — in either direction.
	ErrFmtBaselineStale = errors.New("fmt: the refusals do not match the baseline")
	// ErrFmtBaselineNeedsCheck refuses `--baseline` without `--check`: a
	// baseline says which files a CHECK tolerates, and a write pass that
	// ended on a ratchet verdict would have rewritten the tree first.
	ErrFmtBaselineNeedsCheck = errors.New("fmt: --baseline applies to --check")
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
	if opts.Baseline != "" && !opts.Check {
		return res, fmt.Errorf("%w", ErrFmtBaselineNeedsCheck)
	}
	if opts.Check && len(files) == 0 {
		return res, fmt.Errorf("%w under %s", ErrFmtNothingToCheck, strings.Join(opts.Paths, ", "))
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		out, err := canon.Bytes(path, raw)
		if err != nil {
			// canon states the fact; what to do about it is the caller's,
			// and for `fmt` it is "this file stays as its author wrote it".
			res.Refused = append(res.Refused, path+": "+strings.TrimPrefix(err.Error(), canon.ErrRefused.Error()+": ")+". Left as it is")
			res.RefusedPaths = append(res.RefusedPaths, canon.NormalizeBaselinePath(path))
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
	// The baseline is read BEFORE the report is written: both directions
	// of a stale verdict are fields of it, so `--json` carries the file to
	// edit and not only the error.
	stale := false
	if opts.Baseline != "" {
		known, err := canon.ReadBaseline(opts.Baseline)
		if err != nil {
			return res, fmt.Errorf("fmt: %w", err)
		}
		res.NewlyRefused, res.NoLongerRefused = canon.DiffBaseline(known, res.RefusedPaths)
		stale = len(res.NewlyRefused) > 0 || len(res.NoLongerRefused) > 0
	}
	reportFmt(opts, res)
	if opts.Baseline != "" {
		// Green while the refusals are the ones the tree already knows
		// about; red, naming the difference, otherwise.
		reportBaseline(opts, res)
		if stale {
			return res, ErrFmtBaselineStale
		}
	} else if len(res.Refused) > 0 {
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

// reportBaseline says which side of the baseline moved, by name: a check
// whose verdict does not name the file to edit is a check nobody acts on.
func reportBaseline(opts FmtOptions, res FmtResult) {
	p := opts.Printer
	if p == nil || p.Format == OutputJSON {
		// The JSON report carries both directions as fields; reportFmt
		// has already written it.
		return
	}
	for _, f := range res.NewlyRefused {
		p.Line("newly refused, and not in %s: %s — format it, or add it to the baseline with the reason", opts.Baseline, f)
	}
	for _, f := range res.NoLongerRefused {
		p.Line("%s names %s, which nothing refuses any more: remove the line (format the file in the same change)", opts.Baseline, f)
	}
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
