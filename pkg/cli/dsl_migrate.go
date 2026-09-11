package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/migrate"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// MigrateDSLOptions drives `iterion dsl migrate`.
type MigrateDSLOptions struct {
	// Paths are `.bot` files, bundle directories or directories to walk.
	Paths []string
	// To is the target syntax profile (0: the newest this build reads).
	To int
	// DryRun reports every change and writes nothing.
	DryRun bool
	// Check writes nothing and fails when a file would change (a CI gate).
	Check bool
	// StrictPrompts refuses a file in which a named prompt keeps paragraph
	// breaks it used to lose.
	StrictPrompts bool
	// ShowPrompts lists those prompts in the report.
	ShowPrompts bool
	// Floor is the engine version a migrated bundle's manifest must require
	// at least; "" means this build's own version — the one that reads the
	// profile. A version that cannot be ordered (a dev build) raises nothing
	// and says what to declare instead.
	Floor string
	// Printer receives the report; nil is silent.
	Printer *Printer
}

// MigratedFile is one file's outcome.
type MigratedFile struct {
	Path    string                 `json:"path"`
	Changed bool                   `json:"changed"`
	Written bool                   `json:"written"`
	Changes []migrate.Change       `json:"changes,omitempty"`
	Prompts []migrate.PromptChange `json:"prompts,omitempty"`
}

// ManifestFloor is a bundle manifest whose engine floor the migration
// raised, or would raise, or could not.
type ManifestFloor struct {
	Path    string `json:"path"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Written bool   `json:"written"`
	// Skipped says why the floor was left alone, when it was.
	Skipped string `json:"skipped,omitempty"`
}

// MigrateDSLResult is the whole run's outcome.
type MigrateDSLResult struct {
	Files     []MigratedFile  `json:"files"`
	Manifests []ManifestFloor `json:"manifests,omitempty"`
	// Refused lists the files the migration would not rewrite, with why.
	Refused []string `json:"refused,omitempty"`
}

// ErrWouldChange is MigrateDSL's error under Check when a file is not yet
// at the target profile.
var ErrWouldChange = errors.New("dsl migrate: a file would change")

// MigrateDSL migrates every `.bot` under opts.Paths to the target profile
// (pkg/dsl/migrate) and raises the engine floor of every bundle manifest
// beside a migrated file. A refused file is reported and fails the run once
// every other file has been handled; nothing is written under DryRun or
// Check.
func MigrateDSL(opts MigrateDSLOptions) (MigrateDSLResult, error) {
	var res MigrateDSLResult
	files, err := collectBotFiles(opts.Paths)
	if err != nil {
		return res, err
	}
	write := !opts.DryRun && !opts.Check
	bundleDirs := map[string]bool{}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("dsl migrate: read %s: %w", path, err)
		}
		out, err := migrate.Bytes(path, src, migrate.Options{To: opts.To, StrictPrompts: opts.StrictPrompts})
		if err != nil {
			res.Refused = append(res.Refused, err.Error())
			continue
		}
		mf := MigratedFile{Path: path, Changed: out.Changed, Changes: out.Changes, Prompts: out.Prompts}
		if out.Changed && write {
			info, err := os.Stat(path)
			if err != nil {
				return res, err
			}
			if err := os.WriteFile(path, out.Migrated, info.Mode().Perm()); err != nil {
				return res, fmt.Errorf("dsl migrate: write %s: %w", path, err)
			}
			mf.Written = true
		}
		res.Files = append(res.Files, mf)
		if dir := filepath.Dir(path); manifestPath(dir) != "" {
			bundleDirs[dir] = true
		}
	}

	dirs := make([]string, 0, len(bundleDirs))
	for d := range bundleDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		floor, err := raiseManifestFloor(manifestPath(dir), opts.Floor, write)
		if err != nil {
			return res, err
		}
		res.Manifests = append(res.Manifests, floor)
	}

	reportMigration(opts, res)
	if len(res.Refused) > 0 {
		return res, fmt.Errorf("dsl migrate: %d file(s) refused:\n  %s", len(res.Refused), strings.Join(res.Refused, "\n  "))
	}
	if opts.Check {
		for _, f := range res.Files {
			if f.Changed {
				return res, fmt.Errorf("%w: %s", ErrWouldChange, f.Path)
			}
		}
		for _, m := range res.Manifests {
			if m.To != "" && !m.Written && m.Skipped == "" {
				return res, fmt.Errorf("%w: %s (requires.iterion %s → %s)", ErrWouldChange, m.Path, m.From, m.To)
			}
		}
	}
	return res, nil
}

// collectBotFiles expands the paths: a workflow file as itself, a directory
// as every workflow file under it (skipping the store, the VCS and vendored
// trees), sorted.
func collectBotFiles(paths []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("dsl migrate: %w", err)
		}
		if !info.IsDir() {
			if !workflowfile.IsWorkflowFile(p) {
				return nil, fmt.Errorf("dsl migrate: %s is not a workflow file", p)
			}
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", ".iterion", "vendor", "node_modules":
					if path != p {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if workflowfile.IsWorkflowFile(path) {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("dsl migrate: walk %s: %w", p, err)
		}
	}
	sort.Strings(out)
	return out, nil
}

// manifestPath is the bundle manifest beside a directory's files, or "".
func manifestPath(dir string) string {
	for _, name := range []string{bundle.ManifestFile, bundle.ManifestFileAlt} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// raiseManifestFloor makes a manifest require at least floor (this build's
// version when floor is ""), through the shared writer that keeps the
// manifest's comments and validates the result with the strict loader.
func raiseManifestFloor(path, floor string, write bool) (ManifestFloor, error) {
	out := ManifestFloor{Path: path}
	if floor == "" {
		floor = appinfo.Version
	}
	floor = strings.TrimPrefix(strings.TrimSpace(floor), "v")
	m, err := bundle.LoadManifest(path)
	if err != nil {
		return out, fmt.Errorf("dsl migrate: %w", err)
	}
	if m != nil && m.Requires != nil {
		out.From = m.Requires.Iterion
	}
	if _, ok := bundle.CompareVersions(floor, "0"); !ok {
		out.Skipped = fmt.Sprintf("this build's version %q cannot be ordered: declare `requires: { iterion: \">= <the release that reads profile 2>\" }` by hand", floor)
		return out, nil
	}
	if out.From != "" {
		have := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.From), ">="))
		if cmp, ok := bundle.CompareVersions(have, floor); ok && cmp >= 0 {
			out.Skipped = "already at or above the floor"
			return out, nil
		}
	}
	out.To = ">= " + floor
	if !write {
		return out, nil
	}
	if _, err := bundle.WriteManifest(path, bundle.ManifestPatch{Requires: &bundle.Requires{Iterion: out.To}}); err != nil {
		return out, fmt.Errorf("dsl migrate: %w", err)
	}
	out.Written = true
	return out, nil
}

