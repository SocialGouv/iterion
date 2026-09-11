package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// errResumeResolveTransient marks a stored-bot resolution failure that may
// clear on its own (a store blip) — the retry sweeper re-arms on it instead
// of abandoning the retry, and callers must not treat it as "bot deleted".
var errResumeResolveTransient = errors.New("resume: stored-bot resolution transiently failed")

// resumeSourceFiller is the runview.ResumeSourceFiller the server wires into
// the run service: it covers every Resume call that arrives WITHOUT a
// resolved source — answer-human (HTTP + WS) and the ADR-081 async
// auto-resume — so a stored-bot run resumes on its own tiers instead of the
// pod's baked twin. Cloud-only: a local resume never touches the bot-source
// store, and its path semantics stay byte-identical to the pre-seam code.
func (s *Server) resumeSourceFiller(ctx context.Context, run *store.Run, spec *runview.ResumeSpec) (func(), error) {
	if s.cfg.Mode != "cloud" {
		if run != nil && spec != nil && spec.Source == "" {
			resolved, lb, ok, err := s.resolveLegacyCatalogResume(ctx, run, spec.FilePath)
			if err != nil {
				return nil, err
			}
			if ok {
				spec.FilePath = resolved
				if lb != nil {
					spec.BundleDir, spec.BotBundle = lb.BundleDir, lb.Ref
				}
				return lb.Cleanup, nil
			}
		}
		return nil, nil
	}
	filePath := spec.FilePath
	if filePath == "" {
		filePath = run.FilePath
	}
	absPath, source, lb, err := s.resolveResumeSource(ctx, run.BotSourceTenant, filePath, spec.Source, run.WorkflowSource)
	if err != nil {
		return nil, err
	}
	spec.FilePath, spec.Source = absPath, source
	if lb != nil {
		spec.BundleDir, spec.BotBundle = lb.BundleDir, lb.Ref
	}
	return lb.Cleanup, nil
}

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
// Shared by the operator-initiated resume handler, the retry sweeper, and
// (through resumeSourceFiller) every bare runview.Resume, so the cloud-mode
// rule lives in one place: a server pod has no operator filesystem, so a
// resume must carry inline source UNLESS it names a bot the pod can resolve
// itself. Duplicating that rule is how the automated path would quietly
// diverge from the manual one.
func (s *Server) resolveResumeSource(ctx context.Context, botSourceTenant, filePath, source, persistedSource string, identity ...*store.Run) (string, string, *launchBot, error) {
	return s.resolveResumeSourceWithFallback(ctx, botSourceTenant, filePath, source, persistedSource, true, identity...)
}

