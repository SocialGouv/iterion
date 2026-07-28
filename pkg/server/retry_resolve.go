package server

import (
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Launch-time resolution of a run's retry policy.
//
// The chain cannot be resolved anywhere else. A bot cannot probe the
// schedule that launched it; a schedule row does not know the bot's
// manifest; the runner sees neither and must not have to. Only the launch
// site sees every layer at once — the same reason the review topology is
// resolved out of band here rather than in the DSL.
//
// So it is resolved once, at launch, and snapshotted on the run document.
// The runner then reads the doc it already loads: no queue-schema bump, no
// lookups from the pod, and a permanent record of what the run was promised
// even if the schedule is edited or deleted afterwards.

// resolveRunRetryPolicy resolves the effective retry policy for a launch and
// returns the snapshot to persist on the run.
//
// `higher` carries the layers the CALLER knows about, highest priority
// first — typically a per-run override and/or the launching surface's own
// policy (a schedule row, a trigger subscription). The bot manifest, the
// machine default and the platform ceiling are appended here so no launch
// site has to remember them, and so a site that passes nothing still gets a
// correct policy rather than an empty one.
func (s *Server) resolveRunRetryPolicy(botID string, higher ...retrypolicy.Layer) *store.RunRetryPolicy {
	layers := make([]retrypolicy.Layer, 0, len(higher)+2)
	layers = append(layers, higher...)
	if m := s.botManifest(botID); m != nil {
		layers = append(layers, retrypolicy.Layer{Source: retrypolicy.SourceBot, Policy: m.RetryPolicy()})
	}
	layers = append(layers, retrypolicy.Layer{Source: retrypolicy.SourceEnv, Policy: retrypolicy.FromEnv()})

	pol, sources := retrypolicy.Resolve(layers...)
	pol = retrypolicy.Clamp(pol, retrypolicy.CeilingFromEnv(), sources)

	return &store.RunRetryPolicy{
		UsageWindow: pol.UsageWindow,
		MaxAttempts: pol.MaxAttempts,
		MaxWait:     pol.MaxWait,
		Jitter:      pol.Jitter,
		Sources:     sources,
	}
}
