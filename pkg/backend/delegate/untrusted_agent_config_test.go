package delegate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestEverySpawnOfANodeCarriesTheAgentConfigFloor is the ADR-162 U3 witness
// for claude_code: asserted BY VALUE over every argv one Execute assembles —
// the main pass AND the structured-output formatting pass, which is a second
// spawn resuming the same session in the same checkout — never over a named
// spawn. A spawn added tomorrow without the floor fails the same loop.
//
// Two readers, one resolution: the --setting-sources flag, and whether
// iterion's own hooks ride the inline --settings object. They follow the
// RESOLVED sources — inline exactly when the spawn loads no `project` scope —
// so a fork keeps the operator's hooks and a trusted default does not
// configure them twice.
//
// Mutations: withoutCheckoutScopes dropped from claudeAmbient reddens every
// fork row on its own --setting-sources assertion; the loadsProjectSource
// guard in claudeSpawnPins muted to `false` reddens the trusted-default row's
// no-inline assertion; muted to always-inline it reddens the fork row the
// other way.
func TestEverySpawnOfANodeCarriesTheAgentConfigFloor(t *testing.T) {
	hooks := map[string][]any{"Stop": {map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": "true"}},
	}}}
	schema := []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	for _, row := range []struct {
		name        string
		trust       store.RunTrust
		legacy      string // "" = ITERION_CLAUDE_CODE_SETTING_SOURCES stays absent
		wantSources string
		wantInline  bool
	}{
		{"fork, the default policy", store.RunTrustFork, "", "user", true},
		{"fork narrowed to none", store.RunTrustFork, "none", "", true},
		{"trusted, the default — the project file carries the hooks", store.RunTrustDefault, "", "user,project", false},
		{"trusted with none — no project file, so they come inline", store.RunTrustDefault, "none", "", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.legacy == "" {
				unsetSettingSourcesEnv(t)
			} else {
				t.Setenv(settingSourcesEnv, row.legacy)
			}
			rec := spawnRecorded(t, Task{
				NodeID:         "n",
				AmbientContext: ambient.All,
				WorkspaceTrust: row.trust,
				InjectedHooks:  hooks,
				OutputSchema:   schema,
			})
			if len(rec.argv) < 2 {
				t.Fatalf("expected the main pass AND the formatting pass, got %d spawn(s) — this row proves nothing: %v", len(rec.argv), rec.argv)
			}
			for i, argv := range rec.argv {
				// Split on single spaces, not Fields: an EMPTY argument —
				// `--setting-sources ""` — is exactly the value this test
				// must be able to see.
				args := strings.Split(argv, " ")
				got := "<absent>"
				for j, a := range args {
					if a == "--setting-sources" && j+1 < len(args) {
						got = args[j+1]
					}
				}
				if got != row.wantSources {
					t.Errorf("spawn %d: --setting-sources %q, want %q\nargv: %s", i, got, row.wantSources, argv)
				}
				var settings map[string]any
				if err := json.Unmarshal([]byte(rec.settings[i]), &settings); err != nil {
					t.Fatalf("spawn %d: the flag settings are not JSON: %v (%s)", i, err, rec.settings[i])
				}
				if _, inline := settings["hooks"]; inline != row.wantInline {
					t.Errorf("spawn %d: iterion's hooks inline = %v, want %v\nsettings: %s", i, inline, row.wantInline, rec.settings[i])
				}
			}
		})
	}
}
