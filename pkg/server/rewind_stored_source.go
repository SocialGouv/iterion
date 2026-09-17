package server

import (
	"context"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// currentStoredBotSource materializes, for a run served by a STORED bot tier,
// the CURRENT version of that bot, and returns the path of its main — what
// `rewind --auto` diffs the recorded source against.
//
// It exists because the two sides of that diff must be the same artifact. A
// team bot or a platform override has no current source on this pod:
// `run.FilePath` is a logical label (`bots/<slug>/main.bot`), and
// resolveWorkflowPath answers it with the BAKED catalog twin — a fallback
// written so the studio's diagram view has something compilable to draw. Right
// for a picture, wrong for a rewind: a team bot exists precisely to differ from
// the baked one, so every declaration would read as changed and the pivot would
// land on the entry node.
//
// The same row is re-resolved at its current version, exactly as a resume of a
// stored bot does — never a path string re-derived from the tier, which once
// swapped a team bot onto a same-slug platform override.
//
// Returns ("", noop, nil) when the run was not served by a stored tier, or when
// the caller is not asking for an --auto diff: materializing a bundle to answer
// a `--node` rewind would be work nobody asked for.
func (s *Server) currentStoredBotSource(ctx context.Context, run *store.Run, wanted bool) (string, func(), error) {
	noop := func() {}
	if !wanted || run == nil {
		return "", noop, nil
	}
	if !run.ServedByStoredBot() {
		return "", noop, nil
	}
	lb, err := s.resolveResumeBot(ctx, run.BotSourceTenant, run.FilePath)
	if err != nil {
		return "", noop, err
	}
	if lb == nil || lb.BundleDir == "" {
		// Resolvable but not materialized: --auto keeps refusing rather than
		// diffing against whatever the path resolves to.
		if lb != nil {
			lb.Cleanup()
		}
		return "", noop, nil
	}
	return filepath.Join(lb.BundleDir, botsource.MainBotFile), lb.Cleanup, nil
}
