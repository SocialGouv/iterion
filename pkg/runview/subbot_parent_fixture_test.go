package runview

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// boundLegacySubbotFixture declares the existing fixtures' original legacy
// intention without reading or creating a parent. It is never a production
// fallback, and intentionally is not used by missing/foreign-binding probes.
func boundLegacySubbotFixture(t *testing.T, ctx context.Context, parentID string) context.Context {
	t.Helper()
	bound, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: parentID, Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}
