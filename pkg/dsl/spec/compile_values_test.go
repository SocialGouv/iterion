package spec_test

import (
	"fmt"
	"slices"
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
//
// A property of a block that lives under SEVERAL hosts is probed in
// hostCompileProbes instead, once per host.
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
	"secret": {
		"as": "secrets:\n  s:\n    as: %s\nagent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n",
	},
}

// hostCompileProbes is compileProbes for the properties of a block hosted
// by more than one kind — a `sandbox:` block on a node as well as on the
// workflow, a `fallbacks:` entry under a judge as well as an agent — with
// one template per host the registry lists, followed through a block that
// sits inside another (sandbox.network is checked wherever sandbox is).
// The compiler checks these at one choke point today; a probe under one
// host proves the check for that host, and nothing would turn red if a
// second host stopped reaching it.
var hostCompileProbes = map[string]map[string]map[string]string{
	"sandbox": {
		"mode": {
			"workflow": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: %s\n    image: \"img\"\n  a -> done\n",
			"agent":    "agent a:\n  model: \"m\"\n  sandbox:\n    mode: %s\n    image: \"img\"\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge":    "judge a:\n  model: \"m\"\n  sandbox:\n    mode: %s\n    image: \"img\"\nworkflow w:\n  entry: a\n  a -> done\n",
			"tool":     "tool a:\n  command: \"true\"\n  sandbox:\n    mode: %s\n    image: \"img\"\nworkflow w:\n  entry: a\n  a -> done\n",
		},
		"host_state": {
			"workflow": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    host_state: %s\n  a -> done\n",
			"agent":    "agent a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    host_state: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge":    "judge a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    host_state: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"tool":     "tool a:\n  command: \"true\"\n  sandbox:\n    image: \"img\"\n    host_state: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		},
	},
	"sandbox.network": {
		"mode": {
			"workflow": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    network:\n      mode: %s\n  a -> done\n",
			"agent":    "agent a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge":    "judge a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"tool":     "tool a:\n  command: \"true\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		},
		"inherit": {
			"workflow": "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    network:\n      mode: allowlist\n      inherit: %s\n  a -> done\n",
			"agent":    "agent a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: allowlist\n      inherit: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge":    "judge a:\n  model: \"m\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: allowlist\n      inherit: %s\nworkflow w:\n  entry: a\n  a -> done\n",
			"tool":     "tool a:\n  command: \"true\"\n  sandbox:\n    image: \"img\"\n    network:\n      mode: allowlist\n      inherit: %s\nworkflow w:\n  entry: a\n  a -> done\n",
		},
	},
	"fallback": {
		"on": {
			"agent": "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      backend: \"claw\"\n      model: \"anthropic/x\"\n      on: [%s]\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge": "judge a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      backend: \"claw\"\n      model: \"anthropic/x\"\n      on: [%s]\nworkflow w:\n  entry: a\n  a -> done\n",
		},
		"action": {
			"agent": "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      action: %s\n      on: [any]\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge": "judge a:\n  model: \"m\"\n  backend: \"claw\"\n  fallbacks:\n    r:\n      action: %s\n      on: [any]\nworkflow w:\n  entry: a\n  a -> done\n",
		},
	},
	"memory": {
		// A quoted value: the parser wants a string here, so the probe
		// quotes it — the compiler (C170) narrows the word inside.
		"visibility": {
			"agent": "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"%s\"\nworkflow w:\n  entry: a\n  a -> done\n",
			"judge": "judge a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"%s\"\nworkflow w:\n  entry: a\n  a -> done\n",
		},
	},
}

// notValidated are the listed values the registry documents as NOT checked
// by the compiler; the list is still rendered, so it is still compiled, but
// a bogus value drawing no error is not a finding for them. Both fail SAFE
// at run time, which is why the gap is tolerated rather than closed here: a
// posture is compared to agent_verdict_ok and anything else keeps the human
// gate; a merge_strategy that is not merge falls back to squash. The set is
// held to the registry's own wording by TestNotValidatedIsWhatTheRegistryTellsAuthors.
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

// probedProperty is the registry entry a probe stands for, or false (and a
// test error) when the probe names a property that lists no values.
func probedProperty(t *testing.T, kind, name string) (spec.Property, bool) {
	t.Helper()
	k, ok := spec.Lookup(kind)
	if !ok {
		t.Fatalf("probe kind %q is not registered", kind)
	}
	p, ok := k.Property(name)
	if !ok || len(p.Values) == 0 {
		t.Errorf("%s.%s: probed but the registry lists no values for it", kind, name)
		return spec.Property{}, false
	}
	return p, true
}

// probeValues runs one template through the two assertions: every listed
// value compiles without an error, and a value outside the list is refused
// by name. `where` names the host for the message when the probe is one of
// several.
func probeValues(t *testing.T, kind, name, where, tmpl string, p spec.Property) {
	t.Helper()
	for _, v := range p.Values {
		if errs, _ := compileDiags(t, fmt.Sprintf(tmpl, v), v, false, false); len(errs) != 0 {
			t.Errorf("%s.%s = %s%s is listed but the compiler refuses it: %v", kind, name, v, where, errs)
		}
	}
	if notValidated[kind+"."+name] {
		return
	}
	if _, refusing := compileDiags(t, fmt.Sprintf(tmpl, "zz_bogus"), "zz_bogus", warningOnly[kind+"."+name], true); len(refusing) == 0 {
		t.Errorf("%s.%s%s: a value outside the list is not refused — the list is not checked, the check is only a warning, or the probe misses it", kind, name, where)
	}
}

// TestEveryCompileCheckedValueCompiles holds the registry's value lists to
// the COMPILER: each listed value compiles without an error in a minimal
// workflow, and a value outside the list is reported by name — so the test
// proves the check exists, not merely that the list is a subset of what
// compiles. A multi-host block's probes run under each of its hosts.
func TestEveryCompileCheckedValueCompiles(t *testing.T) {
	for kind, byProp := range compileProbes {
		for name, tmpl := range byProp {
			if p, ok := probedProperty(t, kind, name); ok {
				probeValues(t, kind, name, "", tmpl, p)
			}
		}
	}
	for kind, byProp := range hostCompileProbes {
		for name, byHost := range byProp {
			p, ok := probedProperty(t, kind, name)
			if !ok {
				continue
			}
			for host, tmpl := range byHost {
				probeValues(t, kind, name, " under "+host, tmpl, p)
			}
		}
	}
}

// effectiveHosts is where a block's properties are compiled: the hosts the
// registry lists, followed through a block hosted by exactly one other
// block (sandbox.network sits under sandbox, which sits under four kinds)
// — a check at the parent's choke point has to be proven at each of the
// parent's hosts.
func effectiveHosts(k spec.Kind) []string {
	hosts := k.Hosts
	seen := map[string]bool{k.Name: true}
	for len(hosts) == 1 && !seen[hosts[0]] {
		seen[hosts[0]] = true
		parent, ok := spec.Lookup(hosts[0])
		if !ok || len(parent.Hosts) == 0 {
			break
		}
		hosts = parent.Hosts
	}
	return hosts
}

// TestEveryCompileProbeOfAMultiHostBlockRunsUnderEachHost: a compile-checked
// property of a block hosted by several kinds is probed under every one of
// them — in hostCompileProbes, never in compileProbes, where one template
// would prove one host and read as proving all — and under nothing else.
func TestEveryCompileProbeOfAMultiHostBlockRunsUnderEachHost(t *testing.T) {
	for kind, byProp := range compileProbes {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Fatalf("probe kind %q is not registered", kind)
		}
		if hosts := effectiveHosts(k); len(hosts) > 1 {
			for name := range byProp {
				t.Errorf("%s.%s: the block lives under %s, so its probe belongs in hostCompileProbes, one template per host", kind, name, strings.Join(hosts, "/"))
			}
		}
	}
	for kind, byProp := range hostCompileProbes {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Fatalf("probe kind %q is not registered", kind)
		}
		hosts := effectiveHosts(k)
		if len(hosts) < 2 {
			t.Errorf("%s: probed per host but the registry gives it one (%s)", kind, strings.Join(hosts, "/"))
		}
		for name, byHost := range byProp {
			for _, h := range hosts {
				if _, ok := byHost[h]; !ok {
					t.Errorf("%s.%s: no compile probe under host %q", kind, name, h)
				}
			}
			for h, tmpl := range byHost {
				if !slices.Contains(hosts, h) {
					t.Errorf("%s.%s: probed under %q, which is not a host of the block (%s)", kind, name, h, strings.Join(hosts, "/"))
				}
				if got := enclosingDeclaration(tmpl, k.Opener); got != h {
					t.Errorf("%s.%s: the probe filed under %q puts `%s:` in a %q declaration", kind, name, h, k.Opener, got)
				}
			}
		}
	}
}

