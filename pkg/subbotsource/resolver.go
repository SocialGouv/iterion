// Package subbotsource resolves the source reference carried by a subbot node.
//
// The first resolver deliberately supports only the historical filesystem
// form. Keeping that policy behind one seam lets additional source kinds be
// added without duplicating them across the CLI, dispatcher, and studio
// runners.
package subbotsource

import (
	"context"
	"path/filepath"
)

// Kind identifies how a resolved subbot source is supplied.
type Kind string

const (
	// KindFile is a local workflow file, absolute or resolved relative to its
	// parent workflow.
	KindFile Kind = "file"
)

// ResolvedSource is the result of resolving a subbot node's source reference.
// Path retains the historical path semantics: relative references are joined
// to the parent directory, while absolute references are returned unchanged.
type ResolvedSource struct {
	Kind Kind
	Path string
}

// ResolverOptions configures source resolution.
type ResolverOptions struct {
	// ParentlessBaseDir is used when the parent workflow has no file path,
	// which is possible for an inline workflow launched by the studio.
	ParentlessBaseDir string
}

// Resolver resolves subbot source references for one launch surface.
type Resolver struct {
	parentlessBaseDir string
}

// NewResolver constructs a filesystem-only resolver.
func NewResolver(opts ResolverOptions) *Resolver {
	return &Resolver{parentlessBaseDir: opts.ParentlessBaseDir}
}

// Resolve resolves requestedSource using exactly the legacy filesystem rules.
// Context is accepted now so future remote source kinds can perform bounded
// work without changing the runner contract.
func (r *Resolver) Resolve(_ context.Context, parentSource, requestedSource string) (ResolvedSource, error) {
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
