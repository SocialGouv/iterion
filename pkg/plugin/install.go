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

// Install installs a plugin from a local directory or git URL into
// ~/.iterion/plugins/<name>/ and returns the installed plugin's name. A git URL
// is shallow-cloned; a local path's plugin.yaml is validated then copied. When
// the source has no plugin.yaml but ships bare skills (a public skill library),
// a skills-only manifest is synthesized and persisted into the install dir.
//
// Install only places files — it never executes plugin code; the cloned repo's
// .git metadata is stripped (see copyTree). Both the CLI (`iterion plugin
// install`) and the HTTP server (POST /api/v1/plugins/install) call this so the
// behaviour is identical on either surface.
func Install(ctx context.Context, src string) (string, error) {
	reg, err := Load()
	if err != nil {
		return "", err
	}
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
		if cerr := git.ShallowClone(ctx, src, "", dest); cerr != nil {
			return "", cerr
		}
		srcDir = dest
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
		// first-class, enable/disable-able plugin.
		if m, err = SynthesizeSkillsManifest(src, srcDir); err != nil {
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
