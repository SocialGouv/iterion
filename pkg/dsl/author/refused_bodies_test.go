package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A refusal is said once, whatever body it empties: the text the parser
// sees is the document less what the converter refused — a body left
// without a line is closed as the parser reads an empty one, or goes with
// its header where the parser has no empty form of it (a preset, a string
// map, `expr`, a cursor's values or bands, `fallbacks`), a node's header
// taking the empty description a bare node reads as. No diagnostic of the
// parser follows a refusal onto the lines below it — at the end of the
// document, inside a group, a body nested in a body alike.
func TestARefusalIsSaidOnceWhateverBodyItEmpties(t *testing.T) {
	tail := "nodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n"
	agent := func(lines string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\n" + lines + "  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> b\n    - b -> done\n"
	}
	tool := func(lines string) string {
		return "dsl: 2\nnodes:\n  - tool: t\n    command: x\n" + lines + "  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> b\n    - b -> done\n"
	}
	for name, src := range map[string]string{
		"a preset, its one value refused":           "dsl: 2\nvars:\n  n: int\npresets:\n  p:\n    n: -1\n" + tail,
		"two presets, the first emptied":            "dsl: 2\nvars:\n  n: int\npresets:\n  p:\n    n: -1\n  q:\n    n: 2\n" + tail,
		"a preset, last of the document":            "dsl: 2\nvars:\n  n: int\n" + tail + "presets:\n  p:\n    n: -1\n",
		"vars, every entry refused":                 "dsl: 2\nvars:\n  n: 10\n" + tail,
		"secrets, every entry refused":              "dsl: 2\nsecrets:\n  s: 10\n" + tail,
		"a secret's block, its one part refused":    "dsl: 2\nsecrets:\n  s:\n    as: 10\n  t: x\n" + tail,
		"a schema, every field refused":             "dsl: 2\nschemas:\n  x:\n    f: 10\n" + tail,
		"a cursor's values, every value refused":    "dsl: 2\ncursors:\n  c:\n    values:\n      v: 10\n" + tail,
		"a cursor's values refused, then a part":    "dsl: 2\ncursors:\n  c:\n    values:\n      v: 10\n    description: d\n" + tail,
		"a cursor's bands, every band refused":      "dsl: 2\ncursors:\n  c:\n    bands:\n      0..1: 10\n" + tail,
		"a cursor, its one part refused":            "dsl: 2\ncursors:\n  c:\n    description: 10\n" + tail,
		"a supervisor, its one part refused":        "dsl: 2\nsupervisors:\n  s:\n    max_evals: -1\n" + tail,
		"an mcp server, its one part refused":       "dsl: 2\nmcp_servers:\n  m:\n    command: 10\n" + tail,
		"an mcp server's auth, its one part":        "dsl: 2\nmcp_servers:\n  m:\n    transport: http\n    url: https://x.test\n    auth:\n      scopes: 10\n" + tail,
		"a contract, its one part refused":          "dsl: 2\ncontracts:\n  c:\n    version: -1\n" + tail,
		"a contract's inputs, every port refused":   "dsl: 2\ncontracts:\n  c:\n    inputs:\n      x: 10\n" + tail,
		"a contract's outputs, every port refused":  "dsl: 2\ncontracts:\n  c:\n    outputs:\n      x: 10\n" + tail,
		"a contract port's block, its one part":     "dsl: 2\ncontracts:\n  c:\n    inputs:\n      x:\n        type: string\n        min_items: -1\n" + tail,
		"a contract criterion, its one part":        "dsl: 2\ncontracts:\n  c:\n    criteria:\n      k:\n        kind: 10\n" + tail,
		"a contract's criteria, every one refused":  "dsl: 2\ncontracts:\n  c:\n    criteria:\n      9k: null\n" + tail,
		"a contract effect, its one part":           "dsl: 2\ncontracts:\n  c:\n    effects:\n      e:\n        paid: 10\n" + tail,
		"a contract's effects, every one refused":   "dsl: 2\ncontracts:\n  c:\n    effects:\n      9e: null\n" + tail,
		"an attachment, every entry refused":        "dsl: 2\nattachments:\n  x: 10\n" + tail,
		"an attachment's block, its one part":       "dsl: 2\nattachments:\n  x:\n    type: file\n    required: 10\n" + tail,
		"an agent's fallbacks, every route refused": agent("    fallbacks:\n      - route: 10\n"),
		"a fallback route, its one part refused":    agent("    fallbacks:\n      - route: r\n        model: 10\n"),
		"an agent's only property, its fallbacks":   "dsl: 2\nnodes:\n  - agent: a\n    fallbacks:\n      - route: 10\n  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> b\n    - b -> done\n",
		"an agent's mcp block, its one part":        agent("    mcp:\n      servers: 10\n"),
		"an agent's compaction, its one part":       agent("    compaction:\n      threshold: -1\n"),
		"an agent's memory, its one part":           agent("    memory:\n      enabled: 10\n"),
		"an agent's sandbox, its one part":          agent("    sandbox:\n      image: 10\n"),
		"a sandbox's env, every entry refused":      agent("    sandbox:\n      env:\n        A: 10\n"),
		"a sandbox's env refused, last of the doc":  "dsl: 2\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\nnodes:\n  - agent: a\n    model: m\n    sandbox:\n      env:\n        A: 10\n",
		"a sandbox build's args, every one refused": agent("    sandbox:\n      build:\n        args:\n          A: 10\n"),
		"a sandbox's network, its one part":         agent("    sandbox:\n      network:\n        rules: 10\n"),
		"an agent's cursors, its one entry":         agent("    cursors:\n      tone: 1e2\n"),
		"a tool's recovery, its one part":           tool("    recovery:\n      max_repair_attempts: -1\n"),
		"a compute's expr, every entry refused":     "dsl: 2\nnodes:\n  - compute: c\n    expr:\n      a: 10\n  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: c\n  edges:\n    - c -> b\n    - b -> done\n",
		"a tool's params, every entry refused":      "dsl: 2\nnodes:\n  - tool: c\n    action: forgejo.issue.comment\n    connection: forge-main\n    params:\n      a: 0x10\n  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: c\n  edges:\n    - c -> b\n    - b -> done\n",
		"a workflow's budget, its one part":         "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  budget:\n    max_tokens: -1\n  edges:\n    - a -> done\n",
		"a workflow's resources, its one entry":     "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  resources:\n    gpu: 1.5\n  edges:\n    - a -> done\n",
		"a workflow's vars, its one entry":          "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  vars:\n    n: 10\n  edges:\n    - a -> done\n",
		"a workflow, every part refused":            "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: 10\n",
		"a group, every node refused":               "dsl: 2\ngroups:\n  - group: g\n    nodes:\n      - agent: 10\n" + tail,
		"a group's node, every part refused":        "dsl: 2\ngroups:\n  - group: g\n    nodes:\n      - agent: x\n        model: 10\n" + tail,
		"a group node's sandbox env refused":        "dsl: 2\ngroups:\n  - group: g\n    nodes:\n      - agent: x\n        model: m\n        sandbox:\n          env:\n            A: 10\n      - agent: y\n        model: m\n" + tail,
	} {
		res := Parse("x.bot.yaml", []byte(src))
		var own, other []string
		for _, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorDocument || d.Code == parser.DiagAuthorValue {
				own = append(own, d.Error())
			} else {
				other = append(other, d.Error())
			}
		}
		if len(own) == 0 {
			t.Errorf("%s: nothing refused — the case exercises nothing", name)
		}
		if len(other) > 0 {
			t.Errorf("%s: the refusal is read again by the parser:\n  %s\n--- the text it read:\n%s", name, strings.Join(other, "\n  "), res.Text)
		}
	}
}
