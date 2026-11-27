package ciguard

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// beyondTaskCheck names every member of a required or staged gate-map row
// that `task check` deliberately does not run, with why. Each entry is a
// claim about two files: TestTaskCheckRunsEveryRequiredMemberOrSaysWhy fails
// when a member joins the `check` closure (the entry is stale) or leaves the
// reached gate rows (the entry is dead).
var beyondTaskCheck = map[string]string{
	// AGENTS.md names `race` among the checks CI runs beyond `task check`:
	// minutes of runtime, its flakes are triaged against the CI runner.
	"test:race": "CI-only by AGENTS.md: minutes of runtime, and its shards and timeout are CI's",
	// ci:vendor-check REWRITES go.mod, go.sum and vendor/: a gate that
	// mutates the tree it judges would punish whoever shares the worktree.
	"ci:vendor-check": "rewrites go.mod, go.sum and vendor/ — `check` must not mutate the tree it judges",
	// `check` runs `lint`, whose `fmt` dependency runs `go fmt ./...` in
	// place: after it, ci:gofmt reports what lint already rewrote.
	"ci:gofmt": "redundant after `check`: `lint` runs `fmt`, which rewrites whatever ci:gofmt would report",
	// `check` runs `lint`, whose command is the same golangci-lint analysis
	// (the pinned config; its 5m timeout comes from .golangci.yml where
	// ci:golangci restates the job's --timeout 15m verbatim).
	"ci:golangci": "redundant: `check` runs `lint`, the same golangci-lint analysis with the same config",
	// `test` is `go test ./...`, and ./e2e is inside it.
	"test:e2e": "redundant: `test` is `go test ./...`, which includes ./e2e",
	// Declared gap: the -tags=live build gate is not a `check` member yet.
	"test:live:compile": "declared gap: not run by `task check`, its cost has not been measured",
}

// opaqueCallKeys are the cmds/deps keys a static walk cannot judge: go-task
// ignores `platforms` on a CALL (the call runs anyway — dead config, refused
// because the author probably intended a gate that does not exist), while
// `defer`/`defer_task`/`for` change when or how often the call executes. The
// operative platform/status gate lives on the task DEFINITION, which the
// closure walk refuses.
var opaqueCallKeys = map[string]bool{"platforms": true, "defer": true, "defer_task": true, "for": true}

// taskCallName reads the task a cmds/deps entry invokes. In `cmds`, a scalar
// is a shell line and only a mapping carrying `task` is a call; in `deps`, a
// scalar IS the task name. A mapping without `task` (a dep object, a shell
// line with fields) calls nothing. A mapping carrying `task` alongside an
// opaqueCallKeys key (`platforms`, `defer`, `defer_task`, `for`) fails the
// guard: the closure must not silently count a call whose execution it
// cannot certify.
func taskCallName(t *testing.T, name string, node yaml.Node, scalarIsTask bool) (string, bool) {
	if node.Kind == yaml.ScalarNode {
		if scalarIsTask {
			return node.Value, true
		}
		return "", false
	}
	if node.Kind == yaml.MappingNode {
		var called string
		var opaque []string
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			switch {
			case key == "task" && value.Kind == yaml.ScalarNode:
				called = value.Value
			case opaqueCallKeys[key]:
				opaque = append(opaque, key)
			}
		}
		if called != "" && len(opaque) > 0 {
			t.Fatalf("task %q has a cmds/deps entry calling %q with %v — %v does not gate a task call (go-task ignores it and the call runs), so this intended gate does not exist: declare it without the dead key or drop it", name, called, opaque, opaque)
		}
		if called != "" {
			return called, true
		}
	}
	return "", false
}

