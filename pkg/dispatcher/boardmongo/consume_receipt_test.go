package boardmongo_test

import (
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"sync"
	"testing"
)

type receiptStore interface {
	Create(native.Issue) (*native.Issue, error)
	Get(string) (*native.Issue, error)
	Update(string, native.Patch) (*native.Issue, error)
	ConsumeLabelsForEffect(string, []string, string) (bool, error)
}

func runConsumeReceiptSuite(t *testing.T, st receiptStore) {
	t.Helper()
	iss, err := st.Create(native.Issue{Title: "receipt", State: native.StateInbox, Labels: []string{"once", "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	// Competing effects cannot both own one label generation.
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := []string{}
	for _, key := range []string{"effect-a", "effect-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			ok, err := st.ConsumeLabelsForEffect(iss.ID, []string{"once"}, key)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				winners = append(winners, key)
				mu.Unlock()
			}
		}(key)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("owners=%v", winners)
	}
	// A later, unrelated edit must preserve the receipt (also on Mongo).
	title := "edited"
	if _, err := st.Update(iss.ID, native.Patch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := st.ConsumeLabelsForEffect(iss.ID, []string{"once"}, winners[0]); err != nil || !ok {
			t.Fatalf("recover own consume: %v %v", ok, err)
		}
	}
	card, err := st.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Labels) != 1 || card.Labels[0] != "keep" {
		t.Fatalf("labels=%v", card.Labels)
	}
	labels := []string{"once", "keep"}
	if _, err := st.Update(iss.ID, native.Patch{Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	// Replaying an old receipt must not spend the NEW operator gesture.
	if ok, err := st.ConsumeLabelsForEffect(iss.ID, []string{"once"}, winners[0]); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := st.ConsumeLabelsForEffect(iss.ID, []string{"once"}, "effect-new"); err != nil || !ok {
		t.Fatalf("rearmed consume: %v %v", ok, err)
	}
}
func TestRound2NativeConsumeReceipt(t *testing.T) {
	st, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runConsumeReceiptSuite(t, st)
}
