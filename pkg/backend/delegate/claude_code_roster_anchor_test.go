package delegate

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
)

// notNativeRoster names every tool iterion renders a header for that is NOT
// part of the claude_code native roster, with the reason. It is the escape
// hatch of the guard below: a name belongs here or in the roster, never in
// neither, so "the roster forgot it" and "the roster excludes it on purpose"
// cannot be confused — which is exactly how claudeNativeTools went stale.
//
// Empty today: every CamelCase name the display table knows is a tool the
// pinned CLI registers, so every one of them is on the roster. A name added
// here needs its sentence, because the consequence of being off the roster is
// that `--disallowedTools` never mentions it and a node's `tools:` list
// cannot remove it.
var notNativeRoster = map[string]string{}

// The anchoring guard. `--disallowedTools` is derived from the roster, so on a
// node that declares a restricted `tools:` list and has no permission gate the
// roster is the ONLY bound: a name it forgets is a tool the author did not
// grant and cannot remove. The roster is therefore not allowed to be a
// hand-kept list that drifts from the one the CLI actually offers — and
// iterion already holds a second, independently maintained enumeration of
// that surface, the display table it renders tool headers from. They must
// agree.
//
// Measured before #1651 fixed it: the roster had 14 names, the display table
// 19, and the five-way gap was Agent, AskUserQuestion, BashOutput, KillShell
// and SlashCommand (ScheduleWakeup was already withheld by
// headlessWithheldTools but absent from the roster). This test was RED on
// exactly those names.
func TestEveryDisplayedToolIsOnTheRosterOrExcludedOnPurpose(t *testing.T) {
	roster := backendcaps.NativeTools(backendcaps.ClaudeCode)
	var missing []string
	for name := range tooldisplay.CamelCaseKeys {
		if slices.Contains(roster, name) {
			if reason, excluded := notNativeRoster[name]; excluded {
				t.Errorf("%s is BOTH on the roster and excluded from it (%q) — one of the two is wrong", name, reason)
			}
			continue
		}
		if _, excluded := notNativeRoster[name]; excluded {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("iterion renders a tool header for %d name(s) the claude_code roster does not carry: %s.\n"+
			"Every one is a tool a node's `tools:` list cannot remove, because --disallowedTools is derived from the roster — "+
			"and on the 84 shipped claude_code nodes that declare a restricted list with no permission gate, that list is the only bound. "+
			"Add each name to the registry's native_tools (with its companion row in claudeNativeForCanonical, or headlessWithheldTools "+
			"when no node should ever keep it), or to notNativeRoster with the reason it is not native.",
			len(missing), strings.Join(missing, ", "))
	}
}

// A companion row must grant names the roster actually carries: a projection
// onto a name that is not on the roster grants nothing and reads as if it did.
func TestEveryCompanionGrantIsOnTheRoster(t *testing.T) {
	roster := backendcaps.NativeTools(backendcaps.ClaudeCode)
	for canonical, granted := range claudeNativeForCanonical {
		for _, name := range granted {
			if !slices.Contains(roster, name) {
				t.Errorf("declaring %q grants %q, which is not on the roster — the grant is silently empty", canonical, name)
			}
		}
	}
}

// The two one-to-many rows are one-to-many on purpose, and the reason is
// load-bearing: a node that declares the capability must keep every spelling
// of it, or it declared something it did not get. Pinned by name so that
// dropping a companion is a visible verdict change, not a quiet narrowing.
//
// `agent` is the row #1651 added: `Agent` is what the current CLI calls the
// subagent tool `Task` names on older ones, so a node that declared the
// subagent and kept only the dead spelling declared nothing.
func TestDeclaringACapabilityKeepsEverySpellingOfIt(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     []string
	}{
		{"bash", []string{"Bash"}},
		{"edit", []string{"Edit", "MultiEdit"}},
		{"agent", []string{"Task", "Agent"}},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			native := claudeNativeToolsForAllowed([]string{tc.declared}, false)
			for _, name := range tc.want {
				if !native[name] {
					t.Errorf("a node declaring %q does not keep %q — it declared a capability and got part of it",
						tc.declared, name)
				}
			}
			if len(native) != len(tc.want) {
				var got []string
				for n := range native {
					got = append(got, n)
				}
				sort.Strings(got)
				t.Errorf("declaring %q keeps %v, want exactly %v", tc.declared, got, tc.want)
			}
		})
	}
}

// The names no node may keep are withheld from EVERY spawn, so a `tools:`
// list cannot grant them back by naming the capability they ride on. This is
// the half that makes adding them to the roster a bound rather than a grant:
// AskUserQuestion and SlashCommand are on the roster so that
// `--disallowedTools` can name them, and in headlessWithheldTools so that it
// always does.
func TestTheNamesNoHeadlessNodeMayKeepAreAlwaysWithheld(t *testing.T) {
	roster := backendcaps.NativeTools(backendcaps.ClaudeCode)
	for _, name := range []string{"AskUserQuestion", "SlashCommand", "ScheduleWakeup", "BashOutput", "KillShell"} {
		if !slices.Contains(roster, name) {
			t.Errorf("%s is withheld but absent from the roster — --disallowedTools is derived from the roster, so the withholding would name nothing", name)
		}
		if !slices.Contains(headlessWithheldTools, name) {
			t.Errorf("%s is on the roster but not withheld — a node that declares no tools would keep it", name)
		}
		// And no declaration may grant it back.
		for canonical, granted := range claudeNativeForCanonical {
			if slices.Contains(granted, name) {
				t.Errorf("declaring %q grants %q, which every spawn withholds — the two rules contradict each other", canonical, name)
			}
		}
	}
	// Withholding holds for an ultracode node too: the predicate that relaxes
	// the orchestration surface must not relax these.
	for _, task := range []Task{{}, {Ultracode: true}, {AllowedTools: []string{"bash", "agent"}}} {
		for _, name := range []string{"AskUserQuestion", "SlashCommand", "BashOutput", "KillShell"} {
			if claudeSpawnKeeps(task, name) {
				t.Errorf("task %+v keeps %s", task, name)
			}
		}
	}
}
