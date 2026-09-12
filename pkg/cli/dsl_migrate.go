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
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
	// at least. Given, it must be orderable, or the run is refused before
	// anything is planned; "" means this build's own version — the one that
	// reads the profile — and, on a build with no version to write (dev),
	// the release that first reads the target profile (parser.ProfileSince).
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
// (pkg/dsl/migrate) and raises the engine floor of every bundle that OWNS a
// migrated file — the bundle a child under `kids/` belongs to, not the
// directory beside it. Every file is planned and proven before any is
// written: one refusal anywhere, and nothing is written anywhere, so a run
// never leaves a tree half-migrated. Nothing is written under DryRun or
// Check either.
func MigrateDSL(opts MigrateDSLOptions) (MigrateDSLResult, error) {
	var res MigrateDSLResult
	floor, err := resolveFloor(opts.Floor, opts.To)
	if err != nil {
		return res, err
	}
	files, err := collectBotFiles(opts.Paths)
	if err != nil {
		return res, err
	}
	write := !opts.DryRun && !opts.Check

	// Plan first.
	type planned struct {
		path string
		out  *migrate.Result
	}
	var plans []planned
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
		res.Files = append(res.Files, MigratedFile{Path: path, Changed: out.Changed, Changes: out.Changes, Prompts: out.Prompts})
		plans = append(plans, planned{path, out})
		if out.Changed {
			if dir := owningBundleDir(path); dir != "" {
				bundleDirs[dir] = true
			}
		}
	}
	if len(res.Refused) > 0 {
		reportMigration(opts, res)
		return res, fmt.Errorf("dsl migrate: %d file(s) refused — nothing was written:\n  %s", len(res.Refused), strings.Join(res.Refused, "\n  "))
	}

	// Then write.
	if write {
		for i, p := range plans {
			if !p.out.Changed {
				continue
			}
			info, err := os.Stat(p.path)
			if err != nil {
				return res, err
			}
			if err := os.WriteFile(p.path, p.out.Migrated, info.Mode().Perm()); err != nil {
				return res, fmt.Errorf("dsl migrate: write %s: %w", p.path, err)
			}
			res.Files[i].Written = true
		}
	}

	dirs := make([]string, 0, len(bundleDirs))
	for d := range bundleDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		mp := manifestPath(dir)
		if mp == "" {
			// A bundle known by its skills/ alone has nowhere to carry the
			// floor: said here, and asked again by `iterion validate` (C252).
			res.Manifests = append(res.Manifests, ManifestFloor{
				Path:    filepath.Join(dir, bundle.ManifestFile),
				Skipped: "no manifest to carry requires.iterion — create one (`iterion bots create` writes one); until then `iterion validate` asks for the floor (C252)",
			})
			continue
		}
		mf, err := raiseManifestFloor(mp, floor, write)
		if err != nil {
			return res, err
		}
		res.Manifests = append(res.Manifests, mf)
	}

	reportMigration(opts, res)
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
				// Hidden trees hold other checkouts (`.claude/worktrees`,
				// `.works`, `.repos`), the store and the VCS: never rewritten
				// from a walk, only when named as a path themselves.
				if path != p && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "node_modules") {
					return filepath.SkipDir
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

// resolveFloor is the engine version a migrated bundle's manifest must
// require: the --floor given, refused when it cannot be ordered (an operator
// who typed a floor asked for one); else this build's version, the one that
// reads the profile; else — a build with no version to write — the release
// that first reads the target profile (parser.ProfileSince), "" when none
// is on record.
func resolveFloor(flag string, to int) (string, error) {
	if f := strings.TrimPrefix(strings.TrimSpace(flag), "v"); f != "" {
		if _, ok := bundle.CompareVersions(f, "0"); !ok {
			return "", fmt.Errorf("dsl migrate: --floor %q cannot be ordered: give a dotted numeric version (3.141.0), or omit it for this build's", flag)
		}
		return f, nil
	}
	if v := strings.TrimPrefix(strings.SplitN(appinfo.Version, "+", 2)[0], "v"); v != "" {
		if _, ok := bundle.CompareVersions(v, "0"); ok {
			return v, nil
		}
	}
	if to == 0 {
		to = parser.MaxProfile
	}
	return parser.ProfileSince[to], nil
}

// owningBundleDir is the root of the bundle a workflow file belongs to —
// the nearest directory up from it that pkg/bundle recognises (a manifest,
// or a skills/ directory) — or "" for a loose file.
func owningBundleDir(path string) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return ""
	}
	for {
		if bundle.DirForMainBot(filepath.Join(dir, bundle.MainBotFile)) != "" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// raiseManifestFloor makes a manifest require at least floor (resolveFloor),
// through the shared writer that keeps the manifest's comments and
// validates the result with the strict loader. A floor already at or above
// it is kept — lowering a declared floor is never this command's to do.
func raiseManifestFloor(path, floor string, write bool) (ManifestFloor, error) {
	out := ManifestFloor{Path: path}
	m, err := bundle.LoadManifest(path)
	if err != nil {
		return out, fmt.Errorf("dsl migrate: %w", err)
	}
	if m != nil && m.Requires != nil {
		out.From = m.Requires.Iterion
	}
	if floor == "" {
		out.Skipped = "this build has no version to write and the profile no release on record: declare `requires: { iterion: \">= <the release that reads the profile>\" }` by hand"
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
