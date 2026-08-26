package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveResumeSource turns a run's persisted file path (plus any inline
// source the caller already has) into the (absolute path, source) pair
// runview.ResumeSpec needs, plus — when the run launched from a STORED bot
// (team or platform) — the freshly materialized launchBot whose
// BundleDir/Ref the caller stamps on the ResumeSpec (and must Cleanup).
//
// botSourceTenant is the run's persisted launch origin
// (Run.BotSourceTenant): a resume re-resolves the SAME row, fresh version —
// never re-derives the tier from a path string, which silently swapped a
// team bot's resume onto a same-slug platform override. The resolution runs
// even when the caller supplies inline source: the source then wins for the
// compile, but the bundle ref/dir must still reach the runner or the resume
// silently attaches the STALE BAKED bundle. persistedSource is the trusted
// launch snapshot; it is only used when an implicit local resume cannot
// safely resolve the recorded path.
//
// Shared by the operator-initiated resume handler and the retry sweeper so
// the cloud-mode rule lives in one place: a server pod has no operator
// filesystem, so a resume must carry inline source UNLESS it names a bot
// the pod can resolve itself. Duplicating that rule is how the automated
// path would quietly diverge from the manual one.
func (s *Server) resolveResumeSource(ctx context.Context, botSourceTenant, filePath, source, persistedSource string) (string, string, *launchBot, error) {
	if filePath == "" && source == "" {
		return "", "", nil, fmt.Errorf("file_path or source is required (run has no persisted FilePath)")
	}
	var lb *launchBot
	if s.cfg.Mode == "cloud" {
		resolved, err := s.resolveResumeBot(ctx, botSourceTenant, filePath)
		if err != nil {
			return "", "", nil, err
		}
		lb = resolved
		if lb != nil && source == "" {
			source = lb.Source
			filePath = lb.Path
		}
		if source == "" {
			return "", "", nil, fmt.Errorf("cloud mode: source or a catalog bot is required (file_path is not portable across the server pod's filesystem)")
		}
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

// resolveResumeBot re-resolves the bot a run launched from. With a
// persisted origin it targets THAT tier's row directly; a row that
// vanished is an explicit error naming the remedy (the operator deleted
// the stored bot mid-run — a silent fall-through to another tier would
// resume on content nobody chose). Without an origin (a baked catalog /
// legacy run) it keeps the launch-surface resolution: platform override
// first, then the baked catalog — so an override pushed after the launch
// applies on resume, matching "effective at the next launch".
func (s *Server) resolveResumeBot(ctx context.Context, botSourceTenant, filePath string) (*launchBot, error) {
	slug := inferCatalogBotID(filePath)
	if botSourceTenant != "" {
		if slug == "" || s.botSources == nil {
			return nil, fmt.Errorf("run launched from stored bot (tenant %s) but %q does not name it — resume with inline source", botSourceTenant, filePath)
		}
		bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, botSourceTenant), botSourceTenant, slug)
		if err != nil {
			if errors.Is(err, botsource.ErrNotFound) {
				return nil, fmt.Errorf("the stored bot this run launched from (tenant %s, slug %s) no longer exists — it was deleted after the launch; relaunch the bot, or resume with inline source", botSourceTenant, slug)
			}
			return nil, fmt.Errorf("resolve stored bot %s/%s: %w", botSourceTenant, slug, err)
		}
		origin := "team"
		if botsource.IsPlatform(botSourceTenant) {
			origin = "platform"
		}
		return s.storedLaunchBot(bs, origin)
	}
	lb, err := s.resolveBotTiered(ctx, "", "", filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve bot: %w", err)
	}
	return lb, nil
}
