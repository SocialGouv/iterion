package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A detached runner can pick up a row admitted before activation was
// disabled. Only a genuinely new root needs today's launch authority.
func requireNativeCLIStart(ctx context.Context, s store.RunStore, semantics, runID string) error {
	r, err := s.LoadRun(ctx, runID)
	if err == nil {
		if r.RuntimeSemantics != semantics {
			return fmt.Errorf("cli: queued run and source use different interpreters: %w", store.ErrRunSemantics)
		}
		return portsactivation.RequireExistingAdmission(ctx, s, r)
	}
	if !errors.Is(err, store.ErrRunNotFound) {
		return err
	}
	return portsactivation.RequireLaunch(ctx, s, semantics, runID)
}
