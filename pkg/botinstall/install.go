// Package botinstall imports a bot bundle from a git URL or local path into a
// workspace so `iterion bots list`, the dispatcher, and the studio discover
// it. It is the shared core behind the `iterion bots install` CLI and the
// studio's POST /api/v1/bots/install endpoint — kept in a neutral package so
// pkg/server can reuse it without importing pkg/cli (which would cycle).
//
// Installed bots are NEVER run automatically: the operator inspects, then
// launches (run-time sandboxing applies as usual).
package botinstall

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// Options configures an install.
type Options struct {
	Source  string // git URL (optionally url#ref) or a local directory
	Ref     string // git ref (branch/tag); overrides a #ref in Source
	Path    string // subdirectory within the repo, or an iterion-bots.yaml bot name
	Dest    string // install destination root (default <workdir>/.botz)
	Name    string // install under this name instead of the source's
	Force   bool   // overwrite an existing install
	Workdir string // workspace root for catalog regen (default cwd)
}

// Result is the structured outcome of an install.
type Result struct {
	Name          string `json:"name"`
	Source        string `json:"source"`
	Ref           string `json:"ref,omitempty"`
	InstalledPath string `json:"installed_path"`
	Skills        int    `json:"skills"`
	Presets       int    `json:"presets"`
}

// repoBotIndex is the OPTIONAL repo-root convention file (iterion-bots.yaml)
// listing the bots a repository publishes. When present it is authoritative;
// when absent, install scans the repo for bundle directories. This is the
// "convention de repos" — a repo opts into being a bot source either by
// shipping this index or simply by holding bundle directories.
type repoBotIndex struct {
	Bots []repoBotEntry `yaml:"bots"`
}

