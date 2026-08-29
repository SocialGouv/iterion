// Package subbotsource resolves the source reference carried by a subbot node.
//
// Filesystem references preserve their historical behaviour. bot://
// references resolve through the consumer bundle manifest and the project
// lockfile, then verify the installed bundle before returning any path.
package subbotsource

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// Kind identifies how a resolved subbot source is supplied.
type Kind string

const (
	// KindFile is a local workflow file, absolute or resolved relative to its
	// parent workflow.
	KindFile Kind = "file"
	// KindBot is an exported workflow from a locked, installed bot bundle.
	KindBot Kind = "bot"
)

// Dependency identifies the exact shared workflow resolution used by a run.
type Dependency struct {
	Name          string
	Workflow      string
	BundleVersion string
	BundleSHA256  string
	ResolvedPath  string
}

// ResolvedSource is the result of resolving a subbot node's source reference.
// Path retains the historical path semantics: relative references are joined
// to the parent directory, while absolute references are returned unchanged.
type ResolvedSource struct {
	Kind       Kind
	Path       string
	Bundle     *bundle.Bundle
	Dependency *Dependency
}

// ResolverOptions configures source resolution.
type ResolverOptions struct {
	// ParentlessBaseDir is used when the parent workflow has no file path,
	// which is possible for an inline workflow launched by the studio.
	ParentlessBaseDir string
	// WorkDir contains the committed bots.lock and the materialized .botz
	// directory. When empty, bot:// resolution searches parent directories
	// from the requesting workflow for bots.lock.
	WorkDir string
}

// Resolver resolves subbot source references for one launch surface.
type Resolver struct {
	parentlessBaseDir string
	workDir           string
}

// NewResolver constructs a shared subbot source resolver.
func NewResolver(opts ResolverOptions) *Resolver {
	return &Resolver{parentlessBaseDir: opts.ParentlessBaseDir, workDir: opts.WorkDir}
}

// Resolve resolves requestedSource using exactly the legacy filesystem rules.
// Context is accepted now so future remote source kinds can perform bounded
// work without changing the runner contract.
func (r *Resolver) Resolve(_ context.Context, parentSource, requestedSource string) (ResolvedSource, error) {
	if strings.HasPrefix(requestedSource, "bot://") {
		return r.resolveBot(parentSource, requestedSource)
	}
	base := filepath.Dir(parentSource)
	if parentSource == "" && r.parentlessBaseDir != "" {
		base = r.parentlessBaseDir
	}

	path := requestedSource
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return ResolvedSource{Kind: KindFile, Path: path}, nil
}

func (r *Resolver) resolveBot(parentSource, requestedSource string) (ResolvedSource, error) {
	bundleName, workflowID, err := parseBotURI(requestedSource)
	if err != nil {
		return ResolvedSource{}, err
	}
	consumer, err := bundle.OpenForWorkflow(parentSource)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("bot source %q: inspect consumer bundle: %w", requestedSource, err)
	}
	if consumer == nil || consumer.Manifest == nil {
		return ResolvedSource{}, fmt.Errorf("bot source %q requires the parent workflow to belong to a bundle with manifest.yaml", requestedSource)
	}
	declared := false
	for _, dep := range consumer.Manifest.Dependencies.Workflows {
		if dep.Name == bundleName {
			declared = true
			break
		}
	}
	if !declared {
		return ResolvedSource{}, fmt.Errorf("bot source %q is not declared in the consumer manifest dependencies.workflows", requestedSource)
	}

	workDir, err := r.projectRoot(parentSource)
	if err != nil {
		return ResolvedSource{}, err
	}
	lock, err := botlock.Load(workDir)
	if err != nil {
		return ResolvedSource{}, err
	}
	locked, ok := lock.Dependencies[bundleName]
	if !ok {
		return ResolvedSource{}, fmt.Errorf("bot source %q has no exact %q entry in %s", requestedSource, bundleName, filepath.Join(workDir, botlock.FileName))
	}
	installedPath := filepath.Join(workDir, ".botz", bundleName)
	shared, err := bundle.OpenDir(installedPath)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("bot dependency %q is not materialized at %s; run `iterion bots sync`: %w", bundleName, installedPath, err)
	}
	if shared.Manifest == nil || shared.Manifest.Name != bundleName {
		actual := "<missing>"
		if shared.Manifest != nil {
			actual = shared.Manifest.Name
		}
		return ResolvedSource{}, fmt.Errorf("bot dependency %q does not exactly match installed manifest name %q", bundleName, actual)
	}
	hash, err := bundle.ContentHashDir(shared.Dir)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("bot dependency %q: hash installed bundle: %w", bundleName, err)
	}
	if hash != locked.BundleSHA256 {
		return ResolvedSource{}, fmt.Errorf("bot dependency %q has sha256 %s, lock requires %s; run `iterion bots sync`", bundleName, hash, locked.BundleSHA256)
	}
	var export *bundle.WorkflowExport
	for i := range shared.Manifest.Exports.Workflows {
		if shared.Manifest.Exports.Workflows[i].ID == workflowID {
			export = &shared.Manifest.Exports.Workflows[i]
			break
		}
	}
	if export == nil {
		return ResolvedSource{}, fmt.Errorf("bot dependency %q does not export workflow %q", bundleName, workflowID)
	}
	path := filepath.Join(shared.Dir, filepath.FromSlash(export.Path))
	shared.Hash = hash
	return ResolvedSource{
		Kind: KindBot, Path: path, Bundle: shared,
		Dependency: &Dependency{
			Name: bundleName, Workflow: workflowID, BundleVersion: shared.Manifest.Version,
			BundleSHA256: hash, ResolvedPath: path,
		},
	}, nil
}

func parseBotURI(raw string) (bundleName, workflowID string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "bot" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("invalid bot source %q; expected bot://<bundle>/<workflow-id>", raw)
	}
	workflowID = strings.TrimPrefix(u.EscapedPath(), "/")
	decoded, decodeErr := url.PathUnescape(workflowID)
	if decodeErr != nil || decoded == "" || strings.Contains(decoded, "/") || strings.Contains(decoded, `\`) {
		return "", "", fmt.Errorf("invalid bot source %q; expected one workflow id after the bundle name", raw)
	}
	if strings.Contains(u.Host, ":") || strings.ContainsAny(u.Host, `/\\`) {
		return "", "", fmt.Errorf("invalid bot source %q; bundle name must be exact and contain no port or path separator", raw)
	}
	return u.Host, decoded, nil
}

func (r *Resolver) projectRoot(parentSource string) (string, error) {
	if r.workDir != "" {
		return filepath.Abs(r.workDir)
	}
	start := filepath.Dir(parentSource)
	if parentSource == "" {
		start = r.parentlessBaseDir
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("bot dependencies: resolve project root: %w", err)
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, statErr := os.Stat(filepath.Join(dir, botlock.FileName)); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", fmt.Errorf("bot dependencies: no %s found above %s", botlock.FileName, start)
}
