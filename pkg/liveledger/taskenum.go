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
	// Name is the Taskfile target, e.g. "test:live:bot:review-pr" — the
	// ledger's unique key; each target has AT MOST ONE row.
	Name string
	// RunPattern is the Go test -run pattern the target invokes (e.g.
	// "TestLive_Bot_ReviewPR$"). Empty when the target is an aggregate
	// or a delegating target.
	RunPattern string
	// Records reports whether a run of this target can ever write a
	// ledger row: some cmd is a `go test` invocation with `-tags live`
	// whose -run pattern is absent (wildcard) or names TestLive_ tests.
	//
	// The complements, both observed in the real Taskfile:
	//   - delegating targets (`cmds: - task: …`) re-resolve {{.TASK}}
	//     per sub-task, so the parent's name never reaches a Track call
	//     and the parent row is unwritable;
	//   - non-test targets (status, compile-only `^$`, the quality
	//     engine's unit tests) run nothing the harness tracks.
	//
	// A row that no run can ever write stays `never` forever, and a
	// `never` that cannot change is noise on top of the exact signal
	// `never = a paid target that has not run`.
	Records bool
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
			Records:    targetRecords(task.Cmds),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runPatternRE matches `-run PATTERN`, `-run 'PATTERN'`, `-run "PATTERN"`
// or `-run=PATTERN` in a Task cmd string. Anchored on the flag itself so
// a substring in a comment or description does not false-positive. The
// unquoted and `=` forms are first-class: half the real Taskfile's live
// targets write the pattern unquoted, and a parser that only saw the
// quoted ones would silently drop those targets from every enumeration
// built on it.
var runPatternRE = regexp.MustCompile(`-run[=\s]+['"]?([^'"\s]+)['"]?`)

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

// targetRecords reports whether any cmd of the target runs a live test
// the harness tracks: a `go test` invocation with `-tags live` whose
// -run pattern is absent (wildcard over the package) or names TestLive_
// tests. Delegating targets carry `task:` mapping cmds, which are
// neither, so they classify as non-recording.
func targetRecords(cmds []yaml.Node) bool {
	for _, n := range cmds {
		var s string
		switch n.Kind {
		case yaml.ScalarNode:
			s = n.Value
		case yaml.MappingNode:
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
		pattern, ok := liveGoTestCmd(s)
		if !ok {
			continue
		}
		if pattern == "" || strings.Contains(pattern, "TestLive") {
			return true
		}
	}
	return false
}

// tagsRE captures the -tags value: bare, quoted (a multi-tag value like
// `-tags "live integration"`), or after `=`.
var tagsRE = regexp.MustCompile(`-tags[=\s]+("[^"]*"|'[^']*'|[^\s]+)`)

// liveGoTestCmd reports whether s is a `go test` invocation whose -tags
// list includes `live`, and returns the -run pattern it carries (""
// when none — a wildcard over the whole package).
func liveGoTestCmd(s string) (string, bool) {
	if !strings.Contains(s, "go test") {
		return "", false
	}
	m := tagsRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	live := false
	for _, tg := range strings.FieldsFunc(strings.Trim(m[1], "\"'"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if tg == "live" {
			live = true
		}
	}
	if !live {
		return "", false
	}
	if m := runPatternRE.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", true
}
