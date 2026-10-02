package runner

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// contributionsFromWire converts the queue's contribution payload into the
// engine's domain type (the same wire-mirror split as queue.BudgetOverrides →
// ir.BudgetOverrides).
//
// The payload is what the launching instance read from ITS iterion home:
// enabled-plugin markdown plus the skill-library skills the workflow
// references. This pod has neither on disk, so this is the only way an
// operator-installed plugin's skill reaches the workspace here.
func contributionsFromWire(c *queue.Contributions) *runtime.Contributions {
	if c == nil {
		return nil
	}
	out := &runtime.Contributions{}
	for _, f := range c.Plugin {
		out.Plugin = append(out.Plugin, runtime.ContributionFile{
			Kind:    f.Kind,
			Name:    f.Name,
			Content: f.Content,
		})
	}
	for _, s := range c.Library {
		out.Library = append(out.Library, runtime.LibrarySkillFile{
			Name:        s.Name,
			Description: s.Description,
			Content:     s.Content,
		})
	}
	out.Degraded = c.Degraded
	return out
}

// contributionsEngineOptions carries the dispatch's contributions payload into
// the engine. A nil payload is an anomaly on the queue, not a statement: the
// publisher resolves the launching instance's set on every launch AND every
// resume and ships the result — possibly empty, never lost — so nil means the
// field did not arrive. The engine is told the ambient declaration is
// unresolved (WithContributionsUnresolved) instead of being left to a local
// resolution that proves nothing on a pod whose iterion home is empty by
// design; a pass that cannot verify the declaration must not bless the orphan
// pruner (#1500 R6 medium).
//
// Shared by the root dispatch (loop.go) and every subbot child dispatch
// (subbot.go) so the two cannot drift.
func contributionsEngineOptions(c *queue.Contributions, logger *iterlog.Logger) []runtime.EngineOption {
	if c != nil {
		return []runtime.EngineOption{runtime.WithContributions(contributionsFromWire(c))}
	}
	if logger != nil {
		logger.Warn("runner: dispatch arrived without a contributions payload — the ambient plugin/library declaration cannot be verified on this pod; the mirror pass will not bless this pass for orphan pruning")
	}
	return []runtime.EngineOption{runtime.WithContributionsUnresolved()}
}
