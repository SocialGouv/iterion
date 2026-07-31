package bots

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestReviewHandoffIsDeclaredBySomeone guards the failure mode a declarative
// seam introduces: nothing in the engine names a reviewer or a fixer any more,
// so if no shipped bundle declares the two halves, the hand-off is simply OFF —
// silently, with every test still green, because a missing seed is
// indistinguishable by design from "there was no prior review".
//
// It asserts the ROLES are filled, never that particular bots fill them: any
// bundle may produce the review and any bundle may consume it.
func TestReviewHandoffIsDeclaredBySomeone(t *testing.T) {
	manifests := loadCatalogManifests(t)

	var producers, consumers []string
	for name, m := range manifests {
		for _, p := range m.Produces {
			if p.Kind == bundle.HandoffKindReview {
				producers = append(producers, name)
			}
		}
		for _, c := range m.Consumes {
			if c.Kind == bundle.HandoffKindReview {
				consumers = append(consumers, name)
			}
		}
	}
	if len(producers) == 0 {
		t.Error("no catalog bot declares `produces: kind: review` — nothing can ever seed a fixer, and the miss is silent")
	}
	if len(consumers) == 0 {
		t.Error("no catalog bot declares `consumes: kind: review` — a review is produced and handed to nobody")
	}
	t.Logf("review producers: %v · consumers: %v", producers, consumers)
}

// TestConsumedVarIsDeclaredByTheWorkflow: the engine stamps the consumed digest
// into a launch var, and the IR silently DROPS a var the workflow never
// declared. A typo in `consumes.var` would therefore cost the whole hand-off
// with no error anywhere.
func TestConsumedVarIsDeclaredByTheWorkflow(t *testing.T) {
	for name, m := range loadCatalogManifests(t) {
		if len(m.Consumes) == 0 {
			continue
		}
		wf := compileBot(t, name)
		if wf == nil {
			continue
		}
		for _, c := range m.Consumes {
			if wf.Vars[c.Var] == nil {
				t.Errorf("%s: manifest consumes into `%s`, but main.bot declares no such var — the stamped value is dropped by the IR and the hand-off silently does nothing", name, c.Var)
			}
		}
	}
}

// TestProducedNodesExist: same class of silent miss on the producing side — a
// renamed node leaves the artifact unreadable, which the resolver treats as
// "this review has nothing to say yet" and skips.
func TestProducedNodesExist(t *testing.T) {
	for name, m := range loadCatalogManifests(t) {
		if len(m.Produces) == 0 {
			continue
		}
		wf := compileBot(t, name)
		if wf == nil {
			continue
		}
		for _, p := range m.Produces {
			nodes := append([]string{p.Node}, p.FallbackNodes...)
			if p.AnchorNode != "" {
				nodes = append(nodes, p.AnchorNode)
			}
			for _, node := range nodes {
				n := wf.Nodes[node]
				if n == nil {
					t.Errorf("%s: manifest produces from node %q, which main.bot does not declare — the artifact is never found and the hand-off silently yields nothing", name, node)
					continue
				}
				// Declaring the node is not enough: the engine writes an artifact
				// ONLY for a node carrying `publish:`, so naming one that
				// publishes nothing hands over nothing. e2e/handoff_publish_test.go
				// pins that engine behaviour; this pins the bots against it.
				if ir.NodePublish(n) == "" {
					t.Errorf("%s: node %q is declared as a hand-off source but has no `publish:` — it writes no artifact, so the hand-off silently yields nothing", name, node)
				}
			}
		}
	}
}

// compileBot compiles one catalog bot to its IR, so these checks ask the
// compiler what a node and a var ARE rather than pattern-matching the source.
// A line scanner cannot tell `schema converge:` from `agent converge:`; the IR
// cannot express that confusion. Every catalog bot is compiled in this package
// already (catalog_parse_compile_test.go), so this adds no new dependency.
func compileBot(t *testing.T, name string) *ir.Workflow {
	t.Helper()
	path := filepath.Join(botsDir(t), name, "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: main.bot is unreadable: %v", name, err)
		return nil
	}
	pr := parser.Parse(path, string(src))
	if pr.File == nil {
		t.Errorf("%s: main.bot does not parse", name)
		return nil
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Errorf("%s: main.bot does not compile", name)
		return nil
	}
	return cr.Workflow
}

func botsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func loadCatalogManifests(t *testing.T) map[string]*bundle.Manifest {
	t.Helper()
	entries, err := os.ReadDir(botsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]*bundle.Manifest)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(botsDir(t), e.Name(), "manifest.yaml")
		if _, serr := os.Stat(path); serr != nil {
			continue
		}
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Errorf("%s: manifest does not parse: %v", e.Name(), err)
			continue
		}
		out[e.Name()] = m
	}
	if len(out) == 0 {
		t.Fatal("no catalog manifests found — the test is looking in the wrong place")
	}
	return out
}
