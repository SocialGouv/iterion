package trigger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestConcurrentCreatesLandExactlyOneBinding is the race Revi named: a
// caller's list-then-create cannot promise uniqueness, so the store's
// Create owns it. Six goroutines race the same binding; exactly one wins
// and the tenant holds one row.
func TestConcurrentCreatesLandExactlyOneBinding(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		var ok, refused atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				err := st.Create(ctx, Subscription{
					ID: "s" + string(rune('0'+i)), TenantID: "t1", BotID: "b", Enabled: true,
					Invocation:   bundle.InvocationKindConnector,
					ConnectionID: "conn-a", TriggerID: "slack.message.posted",
				})
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrDuplicateConnectorBinding):
					refused.Add(1)
				default:
					t.Errorf("create %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if ok.Load() != 1 || refused.Load() != 5 {
			t.Fatalf("landed %d, refused %d — want exactly 1 and 5", ok.Load(), refused.Load())
		}
		rows, err := st.ListByTenant(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("the tenant holds %d rows, want 1", len(rows))
		}
	})
}
