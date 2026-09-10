package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// compileProbes is, for every property whose listed values are the
// COMPILER's business (the parser takes any word), a minimal VALID workflow
// with `%s` where the value goes. The listed values are what the reference,
// the skill and the E012 remedy show authors as accepted, and the parser's
// probe cannot see them: only compiling each one proves the list.
var compileProbes = map[string]map[string]string{
	"workflow": {
		"sandbox":              "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox: %s\n  a -> done\n",
		"compress":             "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  compress: %s\n  a -> done\n",
		"auto_memory":          "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  auto_memory: %s\n  a -> done\n",
		"permission":           "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  permission: %s\n  a -> done\n",
		"loop_budget_guard":    "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  loop_budget_guard: %s\n  a -> done\n",
		"repo_devbox":          "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  repo_devbox: %s\n  a -> done\n",
		"workspace_checkpoint": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  workspace_checkpoint: %s\n  a -> done\n",
		"worktree":             "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  worktree: %s\n  a -> done\n",
	},
	"agent": {
		"sandbox":     "agent a:\n  model: \"m\"\n  sandbox: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"compress":    "agent a:\n  model: \"m\"\n  compress: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"auto_memory": "agent a:\n  model: \"m\"\n  auto_memory: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"permission":  "agent a:\n  model: \"m\"\n  permission: %s\nworkflow w:\n  entry: a\n  a -> done\n",
	},
	"judge": {
		"sandbox":     "judge a:\n  model: \"m\"\n  sandbox: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"compress":    "judge a:\n  model: \"m\"\n  compress: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"auto_memory": "judge a:\n  model: \"m\"\n  auto_memory: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"permission":  "judge a:\n  model: \"m\"\n  permission: %s\nworkflow w:\n  entry: a\n  a -> done\n",
	},
	"tool": {
		"sandbox":  "tool a:\n  command: \"true\"\n  sandbox: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"compress": "tool a:\n  command: \"true\"\n  compress: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"language": "tool a:\n  script: \"true\"\n  language: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		"policy":   "tool a:\n  command: \"touch x\"\n  goal: \"g\"\n  postcondition: \"test -f x\"\n  policy: %s\nworkflow w:\n  entry: a\n  a -> done\n",
	},
	"sandbox": {
		"mode":       "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: %s\n    image: \"img\"\n  a -> done\n",
		"host_state": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    host_state: %s\n  a -> done\n",
	},
	"sandbox.network": {
		"mode":    "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    network:\n      mode: %s\n  a -> done\n",
		"inherit": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    network:\n      mode: allowlist\n      inherit: %s\n  a -> done\n",
	},
	"secret": {
		"as": "secrets:\n  s:\n    as: %s\nagent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n",
	},
	"fallback": {
		"on":     "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      backend: \"claw\"\n      model: \"anthropic/x\"\n      on: [%s]\nworkflow w:\n  entry: a\n  a -> done\n",
		"action": "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      action: %s\n      on: [any]\nworkflow w:\n  entry: a\n  a -> done\n",
	},
}

// notValidated are the listed values the registry documents as NOT checked
// by the compiler; the list is still rendered, so it is still compiled, but
// a bogus value drawing no error is not a finding for them. Both fail SAFE
// at run time, which is why the gap is tolerated rather than closed here: a
// posture is compared to agent_verdict_ok and anything else keeps the human
// gate; a merge_strategy that is not merge falls back to squash.
var notValidated = map[string]bool{"human.posture": true, "human.merge_strategy": true}

// warningOnly are the checks that report a bogus value as a WARNING by
// design (the value still flows into the IR): a fallback trigger the engine
// does not know is left to the run, which classifies failures at run time.
var warningOnly = map[string]bool{"fallback.on": true}

// compileDiags compiles a probe and returns its errors, and separately the
// diagnostics that REFUSE the probed value: errors naming it, or warnings
// naming it for the checks listed in warningOnly. A listed value must draw
// no error; a bogus one must be refused — by the compiler, or already by
// the parser (a property that turns out to be the parser's is refused
// earlier, which is a refusal, not a broken probe).
func compileDiags(t *testing.T, doc, value string, warnCounts, bogus bool) (errs, refusing []string) {
	t.Helper()
	pr := parser.Parse("probe.bot", doc)
	if len(pr.Diagnostics) != 0 {
		if !bogus {
			t.Fatalf("probe does not parse: %q: %v", doc, pr.Diagnostics)
		}
		for _, d := range pr.Diagnostics {
			if strings.Contains(d.Message, value) {
				refusing = append(refusing, string(d.Code)+": "+d.Message)
			}
		}
		return nil, refusing
	}
	for _, d := range ir.Compile(pr.File).Diagnostics {
		line := string(d.Code) + ": " + d.Message
		if d.Severity == ir.SeverityError {
			errs = append(errs, line)
		}
		if strings.Contains(d.Message, value) && (d.Severity == ir.SeverityError || warnCounts) {
			refusing = append(refusing, line)
		}
	}
	return errs, refusing
}

// TestEveryCompileCheckedValueCompiles holds the registry's value lists to
// the COMPILER: each listed value compiles without an error in a minimal
// workflow, and a value outside the list is reported by name — so the test
// proves the check exists, not merely that the list is a subset of what
// compiles.
func TestEveryCompileCheckedValueCompiles(t *testing.T) {
	for kind, byProp := range compileProbes {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Fatalf("probe kind %q is not registered", kind)
		}
		for name, tmpl := range byProp {
			p, ok := k.Property(name)
			if !ok || len(p.Values) == 0 {
				t.Errorf("%s.%s: probed but the registry lists no values for it", kind, name)
				continue
			}
			for _, v := range p.Values {
				if errs, _ := compileDiags(t, fmt.Sprintf(tmpl, v), v, false, false); len(errs) != 0 {
					t.Errorf("%s.%s = %s is listed but the compiler refuses it: %v", kind, name, v, errs)
				}
			}
			if notValidated[kind+"."+name] {
				continue
			}
			if _, refusing := compileDiags(t, fmt.Sprintf(tmpl, "zz_bogus"), "zz_bogus", warningOnly[kind+"."+name], true); len(refusing) == 0 {
				t.Errorf("%s.%s: a value outside the list is not refused — the list is not checked, the check is only a warning, or the probe misses it", kind, name)
			}
		}
	}
}

// TestEveryListedValueOfACheckedPropertyHasACompileProbe: a `checked`
// property (a bare word the compiler narrows) added to the registry without
// a compile probe would carry a value list nothing holds to the compiler.
// An Enum is the parser's business — the form probe proves its values are
// accepted and a bogus one refused, in TestEveryListedPropertyParsesCleanWithItsForm.
func TestEveryListedValueOfACheckedPropertyHasACompileProbe(t *testing.T) {
	for _, k := range spec.Kinds {
		for _, p := range k.Properties {
			if len(p.Values) == 0 || p.Form == spec.Enum {
				continue
			}
			if notValidated[k.Name+"."+p.Name] {
				continue
			}
			if _, ok := compileProbes[k.Name][p.Name]; !ok {
				t.Errorf("%s.%s lists values (%s) but has no compile probe", k.Name, p.Name, strings.Join(p.Values, ", "))
			}
		}
	}
}
