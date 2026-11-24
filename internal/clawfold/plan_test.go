package clawfold

import (
	"slices"
	"strings"
	"testing"
)

func TestPlanSwitchLeavesTheToolAndNestedModulesAlone(t *testing.T) {
	t.Parallel()
	src, _ := newSource(t)
	root := newRoot(t, src)
	writeTree(t, root, map[string]string{
		"internal/clawfold/k.go": "package clawfold\n\nconst source = \"github.com/SocialGouv/claw-code-go\"\n",
		"tools/go.mod":           "module example.com/tools\n\ngo 1.26\n",
		"tools/t.go":             consumerSrc,
	})
	commitAll(t, root, "the tool and a nested module")
	sp, err := PlanSwitch(root)
	if err != nil {
		t.Fatal(err)
	}
	var consumers []string
	for _, c := range sp.Consumers {
		consumers = append(consumers, c.File)
	}
	if !slices.Equal(consumers, []string{"cmd/consumer/main.go"}) {
		t.Fatalf("consumers = %v: the nested module's file resolves through its own go.mod", consumers)
	}
	for _, r := range sp.PathResiduals {
		if strings.HasPrefix(r.File, "internal/clawfold/") {
			t.Fatalf("the tool's own source is a residual: %+v", r)
		}
	}
}

func TestBuildPlanNamesTheExternalPackagesASliceBrings(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{
		"internal/testutil/schema_test.go": "package testutil\n\nimport _ \"example.com/jsonschema/v6\"\n",
	})
	root := newRoot(t, src)
	c, err := Load(Inputs{Root: root, Source: src, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(c, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var external []string
	for _, s := range p.Slices {
		external = append(external, s.NewExternal...)
	}
	if !slices.Equal(external, []string{"example.com/jsonschema/v6"}) {
		t.Fatalf("new external packages = %v, want the one vendor/modules.txt lacks", external)
	}
}

func TestThePlanIDNamesWhatLands(t *testing.T) {
	t.Parallel()
	c, _, _ := fixtureCensus(t)
	floor := goFloor(t, c)
	a, err := BuildPlan(c, floor, 40)
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildPlan(c, floor, 40)
	if err != nil {
		t.Fatal(err)
	}
	wider, err := BuildPlan(c, floor*10, 40)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != again.ID || a.ID == wider.ID || len(a.ID) != 12 {
		t.Fatalf("ids %q %q %q: want the same slices named the same, other slices named otherwise", a.ID, again.ID, wider.ID)
	}
	// Same paths, one byte apart: another plan.
	src, pin := newSourceWith(t, map[string]string{"internal/core/skills/a.md": "# a bundled skill!\n"})
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	vendored := vendoredMap(t, src, vendoredFixture)
	other, err := Compute(tree, vendored)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildPlan(other, floor, 40)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatalf("two folds differing by one byte share the id %s", a.ID)
	}
	// Same bytes, one mode apart: another plan.
	flipped := *a
	flipped.Census = &Census{Members: nil}
	for _, m := range a.Members {
		mm := *m
		if mm.Path == "internal/core/skills/run.sh" {
			mm.Mode = "100644"
		}
		flipped.Members = append(flipped.Members, &mm)
	}
	if flipped.fingerprint() == a.ID {
		t.Fatal("two folds differing by an executable bit share an id")
	}
}
