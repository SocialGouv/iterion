package liveledger

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TaskfileRelPath is the ledger's assumed location for the project
// Taskfile — a caller can pass any other path to EnumerateLiveTargets
// (tests do, to drive the parser off a fixture).
const TaskfileRelPath = "Taskfile.yml"

// LiveTargetPrefix filters the ledger to the subset it exists for.
const LiveTargetPrefix = "test:live"

// LiveTarget is a Taskfile task and the -run pattern that names its
// test function, extracted from the target's `cmds` entries.
type LiveTarget struct {
	// Name is the Taskfile target, e.g. "test:live:bot:review-pr".
	Name string
	// RunPattern is the Go test -run pattern the target invokes (e.g.
	// "TestLive_Bot_ReviewPR$"). Empty when the target is an aggregate
	// that runs a wildcard like "TestLive_", or when the target does
	// not invoke `go test` at all.
	RunPattern string
}

// EnumerateLiveTargets reads a Taskfile.yml (Task v3) and returns every
// `test:live*` target it defines, sorted by name. The Taskfile is the
// single source of truth for the ledger's row set — a hand-maintained
// list would drift, and the class enumeration this exists for is
// exactly "every target the Taskfile declares".
func EnumerateLiveTargets(taskfilePath string) ([]LiveTarget, error) {
	b, err := os.ReadFile(taskfilePath)
	if err != nil {
		return nil, fmt.Errorf("liveledger: read %s: %w", taskfilePath, err)
	}
	var doc struct {
		Tasks map[string]struct {
			Cmds []yaml.Node `yaml:"cmds"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("liveledger: parse %s: %w", taskfilePath, err)
	}
	var out []LiveTarget
	for name, task := range doc.Tasks {
		if !strings.HasPrefix(name, LiveTargetPrefix) {
			continue
		}
		out = append(out, LiveTarget{
			Name:       name,
			RunPattern: extractRunPattern(task.Cmds),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runPatternRE matches `-run 'PATTERN'` or `-run "PATTERN"` in a Task
// cmd string. Anchored on the flag itself so a substring in a comment
// or description does not false-positive.
var runPatternRE = regexp.MustCompile(`-run\s+['"]([^'"]+)['"]`)

// extractRunPattern walks a target's cmds and returns the first `-run`
// pattern it finds. Cmds is a []yaml.Node because Task allows both
// scalar strings ("go test …") and mapping entries ({ cmd: "…" }); we
// look for the first string that carries the `-run` flag either way.
func extractRunPattern(cmds []yaml.Node) string {
	for _, n := range cmds {
		var s string
		switch n.Kind {
		case yaml.ScalarNode:
			s = n.Value
		case yaml.MappingNode:
			// A mapping cmd typically has a `cmd` key; scan its scalar
			// children for the -run pattern.
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				v := n.Content[i+1]
				if k.Kind == yaml.ScalarNode && k.Value == "cmd" && v.Kind == yaml.ScalarNode {
					s = v.Value
				}
			}
		}
		if s == "" {
			continue
		}
		if m := runPatternRE.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}

// TargetNames extracts just the names from EnumerateLiveTargets, for
// callers that only need the class enumeration (the ledger's
// EnsureNeverRows is one).
func TargetNames(targets []LiveTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Name)
	}
	return out
}