func reportMigration(opts MigrateDSLOptions, res MigrateDSLResult) {
	p := opts.Printer
	if p == nil {
		return
	}
	if p.Format == OutputJSON {
		p.JSON(res)
		return
	}
	verb := "migrated"
	if opts.DryRun || opts.Check {
		verb = "would migrate"
	}
	for _, f := range res.Files {
		if !f.Changed {
			p.Line("%s: already at the target profile", f.Path)
			continue
		}
		p.Line("%s %s (%d change(s), %d prompt(s) keep their paragraphs)", verb, f.Path, len(f.Changes), len(f.Prompts))
		if opts.DryRun {
			for _, c := range f.Changes {
				switch c.Kind {
				case "header":
					p.Line("  %d: + %s", c.Line, c.To)
				case "directive":
					p.Line("  %d: - %s", c.Line, c.From)
				default:
					p.Line("  %d: %s → %s", c.Line, c.From, c.To)
				}
			}
		}
		if opts.ShowPrompts {
			for _, pc := range f.Prompts {
				p.Line("  prompt %s (line %d): %d blank line(s) now reach the model as paragraph breaks", pc.Name, pc.Line, pc.BlankLines)
			}
		}
	}
	for _, m := range res.Manifests {
		switch {
		case m.Skipped != "":
			p.Line("%s: requires.iterion left alone — %s", m.Path, m.Skipped)
		case m.Written:
			p.Line("%s: requires.iterion %q → %q", m.Path, m.From, m.To)
		default:
			p.Line("%s: would set requires.iterion %q → %q", m.Path, m.From, m.To)
		}
	}
	for _, r := range res.Refused {
		p.Line("refused: %s", r)
	}
}
