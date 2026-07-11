package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/git"
)

// InstallOptions parameterizes InstallWith. Source is a local directory or a
// git URL (required); Ref pins a branch or tag for a git source; Subpath
// selects a plugin directory inside the source (for monorepos shipping several
// plugins).
type InstallOptions struct {
	Source  string
	Ref     string
	Subpath string
}

// Install installs a plugin from a local directory or git URL into
// ~/.iterion/plugins/<name>/ and returns the installed plugin's name. It is
// InstallWith with only a source. Both the CLI (`iterion plugin install`) and
// the HTTP server (POST /api/v1/plugins/install) call this so the behaviour is
// identical on either surface.
func Install(ctx context.Context, src string) (string, error) {
	return InstallWith(ctx, InstallOptions{Source: src})
}

// InstallWith installs a plugin per opts into ~/.iterion/plugins/<name>/ and
// returns the installed plugin's name. A git source is shallow-cloned (at
// opts.Ref when set); a local path's plugin.yaml is validated then copied.
// When the (sub)source has no plugin.yaml but ships bare skills (a public
// skill library), a skills-only manifest is synthesized and persisted into the
// install dir.
//
// InstallWith only places files — it never executes plugin code; the cloned
// repo's .git metadata is stripped (see copyTree).
func InstallWith(ctx context.Context, opts InstallOptions) (string, error) {
	reg, err := Load()
	if err != nil {
		return "", err
	}
	src := opts.Source
	srcDir := src
	if isGitURL(src) {
		parent, terr := os.MkdirTemp("", "iterion-plugin-clone-*")
		if terr != nil {
			return "", terr
		}
		defer os.RemoveAll(parent)
		// git.ShallowClone requires an absent dest and gates the URL via
		// ValidateCloneSource (https/ssh only — ext::/file:// rejected). That
		// transport guard matters here because the studio install endpoint
		// clones an operator-supplied URL server-side.
		dest := filepath.Join(parent, "src")
		if cerr := git.ShallowClone(ctx, src, opts.Ref, dest); cerr != nil {
			return "", cerr
		}
		srcDir = dest
	} else if opts.Ref != "" {
		return "", fmt.Errorf("plugin: ref %q given for local source %q (refs apply to git sources only)", opts.Ref, src)
	}
	if opts.Subpath != "" {
		// The subpath is operator/marketplace input; reject anything that would
		// escape the source root (absolute paths, ".." traversal).
		if !filepath.IsLocal(opts.Subpath) {
			return "", fmt.Errorf("plugin: subpath %q escapes the source root", opts.Subpath)
		}
		srcDir = filepath.Join(srcDir, opts.Subpath)
	}
	var m *Manifest
	synthesized := false
	if data, rerr := os.ReadFile(filepath.Join(srcDir, ManifestFile)); rerr == nil {
		if m, err = ParseManifest(data); err != nil {
			return "", err
		}
	} else if os.IsNotExist(rerr) {
		// No plugin.yaml — treat the source as a public skill library: collect
		// its bare skills and synthesize a skills-only manifest so it becomes a
		// first-class, enable/disable-able plugin. The name derives from the
		// subpath when one selected the plugin, else from the source itself.
		nameSrc := src
		if opts.Subpath != "" {
			nameSrc = opts.Subpath
		}
		if m, err = SynthesizeSkillsManifest(nameSrc, srcDir); err != nil {
			return "", err
		}
		synthesized = true
	} else {
		return "", fmt.Errorf("plugin: read %s in %q: %w", ManifestFile, src, rerr)
	}

	dest := reg.InstallDir(m.Name)
	if _, ok := reg.Get(m.Name); ok {
		// Overwrite an existing installed plugin (upgrade); builtins of the
		// same name still win at load and the install is shadowed — warn-free
		// here, surfaced by `plugin list`.
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			return "", rmErr
		}
	}
	if err := copyTree(srcDir, dest); err != nil {
		return "", fmt.Errorf("plugin: install %q: %w", m.Name, err)
	}
	// Persist the synthesized manifest into the install dir (the source had
	// none) so the registry loads the skill library like any other plugin.
	if synthesized {
		if err := WriteManifest(dest, m); err != nil {
			return "", err
		}
	}
	return m.Name, nil
}

// Uninstall removes an installed plugin. Builtins cannot be uninstalled
// (Registry.Remove rejects them — disable instead).
func Uninstall(name string) error {
	reg, err := Load()
	if err != nil {
		return err
	}
	return reg.Remove(name)
}

func isGitURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

// copyTree recursively copies srcDir into dstDir (files + dirs), skipping a
// nested .git directory so a cloned plugin doesn't carry its repo metadata.
func copyTree(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	// Stable order for deterministic installs.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		s := filepath.Join(srcDir, e.Name())
		d := filepath.Join(dstDir, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
