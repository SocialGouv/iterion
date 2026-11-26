package main

import (
	"slices"
	"strings"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"go.mod", "go.mod", true},
		{"go.mod", "pkg/go.mod", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/a/b.go", true},
		{"**/*.go", "pkg/a/b.gox", false},
		{"vendor/**", "vendor/x/y.go", true},
		{"vendor/**", "vendor", true},
		{"vendor/**", "vendorx/y.go", false},
		{"docs/**/*.md", "docs/a.md", true},
		{"docs/**/*.md", "docs/adr/b/c.md", true},
		{"docs/**/*.md", "pkg/docs/a.md", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"sandbox/*/Dockerfile", "sandbox/claw/Dockerfile", true},
		{"sandbox/*/Dockerfile", "sandbox/a/b/Dockerfile", false},
		{"**", "anything/at/all", true},
		{"docs/public/og.png*", "docs/public/og.png.sha", true},
	} {
		if got := matchGlob(tc.glob, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func TestCheckGlobRefusesWhatMatchGlobWouldMisread(t *testing.T) {
	for _, g := range []string{"", "/abs", "./rel", "dir/", "a//b", "a/../b", "a/**x/b", "a/[/b"} {
		if checkGlob(g) == nil {
			t.Errorf("checkGlob(%q) accepted", g)
		}
	}
	for _, g := range []string{"**", "**/*.go", "docs/**", ".github/**", "a/*/b.c"} {
		if err := checkGlob(g); err != nil {
			t.Errorf("checkGlob(%q): %v", g, err)
		}
	}
}

const mod = "example.com/m"

// fixtureGraph: lib is imported by app's production code; app's tests link
// lib; tool's tests link testhelp only; docs has no package.
func fixtureGraph() *graph {
	g := &graph{module: mod, pkgs: map[string]*pkgInfo{}, byDir: map[string]*pkgInfo{}, nestedModules: map[string]bool{"e2e/fixtures/svc": true}}
	add := func(dir string, tests bool, prod []string, testDeps []string, embeds ...string) {
		p := g.get(mod + "/" + dir)
		p.dir = dir
		p.hasTests = tests
		for _, d := range prod {
			p.prodDeps[mod+"/"+d] = true
		}
		for _, d := range testDeps {
			p.testDeps[mod+"/"+d] = true
		}
		for _, e := range embeds {
			p.embeds[e] = true
		}
		g.byDir[dir] = p
	}
	add("pkg/lib", true, nil, []string{"pkg/lib"}, "pkg/lib/assets/a.json")
	add("pkg/app", true, []string{"pkg/lib"}, []string{"pkg/app", "pkg/lib"})
	add("pkg/app/sub", true, nil, []string{"pkg/app/sub"})
	add("internal/testhelp", false, nil, nil)
	add("pkg/tool", true, nil, []string{"pkg/tool", "internal/testhelp"})
	add("cmd/bin", false, []string{"pkg/app", "pkg/lib"}, nil)
	add("e2e", true, nil, []string{"e2e"})
	add("pkg/sweeper", true, nil, []string{"pkg/sweeper"})
	add("pkg/checker", true, nil, []string{"pkg/checker"})
	add("pkg/builder", true, nil, []string{"pkg/builder"})
	return g
}

func fixtureInputs() *inputs {
	return &inputs{
		Global: []rule{{Pattern: "go.mod", Why: "module"}, {Pattern: ".github/**", Why: "ci"}},
		Packages: map[string]readDecl{
			"pkg/sweeper": {Reads: []string{"docs/**/*.md"}, Why: "reads docs"},
			"pkg/checker": {Module: true, Why: "type-checks the module"},
			"pkg/builder": {Reach: []string{"cmd/bin"}, Why: "builds cmd/bin"},
		},
		Legs: map[string]leg{
			"unit":   {Packages: []string{"..."}, Paths: []string{"**/*.go"}, Why: "unit"},
			"e2e":    {Packages: []string{"e2e"}, Why: "e2e"},
			"web":    {Paths: []string{"web/**"}, Reach: []string{"cmd/bin"}, Why: "web"},
			"links":  {Paths: []string{"**/*.md"}, OnDelete: true, Why: "links"},
			"agg":    {Aggregates: []string{"e2e", "web"}, Why: "agg"},
			"lonely": {Paths: []string{"nothing/**"}, Why: "never"},
		},
	}
}

func dirs(res result) []string {
	var out []string
	for _, p := range res.Packages {
		out = append(out, p.Dir)
	}
	return out
}

func legs(res result) []string {
	var out []string
	for _, l := range res.Legs {
		out = append(out, l.Name)
	}
	return out
}

func run1(t *testing.T, changes ...change) result {
	t.Helper()
	return selectTests(fixtureGraph(), fixtureInputs(), changes)
}

func wantSel(t *testing.T, res result, pkgs, legNames []string) {
	t.Helper()
	if res.All {
		t.Fatalf("selected everything: %v", res.Reasons)
	}
	if got := dirs(res); !slices.Equal(got, pkgs) {
		t.Errorf("packages = %v, want %v", got, pkgs)
	}
	if got := legs(res); !slices.Equal(got, legNames) {
		t.Errorf("legs = %v, want %v", got, legNames)
	}
}

func TestAProductionChangeSelectsEveryTestBinaryLinkingIt(t *testing.T) {
	res := run1(t, change{Path: "pkg/lib/lib.go"})
	// lib (own), app (links lib), checker (module), builder (cmd/bin links lib).
	wantSel(t, res, []string{"pkg/app", "pkg/builder", "pkg/checker", "pkg/lib"}, []string{"agg", "unit", "web"})
}

func TestATestOnlyChangeSelectsItsOwnPackage(t *testing.T) {
	res := run1(t, change{Path: "pkg/lib/lib_test.go"})
	// The module type-check reads test files too.
	wantSel(t, res, []string{"pkg/checker", "pkg/lib"}, []string{"unit"})
}

func TestAHelperChangeReachesTestBinariesThroughTestImports(t *testing.T) {
	res := run1(t, change{Path: "internal/testhelp/h.go"})
	wantSel(t, res, []string{"pkg/checker", "pkg/tool"}, []string{"unit"})
}

func TestAnEmbeddedFileChangesItsPackageForImporters(t *testing.T) {
	res := run1(t, change{Path: "pkg/lib/assets/a.json"})
	wantSel(t, res, []string{"pkg/app", "pkg/builder", "pkg/checker", "pkg/lib"}, []string{"agg", "unit", "web"})
}

func TestADataFileSelectsEveryPackageAboveIt(t *testing.T) {
	// testdata of pkg/app/sub: sub owns it, and pkg/app's tests can read
	// down into it without leaving their directory.
	res := run1(t, change{Path: "pkg/app/sub/testdata/x.txt"})
	wantSel(t, res, []string{"pkg/app", "pkg/app/sub"}, []string{"unit"})
}

func TestADeclaredReadSelectsTheReader(t *testing.T) {
	res := run1(t, change{Path: "docs/guide/a.md"})
	wantSel(t, res, []string{"pkg/sweeper"}, []string{"links", "unit"})
	if why := strings.Join(res.Packages[0].Why, ";"); !strings.Contains(why, "docs/**/*.md") {
		t.Errorf("why = %q, want the declared glob named", why)
	}
}

func TestADeletionSelectsTheOnDeleteLegs(t *testing.T) {
	res := run1(t, change{Path: "pkg/app/sub/testdata/x.txt", Deleted: true})
	wantSel(t, res, []string{"pkg/app", "pkg/app/sub"}, []string{"links", "unit"})
}

func TestALegPathSelectsTheLegAndItsAggregator(t *testing.T) {
	res := run1(t, change{Path: "web/index.html"})
	wantSel(t, res, nil, []string{"agg", "web"})
}

func TestAGlobalTriggerSelectsEverything(t *testing.T) {
	res := run1(t, change{Path: "docs/a.md"}, change{Path: ".github/workflows/x.yml"})
	if !res.All || len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], ".github/**") {
		t.Fatalf("all = %v, reasons = %v", res.All, res.Reasons)
	}
	if len(res.Packages) != len(fixtureGraph().testPackages()) || len(res.Legs) != len(fixtureInputs().Legs) {
		t.Errorf("selected %d packages and %d legs, want all", len(res.Packages), len(res.Legs))
	}
}