// resolveResumeSourceWithFallback is the shared resolver with an explicit
// switch for the persisted inline-source fallback. An omitted file_path is
// allowed to recover an old dispatcher/worktree path from the trusted launch
// snapshot; an explicitly supplied file_path must be authoritative and must
// fail closed when it cannot be resolved.
func (s *Server) resolveResumeSourceWithFallback(ctx context.Context, botSourceTenant, filePath, source, persistedSource string, allowPersistedFallback bool, identity ...*store.Run) (string, string, *launchBot, error) {
	if filePath == "" && source == "" {
		return "", "", nil, fmt.Errorf("file_path or source is required (run has no persisted FilePath)")
	}
	if s.cfg.Mode != "cloud" && source == "" && len(identity) > 0 && identity[0] != nil {
		resolved, lb, ok, err := s.resolveLegacyCatalogResume(ctx, identity[0], filePath)
		if err != nil {
			return "", "", nil, err
		}
		if ok {
			return resolved, "", lb, nil
		}
	}
	var lb *launchBot
	if s.cfg.Mode == "cloud" {
		resolved, rerr := s.resolveResumeBot(ctx, botSourceTenant, filePath, source)
		switch {
		case rerr == nil:
			lb = resolved
		case source != "" && errors.Is(rerr, botsource.ErrNotFound):
			// The stored row is gone but the caller brought its own source:
			// let the resume run on it. The bundle ref is dropped — the
			// runner falls back to the baked bundle if the slug names one,
			// which is the documented delete-reverts-to-baked semantics.
			// LOUD, never silent.
			s.logger.Warn("resume: stored bot (tenant %s) for %s no longer exists — resuming on the caller's inline source with the baked bundle (if any): %v", botSourceTenant, filePath, rerr)
		default:
			return "", "", nil, rerr
		}
		if lb != nil && source == "" {
			source = lb.Source
			filePath = lb.Path
		}
		if source == "" && persistedSource != "" {
			// Not a resolvable bot and the caller brought nothing — an
			// inline-source cloud launch. The persisted launch snapshot is
			// the trusted source to resume on (same rationale as the
			// dispatcher-worktree fallback below).
			source = persistedSource
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
	if allowPersistedFallback && err != nil && source == "" && persistedSource != "" {
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

// resolveLegacyCatalogResume repairs local runs created before catalog bot
// identity was persisted. Such runs either point at an immutable
// server-owned inline/embedded source snapshot or at a catalog path outside
// the active WorkDir. The former is correct for ordinary inline workflows
// but becomes stale for a catalog bot after the catalog changes; the latter
// cannot pass safePath even though it is a configured catalog source. Only a
// server-owned cache or a catalog-shaped path qualifies; arbitrary user paths
// and stored-bot origins remain untouched. The normal workflow hash check in
// runview.Resume still decides whether a force resume is needed.
func (s *Server) resolveLegacyCatalogResume(ctx context.Context, run *store.Run, filePath string) (string, *launchBot, bool, error) {
	if run == nil {
		return filePath, nil, false, nil
	}
	serverCache := s.isServerOwnedWorkflowCache(filePath)
	if !serverCache {
		// A catalog path may live outside WorkDir (the local Copi setup does).
		// Require safePath to reject it and require the path's catalog shape so
		// a normal arbitrary workspace file is never rebound by its workflow
		// name alone.
		if _, err := s.safePath(filePath); err == nil || inferCatalogBotID(filePath) == "" {
			return filePath, nil, false, nil
		}
	}

	names := make([]string, 0, 4)
	if inferred := inferCatalogBotID(filePath); inferred != "" {
		names = append(names, inferred)
	}
	names = append(names, run.BotID, run.BundleName, run.WorkflowName)
	if len(names) == 0 {
		return filePath, nil, false, nil
	}
	seen := make(map[string]struct{}, 3)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		lb, err := s.resolveBotTiered(ctx, "", name, "")
		if err != nil {
			return "", nil, false, fmt.Errorf("resolve catalog bot %q for legacy resume: %w", name, err)
		}
		if lb == nil {
			continue
		}
		if lb.Origin != "catalog" {
			// A legacy cache does not prove whether a stored override supplied
			// it. Do not silently replace it with a different tier.
			return filePath, nil, false, nil
		}
		return lb.Path, lb, true, nil
	}
	return filePath, nil, false, nil
}

// resolveResumeBot re-resolves the bot a run launched from. With a
// persisted origin it targets THAT tier's row directly; a row that
// vanished is an explicit error naming the remedy (the operator deleted
// the stored bot mid-run — a silent fall-through to another tier would
// resume on content nobody chose). Without an origin (a baked catalog /
// legacy run) it keeps the launch-surface resolution: platform override
// first, then the baked catalog — so an override pushed after the launch
// applies on resume, matching "effective at the next launch".
func (s *Server) resolveResumeBot(ctx context.Context, botSourceTenant, filePath string, sourceOverride ...string) (*launchBot, error) {
	slug := inferCatalogBotID(filePath)
	if botSourceTenant != "" {
		if slug == "" || s.botSources == nil {
			return nil, fmt.Errorf("run launched from stored bot (tenant %s) but %q does not name it — resume with inline source", botSourceTenant, filePath)
		}
		bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, botSourceTenant), botSourceTenant, slug)
		if err != nil {
			if errors.Is(err, botsource.ErrNotFound) {
				// %w so the inline-source path above (and the sweeper) can
				// tell "row deleted" from a transient store failure.
				return nil, fmt.Errorf("the stored bot this run launched from (tenant %s, slug %s) no longer exists — it was deleted after the launch; relaunch the bot, or resume with inline source: %w", botSourceTenant, slug, err)
			}
			// Transient (a store blip): typed so the retry sweeper RE-ARMS
			// instead of permanently abandoning a paid usage-window retry.
			return nil, fmt.Errorf("%w: resolve stored bot %s/%s: %v", errResumeResolveTransient, botSourceTenant, slug, err)
		}
		origin := "team"
		if botsource.IsPlatform(botSourceTenant) {
			origin = "platform"
		}
		lb, err := s.storedLaunchBot(bs, origin)
		if err != nil {
			return nil, err
		}
		teamID := botSourceTenant
		if botsource.IsPlatform(teamID) {
			teamID = ""
		}
		return s.snapshotResumeBot(ctx, teamID, lb, sourceOverride...)
	}
	lb, err := s.resolveBotTieredRaw(ctx, "", "", filePath)
	if err != nil {
		// resolveBotTiered only errors on a store/FS failure — a genuine
		// "not found" returns (nil, nil). A blip is transient here exactly
		// as on the persisted-origin branch above: typed so the retry
		// sweeper RE-ARMS instead of permanently abandoning the retry.
		return nil, fmt.Errorf("%w: resolve bot: %v", errResumeResolveTransient, err)
	}
	if lb != nil && s.cfg.Mode == "cloud" {
		return s.snapshotResumeBot(ctx, "", lb, sourceOverride...)
	}
	return lb, nil
}

func (s *Server) snapshotResumeBot(ctx context.Context, teamID string, lb *launchBot, sourceOverride ...string) (*launchBot, error) {
	resolved, err := s.snapshotLaunchBot(ctx, teamID, lb, sourceOverride...)
	if errors.Is(err, errBotSnapshotResolve) {
		return nil, fmt.Errorf("%w: %v", errResumeResolveTransient, err)
	}
	return resolved, err
}
