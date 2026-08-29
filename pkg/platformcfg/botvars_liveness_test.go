package platformcfg

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The operator's whole promise in one test: a bot-var written to the
// settings store changes what ${ITERION_X:-default} expands to IN THE
// SAME PROCESS, within the resolver TTL, with no restart — and clearing
// it falls back to the default. This is the runtime-settings signature
// scenario (the usage-cap family pins the same shape).
func TestBotVars_SettingsChangeIsLiveThroughExpansion(t *testing.T) {
	st := NewMemoryStore[BotVars]()
	clock := time.Now()
	r := NewResolver[BotVars](st, nil)
	r.now = func() time.Time { return clock }

	ir.SetEnvOverlay(func(name string) (string, bool) {
		rec := r.Get(context.Background())
		if rec == nil {
			return "", false
		}
		v, ok := rec.Vars[name]
		return v, ok
	})
	defer ir.SetEnvOverlay(nil)

	const form = "${ITERION_VIBE_EFFORT_CLAUDE:-high}"
	if got := ir.ExpandEnvWithDefault(form); got != "high" {
		t.Fatalf("before any setting: %q, want the .bot default", got)
	}

	rec := BotVars{Vars: map[string]string{"ITERION_VIBE_EFFORT_CLAUDE": "max"}}
	if err := rec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := st.Put(context.Background(), rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Within the TTL the old value may still be served (per-replica
	// convergence); past it the new value MUST be.
	clock = clock.Add(DefaultTTL + time.Second)
	if got := ir.ExpandEnvWithDefault(form); got != "max" {
		t.Fatalf("after the write + TTL: %q, want the DB override live with no restart", got)
	}

	if err := st.Put(context.Background(), BotVars{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	clock = clock.Add(DefaultTTL + time.Second)
	if got := ir.ExpandEnvWithDefault(form); got != "high" {
		t.Fatalf("after clearing: %q, want the .bot default back", got)
	}
}