// enclosingDeclaration is the kind of the top-level declaration the block
// opened by `<opener>:` sits in, in a probe template — which a template
// filed under a host has to be, or the per-host table is N copies of one
// probe that the key check alone would pass.
func enclosingDeclaration(tmpl, opener string) string {
	lines := strings.Split(tmpl, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != opener+":" {
			continue
		}
		for j := i; j >= 0; j-- {
			if lines[j] != "" && !strings.HasPrefix(lines[j], " ") {
				return strings.Fields(lines[j])[0]
			}
		}
	}
	return ""
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
			if _, ok := compileProbes[k.Name][p.Name]; ok {
				continue
			}
			if _, ok := hostCompileProbes[k.Name][p.Name]; ok {
				continue
			}
			t.Errorf("%s.%s lists values (%s) but has no compile probe", k.Name, p.Name, strings.Join(p.Values, ", "))
		}
	}
}

// TestNotValidatedIsWhatTheRegistryTellsAuthors: the probe's exemptions and
// the registry's rendered doc name the SAME properties, in both directions.
// A value check cannot be silenced here without the reference, the skill
// and the E012 remedy telling authors the value is not validated; and a
// property the docs call unvalidated cannot be one the compiler does check.
func TestNotValidatedIsWhatTheRegistryTellsAuthors(t *testing.T) {
	documented := map[string]bool{}
	for _, k := range spec.Kinds {
		for _, p := range k.Properties {
			if len(p.Values) > 0 && strings.Contains(p.Doc, "not validated") {
				documented[k.Name+"."+p.Name] = true
			}
		}
	}
	for key := range notValidated {
		if !documented[key] {
			t.Errorf("%s is exempt from the compile check but its registry doc does not say \"not validated\"", key)
		}
	}
	for key := range documented {
		if !notValidated[key] {
			t.Errorf("%s is documented as not validated but the compile probe holds it to the compiler", key)
		}
	}
}