type repoBotEntry struct {
	Name        string `yaml:"name"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

const repoBotIndexName = "iterion-bots.yaml"

// Install imports a bot bundle per Options and returns where it landed.
func Install(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return nil, fmt.Errorf("a git URL or local path is required")
	}
	url, ref := splitSourceRef(opts.Source)
	if opts.Ref != "" {
		ref = opts.Ref
	}
	workdir := opts.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	dest := opts.Dest
	if dest == "" {
		dest = filepath.Join(workdir, ".botz")
	}

	// 1. Resolve the source to a local repo root (clone a URL into a temp dir;
	//    use a local directory in place).
	repoRoot, cleanup, err := resolveRepoRoot(ctx, url, ref)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// 2. Locate the bundle directory to install (explicit --path, the repo
	//    root itself, the sole discovered bundle, or an index entry).
	botDir, err := selectBundleDir(repoRoot, opts.Path)
	if err != nil {
		return nil, err
	}

	// 3. Validate it is a well-formed bundle (manifest schema, main.bot, path
	//    guards) BEFORE copying anything into the workspace.
	b, err := bundle.OpenDir(botDir)
	if err != nil {
		return nil, fmt.Errorf("not a valid bot bundle at %s: %w", botDir, err)
	}

	// 4. Determine install name + destination.
	name := opts.Name
	if name == "" {
		name = filepath.Base(botDir)
		if b.Manifest != nil && b.Manifest.Name != "" {
			name = b.Manifest.Name
		}
	}
	if err := validateInstallName(name); err != nil {
		return nil, err
	}
	target := filepath.Join(dest, name)
	if _, err := os.Stat(target); err == nil {
		if !opts.Force {
			return nil, fmt.Errorf("%s already exists — pass --force to overwrite", target)
		}
		if err := os.RemoveAll(target); err != nil {
			return nil, fmt.Errorf("remove existing %s: %w", target, err)
		}
	}

	// 5. Copy the bundle directory into the workspace (git metadata excluded).
	if err := copyTree(botDir, target); err != nil {
		return nil, fmt.Errorf("install %s: %w", name, err)
	}

	// 6. Refresh Nexie's catalog so the new bot is advertised (best-effort).
	_, _ = botregistry.RegenerateWhatsNextCatalog(workdir)

	res := &Result{Name: name, Source: opts.Source, Ref: ref, InstalledPath: target}
	if installed, derr := bundle.OpenDir(target); derr == nil {
		if installed.SkillsDir != "" {
			if ents, _ := os.ReadDir(installed.SkillsDir); ents != nil {
				res.Skills = countFiles(ents)
			}
		}
		if installed.PresetsDir != "" {
			if specs, _ := bundle.LoadPresets(installed.PresetsDir); specs != nil {
				res.Presets = len(specs)
			}
		}
	}
	return res, nil
}

// InstallFromBotzBytes extracts a `.botz` archive streamed from r into a
// temp dir, then installs it into the workspace exactly like Install:
// validate the bundle, copy it into <workdir>/.botz/<name>, refresh the
// catalog. The archive's manifest.name wins unless opts.Name overrides
// it; opts.Source/Ref/Path are ignored (the bytes are the source). This
// is the entry point behind the studio "Import .botz file" upload.
func InstallFromBotzBytes(ctx context.Context, r io.Reader, opts Options) (*Result, error) {
	tmp, err := os.MkdirTemp("", "iterion-botz-upload-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	extractDir := filepath.Join(tmp, "bundle")
	if _, err := bundle.ExtractArchive(r, extractDir); err != nil {
		return nil, fmt.Errorf("extract .botz: %w", err)
	}
	// Reuse Install's local-directory path: it validates via
	// bundle.OpenDir, resolves the install name, copies into the
	// workspace and regenerates the catalog.
	o := opts
	o.Source = extractDir
	o.Ref = ""
	o.Path = ""
	res, err := Install(ctx, o)
	if err != nil {
		return nil, err
	}
	// The temp extract dir is the "source"; report it as an upload so the
	// caller doesn't surface a throwaway /tmp path.
	res.Source = "upload"
	return res, nil
}

// Remove deletes an installed bot bundle (<workdir>/.botz/<name>) and
// refreshes Nexie's catalog. A missing install is an error so the caller
// can surface a 404. Mirrors Install's destination resolution.
func Remove(_ context.Context, opts Options) error {
	name := strings.TrimSpace(opts.Name)
	if err := validateInstallName(name); err != nil {
		return err
	}
	workdir := opts.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		workdir = wd
	}
	dest := opts.Dest
	if dest == "" {
		dest = filepath.Join(workdir, ".botz")
	}
	target := filepath.Join(dest, name)
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s is not installed", name)
		}
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a bundle directory", target)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	_, _ = botregistry.RegenerateWhatsNextCatalog(workdir)
	return nil
}

// Fetch resolves opts.Source (a git URL or local path) to a validated bundle
// directory and returns its path plus a cleanup func. Unlike Install it is
// READ-ONLY: it never copies into a workspace, never regenerates a catalog,
// and never mutates the caller's tree. It is the materialization step behind
// the marketplace `.botz` download endpoint — the caller packs the returned
// directory with bundle.PackDir, then invokes cleanup() to drop the temp
// clone. For a local-path Source (e.g. a builtin entry's RepoURL) cleanup is
// a no-op and the returned path is the source dir itself, so callers MUST NOT
// mutate it.
func Fetch(ctx context.Context, opts Options) (dir string, cleanup func(), err error) {
	if strings.TrimSpace(opts.Source) == "" {
		return "", func() {}, fmt.Errorf("a git URL or local path is required")
	}
	url, ref := splitSourceRef(opts.Source)
	if opts.Ref != "" {
		ref = opts.Ref
	}
	repoRoot, rcleanup, err := resolveRepoRoot(ctx, url, ref)
	if err != nil {
		return "", func() {}, err
	}
	botDir, err := selectBundleDir(repoRoot, opts.Path)
	if err != nil {
		rcleanup()
		return "", func() {}, err
	}
	if _, err := bundle.OpenDir(botDir); err != nil {
		rcleanup()
		return "", func() {}, fmt.Errorf("not a valid bot bundle at %s: %w", botDir, err)
	}
	return botDir, rcleanup, nil
}

// FetchRaw resolves opts.Source like Fetch but skips the bot-bundle
// discovery/validation: it returns the raw source tree, narrowed to
// opts.Path when set. It is the materialization step behind the
// marketplace download of non-bot entries (plugins, whose layout
// bundle.OpenDir would reject). Same read-only contract as Fetch: never
// mutate the returned directory.
func FetchRaw(ctx context.Context, opts Options) (dir string, cleanup func(), err error) {
	if strings.TrimSpace(opts.Source) == "" {
		return "", func() {}, fmt.Errorf("a git URL or local path is required")
	}
	url, ref := splitSourceRef(opts.Source)
	if opts.Ref != "" {
		ref = opts.Ref
	}
	repoRoot, rcleanup, err := resolveRepoRoot(ctx, url, ref)
	if err != nil {
		return "", func() {}, err
	}
	dir = repoRoot
	if p := strings.TrimSpace(opts.Path); p != "" {
		dir, err = resolveSubdir(repoRoot, p)
		if err != nil {
			rcleanup()
			return "", func() {}, err
		}
	}
	return dir, rcleanup, nil
}

// splitSourceRef splits "url#ref" into (url, ref). A '#' whose prefix is an
// existing local directory is treated as part of the path, not a ref marker.
func splitSourceRef(src string) (url, ref string) {
	if i := strings.LastIndex(src, "#"); i > 0 {
		if _, err := os.Stat(src[:i]); err != nil {
			return src[:i], src[i+1:]
		}
	}
	return src, ""
}

// resolveRepoRoot clones a git URL into a temp dir (returning a cleanup that
// removes it) or returns a local directory in place (cleanup is a no-op).
func resolveRepoRoot(ctx context.Context, source, ref string) (root string, cleanup func(), err error) {
	cleanup = func() {}
	if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		abs, e := filepath.Abs(source)
		if e != nil {
			return "", cleanup, e
		}
		return abs, cleanup, nil
	}
	tmp, e := os.MkdirTemp("", "iterion-bot-install-*")
	if e != nil {
		return "", cleanup, e
	}
	cloneDest := filepath.Join(tmp, "repo")
	if cerr := gitlib.ShallowClone(ctx, source, ref, cloneDest); cerr != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, cerr
	}
	return cloneDest, func() { _ = os.RemoveAll(tmp) }, nil
}

// selectBundleDir resolves which bundle directory inside repoRoot to install.
func selectBundleDir(repoRoot, subPath string) (string, error) {
	idx, hasIndex := readRepoBotIndex(repoRoot)
	if subPath != "" {
		// Match an index bot name first, then fall back to a relative path.
		if hasIndex {
			for _, e := range idx.Bots {
				if e.Name == subPath {
					return resolveSubdir(repoRoot, e.Path)
				}
			}
		}
		return resolveSubdir(repoRoot, subPath)
	}
	if isBundleDir(repoRoot) {
		return repoRoot, nil
	}
	if hasIndex {
		switch len(idx.Bots) {
		case 0:
			return "", fmt.Errorf("%s lists no bots", repoBotIndexName)
		case 1:
			return resolveSubdir(repoRoot, idx.Bots[0].Path)
		default:
			return "", fmt.Errorf("repo publishes %d bots; choose one with --path <name>:\n%s", len(idx.Bots), formatIndexBots(idx))
		}
	}
	bundles := scanBundleDirs(repoRoot)
	switch len(bundles) {
	case 0:
		return "", fmt.Errorf("no bot bundle found in the repository (expected a main.bot + manifest.yaml, or an %s index)", repoBotIndexName)
	case 1:
		return bundles[0], nil
	default:
		rels := make([]string, len(bundles))
		for i, bd := range bundles {
			rels[i], _ = filepath.Rel(repoRoot, bd)
		}
		return "", fmt.Errorf("repository contains %d bots; choose one with --path:\n  %s", len(bundles), strings.Join(rels, "\n  "))
	}
}

// resolveSubdir joins a relative bundle path to repoRoot, rejecting any path
// that escapes the repository, and verifies it is a bundle.
func resolveSubdir(repoRoot, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("--path %q must be a relative path inside the repository", rel)
	}
	dir := filepath.Join(repoRoot, clean)
	if !isBundleDir(dir) {
		return "", fmt.Errorf("%q is not a bot bundle (needs main.bot + manifest.yaml)", rel)
	}
	return dir, nil
}

func isBundleDir(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, "main.bot")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dir, bundle.ManifestFile)); err == nil && !st.IsDir() {
		return true
	}
	st, err := os.Stat(filepath.Join(dir, bundle.ManifestFileAlt))
	return err == nil && !st.IsDir()
}

func scanBundleDirs(repoRoot string) []string {
	entries, err := botregistry.List(botregistry.ListOptions{Paths: []string{repoRoot}})
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsBundleDir {
			out = append(out, e.Path)
		}
	}
	return out
}

func readRepoBotIndex(repoRoot string) (repoBotIndex, bool) {
	body, err := os.ReadFile(filepath.Join(repoRoot, repoBotIndexName))
	if err != nil {
		return repoBotIndex{}, false
	}
	var idx repoBotIndex
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return repoBotIndex{}, false
	}
	return idx, true
}

func formatIndexBots(idx repoBotIndex) string {
	var b strings.Builder
	for _, e := range idx.Bots {
		fmt.Fprintf(&b, "  %s (%s)\n", e.Name, e.Path)
	}
	return b.String()
}

func validateInstallName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("install name is empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid install name %q (no path separators)", name)
	}
	return nil
}

// copyTree recursively copies src into dst, skipping VCS and local-state
// directories and any non-regular files (symlinks, devices).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".iterion") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFileTo(path, target)
	})
}

func copyFileTo(src, dst string) (err error) {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Preserve the source file's permission bits so an executable shipped in
	// the bundle (a tool script under skills/ or attachments/) stays runnable
	// after install, rather than being flattened to 0644.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	// Close explicitly and surface its error: on NFS / full disks
	// io.Copy can succeed while the buffered write only fails at Close,
	// so a deferred-and-ignored Close would hide a real write failure.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func countFiles(ents []os.DirEntry) int {
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			n++
		}
	}
	return n
}
