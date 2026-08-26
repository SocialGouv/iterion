package server

import (
	"context"
	"fmt"
)

// resolveResumeSource turns a run's persisted file path (plus any inline
// source the caller already has) into the (absolute path, source) pair
// runview.ResumeSpec needs, plus — when the path names a STORED bot (a
// platform override) — the freshly materialized launchBot whose BundleDir/Ref
// the caller stamps on the ResumeSpec (and must Cleanup). A cloud resume
// re-resolves the bot like credentials are re-sealed: the resumed run picks
// up the CURRENT override, consistently across compile and runner
// materialization. persistedSource is the trusted launch snapshot; it is
// only used when an implicit local resume cannot safely resolve the
// recorded path.
//
// Shared by the operator-initiated resume handler and the retry sweeper so
// the cloud-mode rule lives in one place: a server pod has no operator
// filesystem, so a resume must carry inline source UNLESS the path names a
// bot the pod can resolve itself (platform store or baked catalog).
// Duplicating that rule is how the automated path would quietly diverge
// from the manual one.
func (s *Server) resolveResumeSource(ctx context.Context, filePath, source, persistedSource string) (string, string, *launchBot, error) {
	if filePath == "" && source == "" {
		return "", "", nil, fmt.Errorf("file_path or source is required (run has no persisted FilePath)")
	}
	var lb *launchBot
	if s.cfg.Mode == "cloud" && source == "" {
		resolved, err := s.resolveBotTiered(ctx, "", "", filePath)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve bot: %w", err)
		}
		if resolved == nil {
			return "", "", nil, fmt.Errorf("cloud mode: source or a catalog bot is required (file_path is not portable across the server pod's filesystem)")
		}
		lb = resolved
		source = lb.Source
		filePath = lb.Path
	}
	absPath, err := s.resolveWorkflowPath(filePath, source)
	// Dispatcher worktrees live under the managed store, outside the
	// Studio's WorkDir. A child subbot records that absolute path, then the
	// pipeline board resumes its human gate without sending source. The path
	// containment check correctly refuses to open the foreign path, but the
	// run already carries the exact launch source as trusted persisted data.
	// Materialise that snapshot into the server-owned inline cache instead.
	// An explicit source always wins; this fallback only repairs implicit
	// resume of a path the Studio cannot safely resolve.
	if err != nil && source == "" && persistedSource != "" {
		if persistedPath, persistedErr := s.resolveWorkflowPath(filePath, persistedSource); persistedErr == nil {
			return persistedPath, persistedSource, nil, nil
		}
	}
	if err != nil {
		lb.Cleanup()
		return "", "", nil, fmt.Errorf("invalid file_path: %w", err)
	}
	return absPath, source, lb, nil
}
