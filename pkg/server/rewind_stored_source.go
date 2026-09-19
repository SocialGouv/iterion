package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// currentStoredBotSource materializes, for a run served by a STORED bot tier,
// the CURRENT version of that bot, and returns the path of its main — the
// program a rewind means by "as it is now".
//
// A team bot or a platform override has no current source on this pod:
// `run.FilePath` is a logical label (`bots/<slug>/main.bot`), and
// resolveWorkflowPath answers it with the BAKED catalog twin — a fallback
// written so the studio's diagram view has something compilable to draw. Right
// for a picture, wrong for a rewind: a team bot exists precisely to differ from
// the baked one.
//
// It is needed for EVERY rewind of such a run, not only `--auto`. The diff is
// the visible consumer, but the compile of that same source yields the GRAPH,
// and the graph is what decides which nodes are downstream of the pivot —
// which outputs are dropped and which artifacts are tombstoned. A `--node`
// rewind names its own pivot and still computes its blast radius from the
// graph, so serving it from the twin drops the wrong set: measured at both an
// under-drop (a stale downstream output survives) and an over-drop (an
// UPSTREAM node's artifact tombstoned).
//
// The same row is re-resolved at its current version, exactly as a resume of a
// stored bot does — never a path string re-derived from the tier, which once
// swapped a team bot onto a same-slug platform override.
//
// Returns ("", 0, "", noop, nil) when the run was not served by a stored
// tier, or when the caller has named a source itself (`wanted` false): an
// explicit source_path is the operator overriding this resolution outright.
func (s *Server) currentStoredBotSource(ctx context.Context, run *store.Run, wanted bool) (string, func(), error) {
	path, _, _, rel, err := s.storedBotSourceAtVersion(ctx, run, 0, "", wanted)
	return path, rel, err
}

// storedBotSourceAtVersion is currentStoredBotSource with a version pin:
// version > 0 resolves THAT botsource row version rather than the current
// row — the write half of the #1381 pin. The mission's rewind preview
// certifies a row version and records it on the receipt
// (ActionReceipt.SourceVersion beside SourceID); the apply resolves the
// PINNED row, so a bot republished between the two coordinator passes
// cannot move the blast radius onto a graph the preview never saw. The pin
// is identity-keyed (row id + version): a slug deleted and re-authored in
// between mints a new row whose versions never serve the old pin. version
// == 0 keeps the current-row resolution (the preview's own pass, and every
// non-mission rewind: the HTTP endpoint, --auto).
//
// The resolved version and row id are returned alongside the path so the
// preview can pin them (a stored-tier launchBot always carries both; zeros
// for the non-stored tiers).
func (s *Server) storedBotSourceAtVersion(ctx context.Context, run *store.Run, version int, pinnedRowID string, wanted bool) (string, int, string, func(), error) {
	noop := func() {}
	if !wanted || run == nil {
		return "", 0, "", noop, nil
	}
	if !run.ServedByStoredBot() {
		return "", 0, "", noop, nil
	}
	lb, err := s.resolveResumeBotAtVersion(ctx, run.BotSourceTenant, run.FilePath, version, pinnedRowID)
	if err != nil {
		return "", 0, "", noop, err
	}
	if lb == nil || lb.BundleDir == "" {
		// Resolvable but not materialized: the rewind falls back to the path
		// resolution it had before, and --auto keeps refusing there rather
		// than diffing against whatever that answers.
		if lb != nil {
			lb.Cleanup()
		}
		return "", 0, "", noop, nil
	}
	resolvedVersion, resolvedRowID := 0, ""
	if lb.Ref != nil {
		resolvedVersion = lb.Ref.Version
	}
	resolvedRowID = lb.sourceRowID
	return filepath.Join(lb.BundleDir, botsource.MainBotFile), resolvedVersion, resolvedRowID, lb.Cleanup, nil
}

// resolveResumeBotAtVersion is resolveResumeBot's stored-tier branch with a
// version pin: version > 0 reads THAT row version from the botsource
// version history — identified by pinnedRowID, the identity the receipt
// pinned — instead of the current one, so the materialized program is
// byte-for-byte the one the preview certified. The rest of the layering
// (the prompt merge snapshot) is shared — prompts do not take part in the
// compiled graph the blast radius is computed from, so pinning the row
// pins the graph. version == 0 delegates to resolveResumeBot unchanged.
func (s *Server) resolveResumeBotAtVersion(ctx context.Context, botSourceTenant, filePath string, version int, pinnedRowID string) (*launchBot, error) {
	if version <= 0 {
		return s.resolveResumeBot(ctx, botSourceTenant, filePath)
	}
	if botSourceTenant == "" || s.botSources == nil {
		return nil, fmt.Errorf("cannot pin version %d of %q: it does not name a stored bot on a tenant", version, filePath)
	}
	bs, err := s.botSources.GetByVersion(store.WithTenant(ctx, botSourceTenant), botSourceTenant, pinnedRowID, version)
	if err != nil {
		if errors.Is(err, botsource.ErrNotFound) {
			// The pinned identity is gone (the row deleted, its slug
			// re-authored as a NEW row whose versions cannot serve this pin,
			// or the store reset): an explicit refusal, never a fall-through
			// to the current row — that would recompute the blast radius in
			// a program the preview never certified (#1381).
			return nil, fmt.Errorf("the stored bot version this preview certified (tenant %s, row %s, version %d) no longer exists; re-propose the rewind against the current version: %w", botSourceTenant, pinnedRowID, version, err)
		}
		return nil, fmt.Errorf("%w: resolve stored bot %s/%s at version %d: %v", errResumeResolveTransient, botSourceTenant, pinnedRowID, version, err)
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
	return s.snapshotResumeBot(ctx, teamID, lb)
}
