package bots

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A FAIL NODE'S MESSAGE IS WHAT THE OPERATOR READS, and it is the only thing
// they read: the run ends there, the code is a word, and the message is the
// reason. When it interpolates `outputs.<node>.<field>` of a node that is NOT
// the one that routed in, the operator is handed a sentence about something
// else.
//
// Three of those shipped in one bundle. The worst of them read
// `outputs.render.reason` on an edge leaving `render_plan`, and by that point
// in the run `render.reason` holds render's SUCCESS line — so the reason a run
// FAILED was "state document rendered at …". The other two handed the
// declaration lint's verdict for a survey that never materialised, and the
// coverage gate's verdict for extractors that never ran.
//
// The defect is invisible to every other check: the ref resolves, the node
// exists, the field exists, the type is right. Only the ROUTE is wrong, so
// only a test that reads the route catches it.
func TestCatalogFailMessagesReadTheNodeThatFailed(t *testing.T) {
	var targets []string
	for _, root := range []string{".", "../examples"} {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			targets = append(targets, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery likely broke")
	}

	outputsOf := regexp.MustCompile(`outputs\.([A-Za-z0-9_]+)\.`)
	checked := 0
	for _, path := range targets {
		pr := parseBotUnit(path)
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			t.Logf("%s: not inspected (does not compile to a workflow)", path)
			continue
		}

		// Who routes into each node.
		routes := map[string]map[string]bool{}
		for _, edge := range cr.Workflow.Edges {
			if routes[edge.To] == nil {
				routes[edge.To] = map[string]bool{}
			}
			routes[edge.To][edge.From] = true
		}

		for _, n := range cr.Workflow.Nodes {
			fail, ok := n.(*ir.FailNode)
			if !ok || fail.Message == nil || fail.Message.Raw == "" {
				continue
			}
			sources := routes[fail.ID]
			if len(sources) == 0 {
				continue // unreachable: another lint's business
			}
			named := map[string]bool{}
			for _, match := range outputsOf.FindAllStringSubmatch(fail.Message.Raw, -1) {
				named[match[1]] = true
			}
			if len(named) == 0 {
				continue // a constant message names nobody, and cannot mislead
			}
			checked++
			// THE RULE IS PER SOURCE, not per fail node. A message is ONE
			// string and a fail node may be reached from several edges, so
			// every edge into it must find its own verdict in that string:
			// for a source S, the message must name S itself or a node
			// immediately feeding S. The second form is the common and correct
			// shape — a compute gate decides and the tool beneath it holds the
			// diagnostic, so the message reads the tool.
			//
			// Checking the fail node as a whole ("at least one named node is
			// some source") is what let all three defects through: a node
			// shared by two sources, with a message that answers only one of
			// them, reads correct from the first edge and describes a node
			// that did not fail from the second.
			for _, source := range sortedKeys(sources) {
				quotable := map[string]bool{source: true}
				for feeder := range routes[source] {
					quotable[feeder] = true
				}
				if anyNamedIsASource(named, quotable) {
					continue
				}
				t.Errorf("%s: fail node %q is reached from %q, and its message names only %v — "+
					"neither %q itself nor anything feeding it. Taking that edge, the operator is "+
					"handed a verdict about a node that did not fail; when that node SUCCEEDED, "+
					"they are handed its success line as the reason the run ended.",
					path, fail.ID, source, sortedKeys(named), source)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fail node with an outputs ref was inspected — the guard would pass by scanning nothing")
	}
	t.Logf("%d fail message(s) with an outputs ref checked against their incoming edges", checked)
}

func anyNamedIsASource(named, sources map[string]bool) bool {
	for node := range named {
		if sources[node] {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
