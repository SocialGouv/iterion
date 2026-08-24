package usagecap

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Platform runtime settings — the DB-backed form of the ITERION_USAGE_CAP_*
// env defaults, so changing a cap in production is one authenticated API
// call instead of a `kubectl set env` on two deployments and a rolling
// restart (the doctrine the platform LLM credentials established:
// env = default, DB record = runtime override, effective without restart).
//
// The record holds only what an operator may retune at runtime: the two
// percentages. The MODES (soft/hard) and the ITERION_USAGE_CAP kill switch
// stay env-only — they encode the deployment's enforcement posture, and a
// posture change is a decision a redeploy should witness. In particular the
// kill switch WINS over a DB percentage: with ITERION_USAGE_CAP=off the
// resolved env policy carries no mode, so an overridden percentage can
// never re-arm a guard the operator explicitly disarmed.

// Settings is the platform-scoped runtime-settings record. A nil field
// means "no override — inherit the env default", which is what keeps a
// deployment that never touches the API at exactly its env-configured
// behaviour.
type Settings struct {
	// FiveHourPct overrides ITERION_USAGE_CAP_5H_PCT (0–100; 0 = no cap).
	FiveHourPct *int `bson:"five_hour_pct,omitempty" json:"five_hour_pct"`
	// WeekPct overrides ITERION_USAGE_CAP_WEEK_PCT (0–100; 0 = no cap).
	WeekPct *int `bson:"week_pct,omitempty" json:"week_pct"`
	// UpdatedAt / UpdatedBy record the last mutation, for the read surface;
	// the authoritative trail is the platform audit log.
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// Validate rejects out-of-range overrides. Integer 0–100, same contract as
// the env vars; the API layer rejects non-integers before they get here.
func (s Settings) Validate() error {
	check := func(name string, v *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > 100 {
			return fmt.Errorf("usagecap: %s: %d is out of range (want an integer 0–100)", name, *v)
		}
		return nil
	}
	if err := check("five_hour_pct", s.FiveHourPct); err != nil {
		return err
	}
	return check("week_pct", s.WeekPct)
}

// Apply lays the record's overrides over the env-resolved defaults and
// returns the EFFECTIVE policy. Only percentages are overridden; each
// window keeps its env-resolved mode — which is also what makes the
// ITERION_USAGE_CAP=off kill switch final: a zero Policy has no modes, so
// an overridden percentage stays inert.
func (s *Settings) Apply(def Policy) Policy {
	pol := def
	if s == nil {
		return pol
	}
	if s.FiveHourPct != nil {
		pol.FiveHour.MaxPercent = float64(*s.FiveHourPct)
	}
	if s.WeekPct != nil {
		pol.Week.MaxPercent = float64(*s.WeekPct)
	}
	return pol
}

// SettingsStore persists the platform runtime-settings record — the same
// tier as the platform credential records (Mongo in cloud, memory for
// tests). There is exactly one record per deployment.
type SettingsStore interface {
	// GetSettings returns the record, or (nil, nil) when none was ever
	// written — which must read as "env defaults apply".
	GetSettings(ctx context.Context) (*Settings, error)
	// PutSettings replaces the record.
	PutSettings(ctx context.Context, s Settings) error
}

// MemorySettingsStore is the in-process SettingsStore for tests and
// local single-process wiring.
type MemorySettingsStore struct {
	mu  sync.RWMutex
	rec *Settings
}

// NewMemorySettingsStore builds an empty in-process settings store.
func NewMemorySettingsStore() *MemorySettingsStore { return &MemorySettingsStore{} }

// GetSettings returns a copy of the record, or nil when unset.
func (m *MemorySettingsStore) GetSettings(context.Context) (*Settings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.rec == nil {
		return nil, nil
	}
	cp := *m.rec
	return &cp, nil
}

// PutSettings replaces the record.
func (m *MemorySettingsStore) PutSettings(_ context.Context, s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rec = &s
	return nil
}
