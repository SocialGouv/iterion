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

// A pinned name is the process environment's, through the engine's own
// expansion: the record pins the shared fleet's vendor id, the pool's env
// block names its gateway, and the node's `${ITERION_VIBE_MODEL_CLAUDE:-…}`
// reads the gateway. An unpinned name still takes the record.
func TestPinnedOverlay_APinnedNameIsThePodEnvs(t *testing.T) {
	record, _ := overlayOver(&BotVars{Vars: map[string]string{
		"ITERION_VIBE_MODEL_CLAUDE":  "zai/glm-5.3",
		"ITERION_VIBE_EFFORT_CLAUDE": "max",
	}})
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE", "openai_compatible/scaleway/glm-5.3")
	t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "high")
	t.Setenv("ITERION_VIBE_PROVIDER_CLAUDE", "openai_compatible")
	ir.SetEnvOverlay(PinnedOverlay(mustPins(t, "ITERION_VIBE_MODEL_CLAUDE,ITERION_VIBE_PROVIDER_CLAUDE"), record))
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	if got := ir.ExpandEnvWithDefault("${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}"); got != "openai_compatible/scaleway/glm-5.3" {
		t.Errorf("pinned model dial = %q, want the pod env's gateway id — the record reached a pinned name", got)
	}
	if got := ir.ExpandEnvWithDefault("${ITERION_VIBE_PROVIDER_CLAUDE:-anthropic}"); got != "openai_compatible" {
		t.Errorf("pinned provider dial = %q, want the pod env's %q", got, "openai_compatible")
	}
	if got := ir.ExpandEnvWithDefault("${ITERION_VIBE_EFFORT_CLAUDE:-medium}"); got != "max" {
		t.Errorf("unpinned effort dial = %q, want the record's %q (setting > pod env holds off the pins)", got, "max")
	}
}

// A pinned name the process leaves empty falls to the .bot default, never to
// the record: the pin is the deployment's word on the name.
func TestPinnedOverlay_AnEmptyPinnedNameTakesTheBotDefault(t *testing.T) {
	record, _ := overlayOver(&BotVars{Vars: map[string]string{"ITERION_VIBE_MODEL_CLAUDE": "zai/glm-5.3"}})
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE", "")
	ir.SetEnvOverlay(PinnedOverlay(mustPins(t, "ITERION_VIBE_MODEL_CLAUDE"), record))
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	if got := ir.ExpandEnvWithDefault("${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}"); got != "claude-opus-5-5" {
		t.Errorf("empty pinned dial = %q, want the .bot default", got)
	}
}

// Without pins the overlay is the record's, byte for byte: the shared fleet
// keeps setting > pod env > .bot default.
func TestEnvOverlayFor_WithoutPinsTheRecordReachesEveryName(t *testing.T) {
	res := NewResolverFunc[BotVars](func(context.Context) (*BotVars, error) {
		return &BotVars{Vars: map[string]string{"ITERION_VIBE_MODEL_CLAUDE": "zai/glm-5.3"}}, nil
	}, nil)
	var infos []string
	env := func(name string) string { return map[string]string{EnvBotVarsEnvPins: " , "}[name] }
	overlay := EnvOverlayFor(res, env, nil, func(f string, a ...any) { infos = append(infos, fmt.Sprintf(f, a...)) })
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE", "openai_compatible/scaleway/glm-5.3")
	ir.SetEnvOverlay(overlay)
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	if got := ir.LookupEnv("ITERION_VIBE_MODEL_CLAUDE"); got != "zai/glm-5.3" {
		t.Errorf("LookupEnv = %q, want the record's value when nothing is pinned", got)
	}
	if len(infos) != 0 {
		t.Errorf("a blank pin list logged %q — nothing is pinned, nothing to say", infos)
	}
}

// EnvOverlayFor reads the pin list from the environment it is given and says
// which names it pinned — sorted, names only.
func TestEnvOverlayFor_PinsFromTheEnvironmentAndSaysSo(t *testing.T) {
	res := NewResolverFunc[BotVars](func(context.Context) (*BotVars, error) {
		return &BotVars{Vars: map[string]string{
			"ITERION_VIBE_MODEL_CLAUDE": "zai/glm-5.3",
			"ITERION_VIBE_MODEL_EMIT":   "zai/glm-5.3",
		}}, nil
	}, nil)
	var infos []string
	env := func(name string) string {
		return map[string]string{EnvBotVarsEnvPins: "ITERION_VIBE_MODEL_EMIT, ITERION_VIBE_MODEL_CLAUDE,,"}[name]
	}
	overlay := EnvOverlayFor(res, env, nil, func(f string, a ...any) { infos = append(infos, fmt.Sprintf(f, a...)) })

	for _, name := range []string{"ITERION_VIBE_MODEL_CLAUDE", "ITERION_VIBE_MODEL_EMIT"} {
		if v, ok := overlay(name); ok || v != "" {
			t.Errorf("overlay(%s) = %q, %v — a pinned name must answer no override", name, v, ok)
		}
	}
	if v, ok := overlay("ITERION_VIBE_EFFORT_CLAUDE"); ok || v != "" {
		t.Errorf("overlay(ITERION_VIBE_EFFORT_CLAUDE) = %q, %v, want unset (not in the record)", v, ok)
	}
	if len(infos) != 1 || !strings.Contains(infos[0], "2 name(s) pinned") ||
		!strings.Contains(infos[0], "ITERION_VIBE_MODEL_CLAUDE,ITERION_VIBE_MODEL_EMIT") {
		t.Errorf("pin log = %q, want one line naming both pins, sorted", infos)
	}
}

