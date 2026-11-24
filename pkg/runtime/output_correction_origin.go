package runtime

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
)

type usageOriginKey struct{}

// WithUsageOrigin carries queue provenance independently from execution grants.
// No production queue caller installs this inactive protocol yet.
func WithUsageOrigin(ctx context.Context, in store.UsageOrigin) (context.Context, error) {
	origin, err := store.CanonicalUsageOrigin(in)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, usageOriginKey{}, origin), nil
}
func usageOriginForRun(ctx context.Context, runID string) (*store.UsageOrigin, error) {
	origin, ok := ctx.Value(usageOriginKey{}).(store.UsageOrigin)
	if !ok {
		return nil, nil
	}
	if origin.RunID != runID {
		return nil, fmt.Errorf("output correction: usage origin belongs to another run")
	}
	return &origin, nil
}
