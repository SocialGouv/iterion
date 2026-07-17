package forge

import (
	"context"
	"errors"
)

// RepoCreateSpec describes a repository to create on a forge. Owner is
// the org/group/namespace login; empty targets the credential's own
// namespace.
type RepoCreateSpec struct {
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	InitReadme    bool
}

// RepoCreator is the optional capability of creating a NEW repository on
// the forge. Deliberately separate from Admin (list/webhook surface):
// creation is a broader privilege — on GitHub Apps it is minted per call
// (administration:write never rides the cached runtime token) — and only
// providers/credentials that support it expose the interface (callers
// type-assert). Iterion only ever CREATES repositories through this
// seam: no update, no delete, no touching existing repos.
type RepoCreator interface {
	CreateRepo(ctx context.Context, spec RepoCreateSpec) (RepoSummary, error)
}

// ErrRepoExists reports a create-time name collision on the forge.
var ErrRepoExists = errors.New("forge: repository already exists")