// mustPins parses a pin list that is expected to be well-formed.
func mustPins(t *testing.T, raw string) map[string]bool {
	t.Helper()
	pins, malformed := ParseEnvPins(raw)
	if len(malformed) != 0 {
		t.Fatalf("ParseEnvPins(%q) refused %q", raw, malformed)
	}
	return pins
}

// ParseEnvPins: whitespace and empty tokens dropped, nil for nothing, and a
// token that is not an ITERION_ upper-snake name pins nothing and is
// returned as malformed — a space- or semicolon-separated hand-spelled
// list, a lowercase name, a glob.
func TestParseEnvPins(t *testing.T) {
	pins, malformed := ParseEnvPins(" ITERION_A , ,ITERION_B,,ITERION_C ")
	if len(pins) != 3 || !pins["ITERION_A"] || !pins["ITERION_B"] || !pins["ITERION_C"] || len(malformed) != 0 {
		t.Errorf("ParseEnvPins = %v, %v, want three pins and nothing malformed", pins, malformed)
	}
	if pins, malformed := ParseEnvPins(" , "); pins != nil || malformed != nil {
		t.Errorf("ParseEnvPins(blank) = %v, %v, want nil, nil", pins, malformed)
	}
	if got := SortedPinNames(mustPins(t, "ITERION_B,ITERION_A")); strings.Join(got, ",") != "ITERION_A,ITERION_B" {
		t.Errorf("SortedPinNames = %v, want sorted", got)
	}
	for _, raw := range []string{
		"ITERION_VIBE_MODEL_CLAUDE ITERION_VIBE_PROVIDER_CLAUDE",
		"ITERION_VIBE_MODEL_CLAUDE;ITERION_VIBE_PROVIDER_CLAUDE",
		"iterion_vibe_model_claude",
		"ITERION_VIBE_*",
		"VIBE_MODEL_CLAUDE",
		"ITERION_",
	} {
		pins, malformed := ParseEnvPins(raw)
		if pins != nil || len(malformed) != 1 || malformed[0] != raw {
			t.Errorf("ParseEnvPins(%q) = %v, %v — want nothing pinned and the token reported", raw, pins, malformed)
		}
	}
	if pins, malformed := ParseEnvPins("ITERION_OK, bad token ,ITERION_ALSO"); len(pins) != 2 || len(malformed) != 1 {
		t.Errorf("ParseEnvPins(mixed) = %v, %v — want the two names pinned and the bad token reported", pins, malformed)
	}
}

// A malformed list is said so (warn, the tokens named) and pins nothing —
// a hand-spelled list that silently pins nothing is the defect it was
// spelled against. No pin line for a list that pinned nothing.
func TestEnvOverlayFor_WarnsOnMalformedPinTokens(t *testing.T) {
	res := NewResolverFunc[BotVars](func(context.Context) (*BotVars, error) {
		return &BotVars{Vars: map[string]string{"ITERION_VIBE_MODEL_CLAUDE": "zai/glm-5.3"}}, nil
	}, nil)
	var warns, infos []string
	env := func(name string) string {
		return map[string]string{EnvBotVarsEnvPins: "ITERION_VIBE_MODEL_CLAUDE ITERION_VIBE_PROVIDER_CLAUDE"}[name]
	}
	overlay := EnvOverlayFor(res, env,
		func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) },
		func(f string, a ...any) { infos = append(infos, fmt.Sprintf(f, a...)) })

	if v, ok := overlay("ITERION_VIBE_MODEL_CLAUDE"); !ok || v != "zai/glm-5.3" {
		t.Errorf("overlay(ITERION_VIBE_MODEL_CLAUDE) = %q, %v — a malformed list pins nothing, the record applies", v, ok)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "1 token(s) are not ITERION_ names") ||
		!strings.Contains(warns[0], "ITERION_VIBE_MODEL_CLAUDE ITERION_VIBE_PROVIDER_CLAUDE") {
		t.Errorf("warns = %q, want one line naming the malformed token", warns)
	}
	if len(infos) != 0 {
		t.Errorf("infos = %q, want no pin line when nothing was pinned", infos)
	}
}

// EnvOverlayFor composes the CHECKED record read: with pins present, a stored
// value the write rule rejects, under an unpinned name, still reads as unset
// — never handed out raw.
func TestEnvOverlayFor_ComposesTheCheckedOverlay(t *testing.T) {
	res := NewResolverFunc[BotVars](func(context.Context) (*BotVars, error) {
		return &BotVars{Vars: map[string]string{
			"ITERION_VIBE_EFFORT_CLAUDE": "{{vars.forge_publish_token}}",
			"ITERION_VIBE_MODEL_CLAUDE":  "zai/glm-5.3",
		}}, nil
	}, nil)
	env := func(name string) string {
		return map[string]string{EnvBotVarsEnvPins: "ITERION_VIBE_MODEL_CLAUDE"}[name]
	}
	overlay := EnvOverlayFor(res, env, nil, nil)

	if v, ok := overlay("ITERION_VIBE_EFFORT_CLAUDE"); ok || v != "" {
		t.Errorf("overlay handed out a stored value the write rule rejects: %q", v)
	}
	if v, ok := overlay("ITERION_VIBE_MODEL_CLAUDE"); ok || v != "" {
		t.Errorf("overlay(ITERION_VIBE_MODEL_CLAUDE) = %q, %v — pinned, must answer no override", v, ok)
	}
}

// The pin list is the process's own: the write gate refuses it as a stored
// name, so a record cannot choose which names it does not reach.
func TestBotVarsValidate_RefusesThePinListName(t *testing.T) {
	err := (BotVars{Vars: map[string]string{EnvBotVarsEnvPins: "ITERION_VIBE_MODEL_CLAUDE"}}).Validate()
	if err == nil {
		t.Fatalf("Validate accepted %s as a stored bot var", EnvBotVarsEnvPins)
	}
}
