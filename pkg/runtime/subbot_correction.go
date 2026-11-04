package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

type subbotCorrectionSeed struct {
	parentID, nodeID string
	episodes         map[string]store.OutputCorrectionEpisode
}
type subbotCorrectionKey struct{}

// subbotCorrectionContext reads the prior child before a host replaces its
// attachment. A failed read cannot grant a fresh correction allowance.
func (e *Engine) subbotCorrectionContext(ctx context.Context, rs *runState, nodeID, key string) (context.Context, error) {
	seed := subbotCorrectionSeed{parentID: rs.runID, nodeID: nodeID}
	parent, err := e.store.LoadRun(ctx, rs.runID)
	if err != nil {
		return ctx, fmt.Errorf("load subbot correction parent: %w", err)
	}
	if id := parent.SubbotChildren[key]; id != "" {
		child, err := e.store.LoadRun(ctx, id)
		if err != nil && !store.RunAbsent(err) {
			return ctx, fmt.Errorf("load subbot correction child: %w", err)
		}
		if child != nil && (child.Status.IsFinalFailure() || child.Status.IsTerminalResumable()) {
			seed.episodes = child.OutputCorrections
		}
	}
	return context.WithValue(ctx, subbotCorrectionKey{}, seed), nil
}

// seedSubbotCorrections writes through the common FS/Mongo ledger seam before
// a fresh child runs. An existing child's own episodes always take precedence.
func (e *Engine) seedSubbotCorrections(ctx context.Context, run *store.Run) error {
	seed, ok := ctx.Value(subbotCorrectionKey{}).(subbotCorrectionSeed)
	if !ok || seed.parentID != e.parentRunID || seed.nodeID != e.parentNodeID {
		return nil
	}
	for key, ep := range seed.episodes {
		if _, exists := run.OutputCorrections[key]; exists {
			continue
		}
		ep.LastError = e.recordedFailureText(ep.LastError)
		if err := e.persistCorrectionEpisode(ctx, run.ID, key, ep); err != nil {
			return err
		}
		if run.OutputCorrections == nil {
			run.OutputCorrections = make(map[string]store.OutputCorrectionEpisode)
		}
		run.OutputCorrections[key] = ep
	}
	return nil
}
