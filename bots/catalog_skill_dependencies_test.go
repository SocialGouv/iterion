package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// shippedSkillExport is one `exports.skills` entry as the TREE holds it: the
// id a consumer resolves it by, where the producer keeps the bytes, and the
// `name:` the document itself answers to.
type shippedSkillExport struct {
	id              string
	path            string
	frontmatterName string
}

// catalogSkillExports reads every shipped bundle's `exports.skills`, keyed by
// the manifest NAME a dependency's `from` spells — the same key the install
// matches on, exactly (it refuses a producer directory whose manifest name
// disagrees).
func catalogSkillExports(t *testing.T) map[string]map[string]shippedSkillExport {
	t.Helper()
	manifests, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no manifests found under bots/*/ — the discovery glob broke")
	}
	exports := map[string]map[string]shippedSkillExport{}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Fatalf("%s: load: %v", path, err)
		}
		if m == nil || m.Name == "" {
			continue
		}
		for _, exp := range m.Exports.Skills {
			doc := filepath.Join(filepath.Dir(path), filepath.FromSlash(exp.Path))
			entry := shippedSkillExport{id: exp.ID, path: exp.Path}
			if body, readErr := os.ReadFile(doc); readErr == nil {
				if match := skillFrontmatterNameRe.FindSubmatch(body); match != nil {
					entry.frontmatterName = string(match[1])
				}
			} else {
				entry.path = "" // absent: the resolution's own refusal
			}
			if exports[m.Name] == nil {
				exports[m.Name] = map[string]shippedSkillExport{}
			}
			exports[m.Name][exp.ID] = entry
		}
	}
	return exports
}

// refuseUnresolvableSkillDependencies is the catalogue's half of ADR-144
// clause 5, as a predicate both the tree walk and its own fixtures call —
// a guard that only ever sees the tree proves nothing while no bundle has
// adopted a dependency yet.
//
// A declared dependency must resolve HERE, in the repository, under the name
// the consumer references it by, and it must not take over a name the
// OPERATOR attaches: that exception ("a bot may reference a skill it does
// NOT ship, so the operator supplies it") carries its reason per entry, and
// a dependency claiming the name would turn a deliberate choice into an
// install-time pin to one platform.
func refuseUnresolvableSkillDependencies(consumer string, deps []bundle.SkillDependency, shipped map[string]bool, exports map[string]map[string]shippedSkillExport, attached map[string]string) []string {
	var findings []string
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, dep.Name)
	}
	sort.Strings(names)
	byName := make(map[string]bundle.SkillDependency, len(deps))
	for _, dep := range deps {
		byName[dep.Name] = dep
	}
	for _, name := range names {
		dep := byName[name]
		if reason, isAttached := attached[dep.Name]; isAttached {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q, which the operator attaches (%s): a catalog bot that depends on it stops being repo-agnostic", consumer, dep.Name, reason))
			continue
		}
		if shipped[dep.Name] {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q AND ships skills/%s.md: the install refuses that shadow, so one of the two has to go", consumer, dep.Name, dep.Name))
			continue
		}
		producer, known := exports[dep.From]
		if !known {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, which exports no skill in this catalogue: the install would find nothing to resolve", consumer, dep.Name, dep.From))
			continue
		}
		exported, ok := producer[dep.ExportedID()]
		if !ok {
			ids := make([]string, 0, len(producer))
			for id := range producer {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, which does not export %q (it exports: %s)", consumer, dep.Name, dep.From, dep.ExportedID(), strings.Join(ids, ", ")))
			continue
		}
		if exported.path == "" {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, whose export %q names a document that is not in the tree", consumer, dep.Name, dep.From, dep.ExportedID()))
			continue
		}
		if exported.frontmatterName != "" && exported.frontmatterName != dep.Name {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, but %s carries `name: %s`: the materialized document would answer to another name than the one `skills: [%s]` spells", consumer, dep.Name, dep.From, exported.path, exported.frontmatterName, dep.Name))
		}
	}
	return findings
}

// TestCatalogSkillDependenciesResolve runs the predicate over the shipped
// catalogue: a declaration that cannot resolve is a bot that installs one
// document short and answers from the model's priors instead.
func TestCatalogSkillDependenciesResolve(t *testing.T) {
	exports := catalogSkillExports(t)
	bundles, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range bundles {
		dir := filepath.Dir(path)
		deps := declaredSkillDependencies(t, dir)
		if len(deps) == 0 {
			continue
		}
		shipped := map[string]bool{}
		if docs, globErr := filepath.Glob(filepath.Join(dir, "skills", "*.md")); globErr == nil {
			for _, doc := range docs {
				shipped[strings.TrimSuffix(filepath.Base(doc), ".md")] = true
			}
		}
		list := make([]bundle.SkillDependency, 0, len(deps))
		for _, dep := range deps {
			list = append(list, dep)
		}
		for _, finding := range refuseUnresolvableSkillDependencies(dir, list, shipped, exports, operatorAttachedSkills) {
			t.Error(finding)
		}
	}
}

// TestTheSkillDependencyGateBites is the guard of the guard: every refusal
// above is exercised against a declaration built here, so the gate cannot
// pass by having nothing to judge — which is exactly its state in the tree
// until the first bundle adopts a dependency.
func TestTheSkillDependencyGateBites(t *testing.T) {
	exports := map[string]map[string]shippedSkillExport{
		"app-dev": {
			"verify-build": {id: "verify-build", path: "app-dev/skills/verify-build.md", frontmatterName: "verify-build"},
			"gone":         {id: "gone"},
			"renamed":      {id: "renamed", path: "app-dev/skills/renamed.md", frontmatterName: "something-else"},
		},
	}
	for name, tc := range map[string]struct {
		dep     bundle.SkillDependency
		shipped map[string]bool
		want    string
	}{
		"resolves": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "verify-build"},
			want: "",
		},
		"an operator-attached name": {
			dep:  bundle.SkillDependency{Name: "deploy-target", From: "app-dev", ID: "verify-build"},
			want: "which the operator attaches",
		},
		"shadowing a shipped document": {
			dep:     bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "verify-build"},
			shipped: map[string]bool{"verify-build": true},
			want:    "the install refuses that shadow",
		},
		"a producer outside the catalogue": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "nowhere", ID: "verify-build"},
			want: `from "nowhere", which exports no skill in this catalogue`,
		},
		"an id the producer does not export": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "absent"},
			want: `does not export "absent" (it exports: gone, renamed, verify-build)`,
		},
		"an export whose document is missing": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "gone"},
			want: "names a document that is not in the tree",
		},
		"a document that answers to another name": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "renamed"},
			want: "carries `name: something-else`",
		},
	} {
		t.Run(name, func(t *testing.T) {
			findings := refuseUnresolvableSkillDependencies("a-bot", []bundle.SkillDependency{tc.dep}, tc.shipped, exports, operatorAttachedSkills)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("a resolvable declaration was refused: %v", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %v, want exactly one naming %q", findings, tc.want)
			}
			if !strings.Contains(findings[0], tc.want) {
				t.Fatalf("finding = %q, want it to name %q", findings[0], tc.want)
			}
		})
	}
}
