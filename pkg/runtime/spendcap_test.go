package runtime

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/store"
)

// memSpendStore is an in-memory store.SpendStore for guard unit tests.
type memSpendStore struct {
	mu   sync.Mutex
	days map[string]*store.DailySpend
}

func newMemSpendStore() *memSpendStore {
	return &memSpendStore{days: map[string]*store.DailySpend{}}
}

func (m *memSpendStore) get(date string) *store.DailySpend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyTestDailySpend(m.getLocked(date))
}

func (m *memSpendStore) getLocked(date string) *store.DailySpend {
	ds, ok := m.days[date]
	if !ok {
		ds = &store.DailySpend{Date: date, RunsContributed: map[string]float64{}}
		m.days[date] = ds
	}
	return ds
}

func (m *memSpendStore) LoadDailySpend(_ context.Context, date string) (*store.DailySpend, error) {
	return m.get(date), nil
}

func (m *memSpendStore) AddSpend(_ context.Context, date, runID string, cum float64) (*store.DailySpend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ds := m.getLocked(date)
	if prev, ok := ds.RunsContributed[runID]; !ok || cum > prev {
		ds.RunsContributed[runID] = cum
	}
	var total float64
	for _, c := range ds.RunsContributed {
		total += c
	}
	ds.SpentUSD = total
	return copyTestDailySpend(ds), nil
}

func (m *memSpendStore) SetSpendOverride(_ context.Context, date string, ov *store.SpendOverride) (*store.DailySpend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ds := m.getLocked(date)
	ds.Override = ov
	return copyTestDailySpend(ds), nil
}

// Like the filesystem store, each call returns its own snapshot. Callers read
// the result after the lock is released while sibling branches keep writing.
func copyTestDailySpend(ds *store.DailySpend) *store.DailySpend {
	copy := *ds
	copy.RunsContributed = maps.Clone(ds.RunsContributed)
	if ds.Override != nil {
		override := *ds.Override
		copy.Override = &override
	}
	return &copy
}

func TestDailyCapGuardDisabled(t *testing.T) {
	// Non-positive limit → nil guard, all methods inert.
	g := NewDailyCapGuard(newMemSpendStore(), clock.Default, DailyCapConfig{MaxCostPerDayUSD: 0})
	if g != nil {
		t.Fatalf("expected nil guard for zero limit, got %v", g)
	}
	st, err := g.Status(context.Background())
	if err != nil || st.Enabled {
		t.Errorf("nil-guard Status = %+v, err=%v; want disabled", st, err)
	}
}

func TestDailyCapGuardCrossesCap(t *testing.T) {
	ctx := context.Background()
	mem := newMemSpendStore()
	clk := clock.NewFakeClock(time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC))
	g := NewDailyCapGuard(mem, clk, DailyCapConfig{MaxCostPerDayUSD: 1.00})

	st, err := g.Record(ctx, "run-a", 0.50)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if st.Exceeded {
		t.Errorf("under cap should not be exceeded: %+v", st)
	}

	st, _ = g.Record(ctx, "run-a", 1.20)
	if !st.Exceeded {
		t.Errorf("over cap should be exceeded: %+v", st)
	}
	if st.SpentUSD != 1.20 || st.LimitUSD != 1.00 {
		t.Errorf("status numbers = %+v, want spent 1.20 / limit 1.00", st)
	}
}

func TestDailyCapGuardOverrideClearsExceeded(t *testing.T) {
	ctx := context.Background()
	mem := newMemSpendStore()
	clk := clock.NewFakeClock(time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC))
	g := NewDailyCapGuard(mem, clk, DailyCapConfig{MaxCostPerDayUSD: 1.00})

	if _, err := g.Record(ctx, "run-a", 2.00); err != nil {
		t.Fatalf("Record: %v", err)
	}
	st, _ := g.Status(ctx)
	if !st.Exceeded {
		t.Fatalf("precondition: should be exceeded")
	}

	st, err := g.SetOverride(ctx, true, "operator", "ship it")
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if st.Exceeded {
		t.Errorf("override should clear Exceeded: %+v", st)
	}
	if !st.OverrideActive {
		t.Errorf("OverrideActive should be true: %+v", st)
	}
}

func TestDailyCapGuardResetsNextDay(t *testing.T) {
	ctx := context.Background()
	mem := newMemSpendStore()
	clk := clock.NewFakeClock(time.Date(2026, 5, 30, 23, 0, 0, 0, time.UTC))
	g := NewDailyCapGuard(mem, clk, DailyCapConfig{MaxCostPerDayUSD: 1.00})

	if _, err := g.Record(ctx, "run-a", 5.00); err != nil {
		t.Fatalf("Record: %v", err)
	}
	st, _ := g.Status(ctx)
	if !st.Exceeded {
		t.Fatalf("day 1 should be exceeded")
	}

	// Roll into the next UTC day — a fresh ledger means the cap resets.
	clk.Advance(2 * time.Hour) // 2026-05-31 01:00
	st, _ = g.Status(ctx)
	if st.Exceeded {
		t.Errorf("next day should reset, got exceeded: %+v", st)
	}
	if st.SpentUSD != 0 {
		t.Errorf("next day SpentUSD = %v, want 0", st.SpentUSD)
	}
	if st.Date != "2026-05-31" {
		t.Errorf("Date = %q, want 2026-05-31", st.Date)
	}
}