func TestAnUnclaimedFileSelectsEverything(t *testing.T) {
	res := run1(t, change{Path: "random/notes.txt"})
	if !res.All || !strings.Contains(res.Reasons[0], "random/notes.txt is claimed by no package") {
		t.Fatalf("all = %v, reasons = %v", res.All, res.Reasons)
	}
}

func TestAWholeTreeGlobDoesNotVouchForAFile(t *testing.T) {
	in := fixtureInputs()
	in.Packages["pkg/sweeper"] = readDecl{Reads: []string{"**", "docs/**/*.md"}, Why: "sweeps the tree"}
	res := selectTests(fixtureGraph(), in, []change{{Path: "random/notes.txt"}})
	if !res.All {
		t.Fatalf("a file only a ** sweep matches was taken as known: %v", dirs(res))
	}
	res = selectTests(fixtureGraph(), in, []change{{Path: "docs/a.md"}})
	wantSel(t, res, []string{"pkg/sweeper"}, []string{"links", "unit"})
}

func TestAGoFileOutsideAnyPackageSelectsEverything(t *testing.T) {
	// A deleted package: its importers are unknown at head.
	res := run1(t, change{Path: "pkg/gone/gone.go", Deleted: true})
	if !res.All || !strings.Contains(res.Reasons[0], "no package at head") {
		t.Fatalf("all = %v, reasons = %v", res.All, res.Reasons)
	}
}

func TestAFixtureGoFileIsNoPackage(t *testing.T) {
	for _, p := range []string{"pkg/app/testdata/broken.go", "e2e/fixtures/svc/main.go"} {
		res := run1(t, change{Path: p})
		if res.All {
			t.Errorf("%s selected everything: %v", p, res.Reasons)
		}
	}
}

func TestAnEmptyDiffSelectsNothing(t *testing.T) {
	wantSel(t, run1(t), nil, nil)
}

func TestParseNameStatus(t *testing.T) {
	got, err := parseNameStatus("M\x00a.go\x00D\x00b/c.md\x00A\x00d e.txt\x00")
	if err != nil {
		t.Fatal(err)
	}
	want := []change{{Path: "a.go"}, {Path: "b/c.md", Deleted: true}, {Path: "d e.txt"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseNameStatus("M\x00a.go\x00D\x00"); err == nil {
		t.Fatal("an odd field count parsed")
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{0.5, 0.1, 0.9, 0.3, 0.7, 0.2, 0.4, 0.6, 0.8, 1.0}
	if got := percentile(xs, 0.5); got != 0.5 {
		t.Errorf("median = %v", got)
	}
	if got := percentile(xs, 0.9); got != 0.9 {
		t.Errorf("p90 = %v", got)
	}
}
