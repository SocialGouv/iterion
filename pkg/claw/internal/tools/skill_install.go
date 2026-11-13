package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MarketplaceURL is the canonical source for installing official skills.
// Hardcoded by design — third-party sources are not configurable in this
// scope.
const MarketplaceURL = "https://github.com/anthropics/claude-plugins-official.git"

// marketplaceCacheTTL controls when we refresh the local clone.
const marketplaceCacheTTL = 24 * time.Hour

// marketplacePlugin describes the subset of the marketplace.json schema we
// care about for SkillInstall.
type marketplacePlugin struct {
	Name    string          `json:"name"`
	Source  json.RawMessage `json:"source"` // can be string ("./plugins/x") or object
	Skills  []string        `json:"-"`      // populated by directory walk
	Plugins []string        `json:"-"`
}

type marketplaceManifest struct {
	Plugins []marketplacePlugin `json:"plugins"`
}

// InstallOptions controls SkillInstall behavior. Zero value is the default
// (no force, network allowed).
type InstallOptions struct {
	// Force overwrites an existing skill directory.
	Force bool
	// Destination overrides ~/.claude/skills/. Mainly for tests.
	Destination string
	// CacheRoot overrides the marketplace cache directory. Mainly for tests.
	CacheRoot string
	// MarketplaceURL overrides MarketplaceURL. Mainly for tests.
	MarketplaceURL string
}

// InstallSkillFromMarketplace installs the named skill into the user's
// local skills directory by fetching it from the official Anthropic
// marketplace.
//
// The name accepts "<plugin>:<skill>", "<plugin>/<skill>" or a bare
// "<skill>" (resolved via the marketplace structure).
func InstallSkillFromMarketplace(name string, opts InstallOptions) (installedPath string, err error) {
	plugin, skill, err := parseInstallName(name)
	if err != nil {
		return "", err
	}

	url := opts.MarketplaceURL
	if url == "" {
		url = MarketplaceURL
	}

	cache, err := marketplaceCacheDir(opts.CacheRoot)
	if err != nil {
		return "", err
	}

	if err := ensureMarketplaceClone(cache, url); err != nil {
		return "", fmt.Errorf("skill install: %w", err)
	}

	// If plugin is empty, resolve via the marketplace manifest.
	if plugin == "" {
		plugin, err = resolvePluginForSkill(cache, skill)
		if err != nil {
			return "", err
		}
	}

	if err := assertAnthropicDirect(cache, plugin); err != nil {
		return "", err
	}

	src := filepath.Join(cache, "plugins", plugin, "skills", skill)
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return "", fmt.Errorf("skill install: %s:%s not found in marketplace", plugin, skill)
	}

	destRoot := opts.Destination
	if destRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("skill install: cannot resolve home dir: %w", err)
		}
		destRoot = filepath.Join(home, ".claude", "skills")
	}

	dest := filepath.Join(destRoot, plugin, "skills", skill)
	if _, err := os.Stat(dest); err == nil {
		if !opts.Force {
			return dest, fmt.Errorf("skill install: %s already exists at %s (use force to overwrite)", plugin+":"+skill, dest)
		}
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("skill install: cannot remove existing %s: %w", dest, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("skill install: cannot create parent dirs: %w", err)
	}

	if err := copyTree(src, dest); err != nil {
		return "", fmt.Errorf("skill install: copy failed: %w", err)
	}

	// Verify the installed SKILL.md has a frontmatter (best-effort warning).
	if content, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err == nil {
		if _, _, ok := parseSkill(content); !ok {
			fmt.Fprintf(os.Stderr, "[skill install] warning: %s/SKILL.md has no YAML frontmatter\n", dest)
		}
	}

	return dest, nil
}

func parseInstallName(name string) (plugin, skill string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("skill install: name required")
	}
	if idx := strings.Index(name, ":"); idx > 0 && idx < len(name)-1 {
		return name[:idx], name[idx+1:], nil
	}
	if idx := strings.Index(name, "/"); idx > 0 && idx < len(name)-1 {
		return name[:idx], name[idx+1:], nil
	}
	return "", name, nil
}

func marketplaceCacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "claw", "marketplace", "claude-plugins-official"), nil
}

func ensureMarketplaceClone(cache, url string) error {
	stat, err := os.Stat(filepath.Join(cache, ".git"))
	switch {
	case err == nil && stat.IsDir():
		// Existing clone — refresh if stale.
		if time.Since(modTime(cache)) > marketplaceCacheTTL {
			cmd := exec.Command("git", "-C", cache, "fetch", "--depth=1", "origin")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git fetch failed: %v: %s", err, out)
			}
			cmd = exec.Command("git", "-C", cache, "reset", "--hard", "origin/HEAD")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git reset failed: %v: %s", err, out)
			}
			_ = os.Chtimes(cache, time.Now(), time.Now())
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
			return err
		}
		cmd := exec.Command("git", "clone", "--depth", "1", url, cache)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone failed: %v: %s", err, out)
		}
		return nil
	default:
		return err
	}
}

func modTime(p string) time.Time {
	if info, err := os.Stat(p); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// resolvePluginForSkill scans the marketplace clone for any plugin that ships
// a skill matching the bare name. Returns an error if zero or multiple
// plugins match.
func resolvePluginForSkill(cache, skill string) (string, error) {
	pluginsDir := filepath.Join(cache, "plugins")
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return "", fmt.Errorf("skill install: cannot read marketplace plugins: %w", err)
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(pluginsDir, e.Name(), "skills", skill)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			matches = append(matches, e.Name())
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("skill install: %q not found in any marketplace plugin", skill)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("skill install: %q is ambiguous; matches plugins: %s — use plugin:skill form", skill, strings.Join(matches, ", "))
	}
}

// assertAnthropicDirect confirms the plugin is shipped directly inside the
// marketplace repo (source = "./plugins/<name>"). External sources are
// rejected with an actionable error.
func assertAnthropicDirect(cache, plugin string) error {
	manifestPath := filepath.Join(cache, ".claude-plugin", "marketplace.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("skill install: cannot read marketplace manifest: %w", err)
	}
	var manifest marketplaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("skill install: cannot parse marketplace manifest: %w", err)
	}
	for _, p := range manifest.Plugins {
		if p.Name != plugin {
			continue
		}
		// source may be a string ("./plugins/x") or an object with
		// {"source": "git-subdir"|"url", "url": "..."}.
		if isAnthropicDirectSource(p.Source) {
			return nil
		}
		return fmt.Errorf("skill install: %s lives in an external plugin repo; install manually via 'git clone <upstream> ~/.claude/skills/%s/'", plugin, plugin)
	}
	// Plugin not listed in manifest but exists on disk — accept (covers
	// transitional states upstream).
	return nil
}

func isAnthropicDirectSource(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.HasPrefix(s, "./plugins/")
	}
	var obj struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Source == ""
	}
	return false
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "[skill install] warning: skipping symlink %s (target %s not copied)\n", p, target)
			return nil
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