// taskClosure walks the Taskfile from root over cmds task calls and deps,
// breadth-first, and returns every task in the closure, root included. It
// fails when root is not defined or when a referenced task is not: a closure
// computed over a dangling call would silently under-report.
func taskClosure(t *testing.T, root string) map[string]bool {
	t.Helper()
	var tf struct {
		Tasks map[string]struct {
			Cmds      []yaml.Node `yaml:"cmds"`
			Deps      []yaml.Node `yaml:"deps"`
			Platforms []string    `yaml:"platforms"`
			Status    []string    `yaml:"status"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &tf); err != nil {
		t.Fatalf("parse %s: %v", taskfilePath, err)
	}
	if _, ok := tf.Tasks[root]; !ok {
		t.Fatalf("%s defines no task %q — the closure walk would report an empty set", taskfilePath, root)
	}
	closure := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		entry := tf.Tasks[name]
		if len(entry.Platforms) > 0 || len(entry.Status) > 0 {
			kind, gated := "platforms", entry.Platforms
			if len(gated) == 0 {
				kind, gated = "status", entry.Status
			}
			t.Fatalf("task %q, inside the %q closure, is gated (%s: %v) — a gated task does not run on every machine owing `task check`", name, root, kind, gated)
		}
		for _, node := range entry.Deps {
			if called, ok := taskCallName(t, name, node, true); ok {
				if _, defined := tf.Tasks[called]; !defined {
					t.Fatalf("task %q depends on %q, which %s does not define", name, called, taskfilePath)
				}
				if !closure[called] {
					closure[called] = true
					queue = append(queue, called)
				}
			}
		}
		for _, node := range entry.Cmds {
			if called, ok := taskCallName(t, name, node, false); ok {
				if _, defined := tf.Tasks[called]; !defined {
					t.Fatalf("task %q calls %q, which %s does not define", name, called, taskfilePath)
				}
				if !closure[called] {
					closure[called] = true
					queue = append(queue, called)
				}
			}
		}
	}
	return closure
}

// agentsMdPath is the one instruction file every session loads, where the
// `task check` bullet enumerates what the gate runs.
const agentsMdPath = "../../AGENTS.md"

// agentsMdGlosses maps `check` members whose human gloss lives in AGENTS.md's
// `task check` bullet to the words that gloss uses. The bullet is a member
// enumeration like docs/development.md's: a member that joins `check` without
// its gloss there makes every session believe the gate judges what it skips —
// exactly how fmt:check stayed invisible.
var agentsMdGlosses = map[string]string{
	"fmt:check": "bot format",
}

// TestTheAgentsMdGlossNamesEveryGlossedMember holds AGENTS.md's `task check`
// bullet to what it enumerates: every glossed member of the `check` closure
// is named by its gloss words in the bullet itself, not somewhere else in the
// file.
func TestTheAgentsMdGlossNamesEveryGlossedMember(t *testing.T) {
	t.Parallel()

	closure := taskClosure(t, "check")
	lines := strings.Split(string(mustRead(t, agentsMdPath)), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "- `task check`") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no `- \\`task check\\`` bullet: the enumeration moved and this guard's premise with it", agentsMdPath)
	}
	var bullet []string
	for _, line := range lines[start:] {
		if len(bullet) > 0 && (line == "" || strings.HasPrefix(line, "## ")) {
			break
		}
		bullet = append(bullet, line)
	}
	glossed := strings.Join(bullet, "\n")
	for _, member := range slices.Sorted(maps.Keys(agentsMdGlosses)) {
		if !closure[member] {
			t.Errorf("agentsMdGlosses pins %q, which the `check` closure does not run: the entry is stale, drop it", member)
			continue
		}
		if !strings.Contains(glossed, agentsMdGlosses[member]) {
			t.Errorf("%s's `task check` bullet enumerates the gate's members but does not name %s (%q runs it): add its gloss to the bullet, as docs/development.md's does", agentsMdPath, agentsMdGlosses[member], member)
		}
	}
}

// TestTaskCheckRunsEveryRequiredMemberOrSaysWhy holds `task check` — the gate
// every session owes — to the gate map: every member of a required or staged
// row is either inside the `check` closure or named in beyondTaskCheck with a
// reason. Without this, a member CI requires (fmt:check today) stays green
// locally and reddens only in CI, exactly the gap the local gate exists to
// close.
//
// The witnesses: remove `- task: fmt:check` from `check:` and this names it;
// add `- task: ci:gofmt` back and the stale beyondTaskCheck entry names it;
// put `platforms: [windows]` on the fmt:check CALL and the refusal fires
// (call-site platforms is dead config go-task ignores); put it on the
// fmt:check DEFINITION and the closure walk fails the run — a gated task
// must not pass as run.
func TestTaskCheckRunsEveryRequiredMemberOrSaysWhy(t *testing.T) {
	t.Parallel()

	closure := taskClosure(t, "check")
	if len(closure) < 15 {
		t.Fatalf("`check` closes over %d tasks: either the gate was gutted or this guard is reading the wrong file", len(closure))
	}
	if !closure["test"] {
		t.Fatalf("`check` does not reach `test`: the closure walk itself is broken")
	}

	m := readGateMap(t)
	reached := reachedRequired(m)
	if !reached["fmt-check"] {
		t.Fatalf("the fmt-check row is neither required nor staged: the gate map changed shape and this guard's premise with it")
	}

	covered := make(map[string]bool, len(beyondTaskCheck))
	for _, row := range slices.Sorted(maps.Keys(reached)) {
		job := m.Jobs[row]
		for _, member := range job.Members {
			inClosure := closure[member]
			_, excused := beyondTaskCheck[member]
			switch {
			case !inClosure && !excused:
				t.Errorf("gate row %q (required=%s) names member %q and neither `task check` nor beyondTaskCheck runs it: add `- task: %s` to `check:` in %s, or add it to beyondTaskCheck with a reason",
					row, job.Required, member, member, taskfilePath)
			case inClosure && excused:
				t.Errorf("gate row %q names member %q: `task check` runs it AND beyondTaskCheck excuses it — the entry is stale, drop it", row, member)
			}
			covered[member] = true
		}
	}
	for member := range beyondTaskCheck {
		if !covered[member] {
			t.Errorf("beyondTaskCheck[%q] is a member of no required/staged gate row: the entry is dead, drop it", member)
		}
	}
}

// TestTheFastLayerJudgesNothingTaskCheckSkips holds the documented relation
// between the two layers: `check:fast` is a fast inner-iteration SUBSET of
// `task check`, which stays owed before every push. A check the fast layer
// judges and the owed gate skips would let a red tree reach CI dressed in a
// green fast layer.
//
// The witness: remove `- task: fmt:check` from `check:` and this names it —
// the fast layer judged what the owed gate skipped, which is precisely how
// the gap stayed invisible.
func TestTheFastLayerJudgesNothingTaskCheckSkips(t *testing.T) {
	t.Parallel()

	full := taskClosure(t, "check")
	fast := taskClosure(t, "check:fast")
	if len(fast) < 5 {
		t.Fatalf("`check:fast` closes over %d tasks: either the fast layer was gutted or this guard is reading the wrong file", len(fast))
	}
	for _, name := range slices.Sorted(maps.Keys(fast)) {
		if name == "check:fast" {
			continue
		}
		if !full[name] {
			t.Errorf("`check:fast` runs %q and `task check` does not: the fast layer judges something the owed gate skips", name)
		}
	}
}
