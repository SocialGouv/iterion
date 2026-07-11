package marketplace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// SourceInfo is what InspectSource reads out of a submitted source: the
// detected artifact kind plus exactly one populated payload (Bot for
// KindBot, Plugin for KindPlugin).
type SourceInfo struct {
	Kind   Kind
	Bot    *botinstall.Metadata
	Plugin *plugin.InspectInfo
}

// InspectSource resolves a submitted source (git URL or local directory)
// ONCE and detects what it holds: a plugin (plugin.yaml at the inspected
// directory) or a bot bundle (main.bot + manifest.yaml, resolved with
// botinstall's repo conventions). It is the kind-dispatching front of the
// marketplace submit flow — one clone serves both probes.
//
// source may carry a `url#ref` suffix (botinstall convention); an explicit
// ref argument wins over it. path selects a subdirectory inside the repo
// (or, for bots, an iterion-bots.yaml bot name — see botinstall).
//
// Detection order: plugin.Inspect first. Only its "no plugin.yaml at all"
// miss (plugin.ErrNoManifest) falls through to the bot probe; a present
// but malformed plugin.yaml propagates as-is — never silently reread as a
// bot. When both probes miss, the returned error carries both causes.
func InspectSource(ctx context.Context, source, ref, path string) (*SourceInfo, error) {
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("marketplace: a git URL or local path is required")
	}
	url, srcRef := splitSourceRef(source)
	if ref == "" {
		ref = srcRef
	}

	root, cleanup, err := materializeSource(ctx, url, ref)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// The plugin probe looks at the path-selected directory directly; the
	// bot probe below gets the repo root + raw path instead, so botinstall's
	// richer path semantics (iterion-bots.yaml names, bundle scan) apply.
	pluginDir := root
	if path != "" {
		if !filepath.IsLocal(path) {
			return nil, fmt.Errorf("marketplace: path %q must be a relative path inside the repository", path)
		}
		pluginDir = filepath.Join(root, path)
	}

	info, perr := plugin.Inspect(ctx, pluginDir)
	if perr == nil {
		return &SourceInfo{Kind: KindPlugin, Plugin: info}, nil
	}
	if !errors.Is(perr, plugin.ErrNoManifest) {
		// plugin.yaml exists but is unusable — surface that, don't guess bot.
		return nil, perr
	}

	md, berr := botinstall.Inspect(ctx, botinstall.Options{Source: root, Path: path})
	if berr != nil {
		return nil, fmt.Errorf("marketplace: not a plugin (no plugin.yaml) and not a bot bundle: %w", berr)
	}
	return &SourceInfo{Kind: KindBot, Bot: md}, nil
}

// EntryFromPlugin builds the registry Entry for an inspected plugin
// source. The slug derives from the manifest name via the same
// normalization the bot submit path uses (botregistry.NormalizeName), and
// Categories carry the manifest's contribution kinds so the studio can
// group plugins by type. The caller stamps timestamps, tags and any
// cloud scope/moderation fields.
func EntryFromPlugin(info *plugin.InspectInfo, repoURL, ref, path string) Entry {
	m := info.Manifest
	return Entry{
		Slug:        botregistry.NormalizeName(m.Name),
		Kind:        KindPlugin,
		Categories:  m.Kinds(),
		Name:        m.Name,
		Description: m.Description,
		Author:      m.Author,
		Version:     m.Version,
		README:      info.README,
		RepoURL:     repoURL,
		Ref:         ref,
		Subpath:     path,
		Source:      SourceGit,
	}
}

// splitSourceRef splits "url#ref" into (url, ref), mirroring botinstall:
// a '#' whose prefix is an existing local path is part of the path, not a
// ref marker.
func splitSourceRef(src string) (url, ref string) {
	if i := strings.LastIndex(src, "#"); i > 0 {
		if _, err := os.Stat(src[:i]); err != nil {
			return src[:i], src[i+1:]
		}
	}
	return src, ""
}

// materializeSource resolves source to a local directory exactly once: a
// local directory is used in place (cleanup is a no-op); anything else is
// shallow-cloned into a temp dir (ShallowClone validates the URL via
// gitlib.ValidateCloneSource before spawning git).
func materializeSource(ctx context.Context, source, ref string) (root string, cleanup func(), err error) {
	cleanup = func() {}
	if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		abs, aerr := filepath.Abs(source)
		if aerr != nil {
			return "", cleanup, aerr
		}
		return abs, cleanup, nil
	}
	tmp, terr := os.MkdirTemp("", "iterion-marketplace-inspect-*")
	if terr != nil {
		return "", cleanup, terr
	}
	dest := filepath.Join(tmp, "repo")
	if cerr := gitlib.ShallowClone(ctx, source, ref, dest); cerr != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, cerr
	}
	return dest, func() { _ = os.RemoveAll(tmp) }, nil
}
