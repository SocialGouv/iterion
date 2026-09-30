package runview

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveSharedResumeSpec restores the exact workflow export recorded on a
// shared-bundle child run, and returns aside the refusal of a dependency whose
// identity changed since the run started: the caller refuses it after the
// scratch and the lineage, as the engine does. v1 shared dependencies are
// local materialized directory bundles; cloud subbots remain outside ADR-094.
func resolveSharedResumeSpec(r *store.Run, spec *ResumeSpec) (identityErr error, err error) {
	if r == nil || r.BundleWorkflow == "" {
		return nil, nil
	}
	bundleDir := spec.BundleDir
	if bundleDir == "" {
		bundleDir = r.BundlePath
	}
	if bundleDir == "" {
		return nil, fmt.Errorf("runview: run %q shared dependency has no persisted bundle path", r.ID)
	}
	b, err := bundle.OpenDir(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("runview: open shared dependency %s: %w", bundleDir, err)
	}
	path, identityErr, err := runtime.ResumeBundleWorkflow(r, b, spec.FilePath)
	if err != nil {
		return nil, err
	}
	spec.FilePath = path
	spec.BundleDir = b.Dir
	return identityErr, nil
}
