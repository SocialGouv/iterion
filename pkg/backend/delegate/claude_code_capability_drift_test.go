package delegate

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// The disclosure's anti-drift pin (iterion#2815): it must account for EVERY
// name of the roster the delegate enforces, so a name added to or removed
// from that roster without the disclosure moving with it is red here. The
// surface is read off the same projection the argv is built from, so this
// test is what stands between the disclosure and #1651's stale-list class.
func TestTheCapabilityDriftDisclosureCoversTheRoster(t *testing.T) {
	roster := backendcaps.NativeTools(backendcaps.ClaudeCode)

	// The reasons map is a lookup keyed by the list, never a second list:
	// its key set IS headlessWithheldTools, and no reason is empty.
	if len(headlessWithheldReasons) != len(headlessWithheldTools) {
		t.Errorf("headlessWithheldReasons holds %d names, headlessWithheldTools %d — a withholding without its reason, or a reason without its withholding", len(headlessWithheldReasons), len(headlessWithheldTools))
	}
	for _, name := range headlessWithheldTools {
		if strings.TrimSpace(headlessWithheldReasons[name]) == "" {
			t.Errorf("headlessWithheldReasons[%q] is empty — the disclosure would withhold it without saying why", name)
		}
	}

	// An UNDECLARED node: every roster name is either granted or withheld —
	// accounted for, with a reason — and nothing off the roster is granted.
	s := ClaudeCodeToolSurface(Task{})
	if s.Declared {
		t.Error("an empty Task must read as an undeclared surface")
	}
	accounted := map[string]string{}
	for _, name := range s.Granted {
		accounted[name] = "granted"
	}
	for _, w := range s.Withheld {
		if w.Reason == "" {
			t.Errorf("%s is withheld with no reason", w.Name)
		}
		if w.OnRoster {
			accounted[w.Name] = "withheld"
		}
	}
	for _, name := range roster {
		if accounted[name] == "" {
			t.Errorf("the roster's %s is in neither Granted nor Withheld for an undeclared node — the disclosure does not account for it", name)
		}
	}
	for _, name := range s.Granted {
		if !slices.Contains(roster, name) {
			t.Errorf("Granted names %s, which is not on the roster — the disclosure promises a tool the spawn does not have", name)
		}
	}

	// A DECLARED-EMPTY node keeps nothing: the whole roster is withheld, the
	// headless names with their specific reason, the rest with the
	// declaration's.
	s = ClaudeCodeToolSurface(Task{AllowedTools: []string{}, ToolsDeclared: true})
	if !s.Declared {
		t.Fatal("a declared-empty list is a boundary")
	}
	if len(s.Granted) != 0 {
		t.Errorf("tools: [] keeps %v — a declared-empty list grants nothing", s.Granted)
	}
	byName := map[string]ClaudeToolWithholding{}
	for _, w := range s.Withheld {
		byName[w.Name] = w
	}
	for _, name := range roster {
		w, ok := byName[name]
		if !ok {
			t.Errorf("tools: [] does not withhold the roster's %s — the declaration boundary and the disclosure disagree", name)
			continue
		}
		if slices.Contains(headlessWithheldTools, name) {
			if !w.Always || w.Reason != headlessWithheldReasons[name] {
				t.Errorf("%s is a headless withholding but the disclosure gives it the declaration's reason (%q)", name, w.Reason)
			}
		} else if w.Always {
			t.Errorf("%s is marked Always but is no unconditional withholding — the author is told no list can keep it when the declaration could", name)
		}
	}

	// The declaration the ticket names: `agent` keeps the subagent pair, and
	// without it the pair leaves WITH the offer to declare it.
	s = ClaudeCodeToolSurface(Task{AllowedTools: []string{"agent"}, ToolsDeclared: true})
	if !slices.Contains(s.Granted, "Agent") || !slices.Contains(s.Granted, "Task") {
		t.Errorf("declaring agent keeps %v, want Task and Agent among them", s.Granted)
	}
	s = ClaudeCodeToolSurface(Task{AllowedTools: []string{"read_file"}, ToolsDeclared: true})
	for _, name := range []string{"Agent", "Task"} {
		w, ok := byNameOf(s, name)
		if !ok {
			t.Errorf("a list without agent does not withhold %s in the disclosure", name)
			continue
		}
		if !strings.Contains(w.Reason, "`agent`") {
			t.Errorf("%s's withholding reason %q does not name the declaration that keeps it", name, w.Reason)
		}
	}

	// readonly: beats the declaration, disclosed as such.
	s = ClaudeCodeToolSurface(Task{AllowedTools: []string{"write_file"}, ToolsDeclared: true, Readonly: true})
	w, ok := byNameOf(s, "Write")
	if !ok || !w.Always || !strings.Contains(w.Reason, "readonly") {
		t.Errorf("a readonly node declaring write_file: Write's withholding = %+v, want an Always withholding naming the flag", w)
	}

	// The withholding order is stable: unconditional first, then the
	// declaration's, so the printed disclosure does not reshuffle between
	// two reads of the same node.
	seenDecl := false
	for _, w := range s.Withheld {
		if !w.Always {
			seenDecl = true
		} else if seenDecl {
			t.Errorf("withholding %s (Always) is listed after a declaration withholding — the unconditional group must come first", w.Name)
		}
	}

	// The granted list follows the roster's own order.
	var wantOrder []string
	for _, r := range roster {
		if slices.Contains(s.Granted, r) {
			wantOrder = append(wantOrder, r)
		}
	}
	if !slices.Equal(s.Granted, wantOrder) {
		t.Errorf("Granted %v does not follow the roster's order %v", s.Granted, wantOrder)
	}
}

func byNameOf(s ClaudeToolSurface, name string) (ClaudeToolWithholding, bool) {
	for _, w := range s.Withheld {
		if w.Name == name {
			return w, true
		}
	}
	return ClaudeToolWithholding{}, false
}
