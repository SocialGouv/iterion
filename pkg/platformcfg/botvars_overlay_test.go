package platformcfg

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// overlayOver builds the overlay cmd wiring installs, over a fixed record,
// and counts what it logs.
func overlayOver(rec *BotVars) (func(string) (string, bool), func() []string) {
	var mu sync.Mutex
	var logged []string
	res := NewResolverFunc[BotVars](func(context.Context) (*BotVars, error) { return rec, nil }, nil)
	overlay := BotVarsOverlay(res, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	return overlay, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), logged...)
	}
}

// Validate guards the admin write only. A record written another way — by a
// binary with an older rule, or by hand — reaches the overlay unchecked, and
// a tool body resolves `{{…}}` AFTER the env expansion: a stored
// `{{vars.forge_publish_token}}` would print the token wherever the var is
// read. The overlay refuses such an entry, as if unset, and says so once.
func TestBotVarsOverlay_RefusesWhatTheWriteRuleRejects(t *testing.T) {
	overlay, logs := overlayOver(&BotVars{Vars: map[string]string{
		"ITERION_VIBE_EFFORT_CLAUDE": "{{vars.forge_publish_token}}",
		"ITERION_VIBE_MODEL_CLAUDE":  "claude-opus-5-5[1m]",
		"ITERION_MONGO_URI":          "mongodb://elsewhere",
	}})

	for i := 0; i < 2; i++ {
		if v, ok := overlay("ITERION_VIBE_EFFORT_CLAUDE"); ok || v != "" {
			t.Fatalf("overlay handed out a stored value the write rule rejects: %q", v)
		}
	}
	if v, ok := overlay("ITERION_MONGO_URI"); ok || v != "" {
		t.Errorf("overlay handed out an infra-namespace override: %q", v)
	}
	if v, ok := overlay("ITERION_VIBE_MODEL_CLAUDE"); !ok || v != "claude-opus-5-5[1m]" {
		t.Errorf("overlay(ITERION_VIBE_MODEL_CLAUDE) = %q, %v — a valid override must still apply", v, ok)
	}
	if v, ok := overlay("ITERION_UNSET"); ok || v != "" {
		t.Errorf("overlay(ITERION_UNSET) = %q, %v, want unset", v, ok)
	}

	got := logs()
	refusals := 0
	for _, l := range got {
		if strings.Contains(l, "ITERION_VIBE_EFFORT_CLAUDE") {
			refusals++
		}
		if strings.Contains(l, "forge_publish_token") {
			t.Errorf("the refusal log carries the stored value: %q", l)
		}
	}
	if refusals != 1 {
		t.Errorf("refusal logged %d times for two lookups, want once: %q", refusals, got)
	}
}

// A refused override reads as unset through the engine's own lookup: the pod
// env answers, never the stored value.
func TestBotVarsOverlay_RefusedEntryFallsBackToThePodEnv(t *testing.T) {
	overlay, _ := overlayOver(&BotVars{Vars: map[string]string{
		"ITERION_VIBE_EFFORT_CLAUDE": "$(id)",
	}})
	ir.SetEnvOverlay(overlay)
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })
	t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "high")

	if got := ir.LookupEnv("ITERION_VIBE_EFFORT_CLAUDE"); got != "high" {
		t.Errorf("LookupEnv = %q, want the pod env's %q", got, "high")
	}
}

// The error names the offending character, whole: a multi-byte one cut at
// its first byte reads as an unrelated escape.
func TestBotVarsValidate_NamesTheWholeCharacter(t *testing.T) {
	for val, want := range map[string]string{"modèle": "'è'", "🚀x": "'🚀'"} {
		err := (BotVars{Vars: map[string]string{"ITERION_VIBE_MODEL_CLAUDE": val}}).Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%q) = %v, want the error to name %s", val, err, want)
		}
	}
}
