package server

import (
	"context"
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
// Returns ("", noop, nil) when the run was not served by a stored tier, or
// when the caller has named a source itself (`wanted` false): an explicit
// source_path is the operator overriding this resolution outright.
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
		// Resolvable but not materialized: the rewind falls back to the path
		// resolution it had before, and --auto keeps refusing there rather
		// than diffing against whatever that answers.
		if lb != nil {
			lb.Cleanup()
		}
		return "", noop, nil
	}
	return filepath.Join(lb.BundleDir, botsource.MainBotFile), lb.Cleanup, nil
}
