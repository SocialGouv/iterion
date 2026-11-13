package clawfold

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// synthetic builds a census from package → (lines, deps) and asset → lines,
// listing the members in the order given so a test can feed both orders.
func synthetic(pkgs []string, lines map[string]int, deps map[string][]string, assets []string, assetLines map[string]int) *Census {
	c := &Census{Packages: map[string]*Package{}}
	for _, p := range pkgs {
		c.Packages[p] = &Package{Dir: p, Deps: deps[p], Lines: lines[p], Files: 1}
		c.Members = append(c.Members, &Member{Path: p + "/x.go", Kind: KindGo, Package: p, Lines: lines[p]})
	}
	for _, a := range assets {
		c.Members = append(c.Members, &Member{Path: a, Kind: KindAsset, Lines: assetLines[a]})
	}
	return c
}

func shape(slices []Slice) []string {
	var out []string
	for _, s := range slices {
		var names []string
		for _, u := range s.Units {
			names = append(names, strings.Join(append([]string{}, u.Packages...), "+")+u.assetName())
		}
		out = append(out, fmt.Sprintf("%d:%s:%d:%s", s.Index, s.Kind, s.Lines, strings.Join(names, ",")))
	}
	return out
}

func (u *Unit) assetName() string {
	if len(u.Packages) == 0 {
		return u.Name
	}
	return ""
}

func TestComponentsContractACycleThatOnlyTheTestsClose(t *testing.T) {
	t.Parallel()
	// a's sources import b; b's external test imports a back.
	c := synthetic([]string{"a", "b", "c"}, map[string]int{"a": 10, "b": 10, "c": 10},
		map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"a"}}, nil, nil)
	got := Components(c)
	want := [][]string{{"a", "b"}, {"c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %v, want %v", got, want)
	}
	slices, err := Partition(c, 20, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(slices); !reflect.DeepEqual(got, []string{"1:go:20:a+b", "2:go:10:c"}) {
		t.Fatalf("slices = %v: the bound block lands whole, before what imports it", got)
	}
}

func TestPartitionLandsDependenciesFirst(t *testing.T) {
	t.Parallel()
	// big imports mid imports leaf; leaf is the smallest, so a packer that
	// ignored readiness would take big first.
	c := synthetic([]string{"big", "mid", "leaf"}, map[string]int{"big": 30, "mid": 20, "leaf": 10},
		map[string][]string{"big": {"mid"}, "mid": {"leaf"}}, nil, nil)
	slices, err := Partition(c, 30, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(slices); !reflect.DeepEqual(got, []string{"1:go:30:leaf,mid", "2:go:30:big"}) {
		t.Fatalf("slices = %v", got)
	}
}

func TestPartitionNamesTheFloor(t *testing.T) {
	t.Parallel()
	c := synthetic([]string{"tools", "small"}, map[string]int{"tools": 50, "small": 5}, nil,
		[]string{"skills/huge.md"}, map[string]int{"skills/huge.md": 40})
	_, err := Partition(c, 49, 100)
	var floor *FloorError
	if !errors.As(err, &floor) || floor.Unit != "tools" || floor.Lines != 50 || floor.Kind != SliceGo {
		t.Fatalf("err = %v, want the Go floor named: tools, 50 lines", err)
	}
	_, err = Partition(c, 100, 39)
	if !errors.As(err, &floor) || floor.Unit != "skills/huge.md" || floor.Kind != SliceAssets {
		t.Fatalf("err = %v, want the asset floor named", err)
	}
	if _, err := Partition(c, 0, 10); err == nil {
		t.Fatal("a zero cap was accepted")
	}
}

func TestPartitionPutsTheAssetsFirstUnderTheirOwnCap(t *testing.T) {
	t.Parallel()
	c := synthetic([]string{"p"}, map[string]int{"p": 10}, nil,
		[]string{"d/a.md", "d/b.md", "d/c.md"}, map[string]int{"d/a.md": 6, "d/b.md": 5, "d/c.md": 4})
	slices, err := Partition(c, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1:assets:10:d/a.md,d/c.md", "2:assets:5:d/b.md", "3:go:10:p"}
	if got := shape(slices); !reflect.DeepEqual(got, want) {
		t.Fatalf("slices = %v, want %v", got, want)
	}
}

func TestPartitionIsTheSameWhateverTheInputOrder(t *testing.T) {
	t.Parallel()
	// Equal sizes everywhere: only the tie-break decides, so a packer that
	// took the first ready unit it met would answer differently per order.
	pkgs := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	lines := map[string]int{}
	for _, p := range pkgs {
		lines[p] = 10
	}
	deps := map[string][]string{"p6": {"p1"}, "p5": {"p2"}}
	assets := []string{"a1", "a2", "a3"}
	assetLines := map[string]int{"a1": 5, "a2": 5, "a3": 5}

	forward, err := Partition(synthetic(pkgs, lines, deps, assets, assetLines), 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	reversedPkgs := []string{"p6", "p5", "p4", "p3", "p2", "p1"}
	reversedAssets := []string{"a3", "a2", "a1"}
	backward, err := Partition(synthetic(reversedPkgs, lines, deps, reversedAssets, assetLines), 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shape(forward), shape(backward)) {
		t.Fatalf("the plan depends on the input order:\n forward  %v\n backward %v", shape(forward), shape(backward))
	}
	want := []string{"1:assets:10:a1,a2", "2:assets:5:a3", "3:go:30:p1,p2,p3", "4:go:30:p4,p5,p6"}
	if got := shape(forward); !reflect.DeepEqual(got, want) {
		t.Fatalf("slices = %v, want %v", got, want)
	}
}

func TestPartitionNamesEveryFloorTheCapsMiss(t *testing.T) {
	t.Parallel()
	c := synthetic([]string{"tools"}, map[string]int{"tools": 50}, nil,
		[]string{"skills/huge.md"}, map[string]int{"skills/huge.md": 40})
	_, err := Partition(c, 10, 10)
	if err == nil || !strings.Contains(err.Error(), "-asset-cap 10 is below the floor: assets unit skills/huge.md") || !strings.Contains(err.Error(), "-cap 10 is below the floor: go unit tools") {
		t.Fatalf("err = %v, want both floors named at once", err)
	}
}
