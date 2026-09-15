package runview

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveSharedResumeSpec validates and restores the exact workflow export
// recorded on a shared-bundle child run. v1 shared dependencies are local
// materialized directory bundles; cloud subbots remain outside ADR-094.
func resolveSharedResumeSpec(r *store.Run, spec *ResumeSpec) error {
	if r == nil || r.BundleWorkflow == "" {
		return nil
	}
	bundleDir := spec.BundleDir
	if bundleDir == "" {
		bundleDir = r.BundlePath
	}
	if bundleDir == "" {
		return fmt.Errorf("runview: run %q shared dependency has no persisted bundle path", r.ID)
	}
	b, err := bundle.OpenDir(bundleDir)
	if err != nil {
		return fmt.Errorf("runview: open shared dependency %s: %w", bundleDir, err)
	}
	path, err := runtime.ResolveResumeBundleWorkflow(r, b, spec.FilePath, spec.Force)
	if err != nil {
		return err
	}
	spec.FilePath = path
	spec.BundleDir = b.Dir
	return nil
}
