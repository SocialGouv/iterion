package clawfold

import (
	"errors"
	"fmt"
	"sort"
)

// SliceKind says what a slice carries.
type SliceKind string

const (
	// SliceAssets carries non-Go files only: verbatim, no rewrite, no
	// dependency — inert until the package that embeds or reads them lands.
	SliceAssets SliceKind = "assets"
	// SliceGo carries whole Go packages whose every dependency, from their
	// sources and their tests, has landed in an earlier slice or this one.
	SliceGo SliceKind = "go"
)

// Unit is what a slice is made of: one asset file, or one strongly
// connected component of the package graph (a set of packages whose tests
// and sources import each other, which can only land together).
type Unit struct {
	Name     string   `json:"name"`               // the asset path, or the component's first package
	Packages []string `json:"packages,omitempty"` // empty for an asset
	Files    []string `json:"files"`              // member paths, sorted
	Lines    int      `json:"lines"`
	deps     []int
}

// Slice is one landing: a pull request's worth of the fold.
type Slice struct {
	Index int       `json:"index"` // 1-based, the order of landing
	Kind  SliceKind `json:"kind"`
	Units []*Unit   `json:"units"`
	Files int       `json:"files"`
	Lines int       `json:"lines"`
	// NewExternal are the external packages the slice imports first: tidy
	// and vendor add them to the same landing, beyond Lines.
	NewExternal []string `json:"new_external,omitempty"`
}

// FloorError says the cap is below the largest unit, which no slicing can
// split: a Go package cannot be cut across two pull requests.
type FloorError struct {
	Kind  SliceKind
	Unit  string
	Lines int
	Cap   int
}

func (e *FloorError) Error() string {
	flag := "-cap"
	if e.Kind == SliceAssets {
		flag = "-asset-cap"
	}
	return fmt.Sprintf("%s %d is below the floor: %s unit %s is %d lines and cannot be split (raise %s to at least %d)",
		flag, e.Cap, e.Kind, e.Unit, e.Lines, flag, e.Lines)
}

// Components returns the census's packages grouped into strongly connected
// components over the source+test import graph, each sorted, in a stable
// order (by first package).
func Components(c *Census) [][]string {
	dirs := sortedKeys(c.Packages)
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var comps [][]string
	next := 0
	var visit func(v string)
	visit = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range c.Packages[v].Deps {
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			comps = append(comps, comp)
		}
	}
	for _, v := range dirs {
		if _, seen := index[v]; !seen {
			visit(v)
		}
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i][0] < comps[j][0] })
	return comps
}

// Partition cuts the census into slices: the asset files first, packed under
// assetCap, then the Go components in dependency order under cap. Within a
// slice the packer takes, among the units whose dependencies have all
// landed (earlier or in this slice) and that still fit, the largest — ties
// broken by name — so the result depends on the census only, never on an
// iteration order.
func Partition(c *Census, cap, assetCap int) ([]Slice, error) {
	if cap <= 0 || assetCap <= 0 {
		return nil, errors.New("the caps must be positive line counts")
	}
	var assets []*Unit
	goFilesByPkg := map[string][]string{}
	for _, m := range c.Members {
		if m.Kind == KindAsset {
			assets = append(assets, &Unit{Name: m.Path, Files: []string{m.Path}, Lines: m.Lines})
			continue
		}
		goFilesByPkg[m.Package] = append(goFilesByPkg[m.Package], m.Path)
	}

	comps := Components(c)
	compOf := map[string]int{}
	for i, comp := range comps {
		for _, d := range comp {
			compOf[d] = i
		}
	}
	goUnits := make([]*Unit, len(comps))
	for i, comp := range comps {
		u := &Unit{Name: comp[0], Packages: comp}
		deps := map[int]bool{}
		for _, d := range comp {
			u.Files = append(u.Files, goFilesByPkg[d]...)
			u.Lines += c.Packages[d].Lines
			for _, dep := range c.Packages[d].Deps {
				if j := compOf[dep]; j != i {
					deps[j] = true
				}
			}
		}
		sort.Strings(u.Files)
		for j := range deps {
			u.deps = append(u.deps, j)
		}
		sort.Ints(u.deps)
		goUnits[i] = u
	}

	// Every floor the caps miss is named at once: a first refusal that hid
	// the second would send the operator round twice.
	var floors []error
	for _, f := range []*FloorError{floorOf(assets, assetCap, SliceAssets), floorOf(goUnits, cap, SliceGo)} {
		if f != nil {
			floors = append(floors, f)
		}
	}
	if len(floors) > 0 {
		return nil, errors.Join(floors...)
	}
	assetSlices, err := pack(assets, assetCap, SliceAssets)
	if err != nil {
		return nil, err
	}
	goSlices, err := pack(goUnits, cap, SliceGo)
	if err != nil {
		return nil, err
	}
	slices := append(assetSlices, goSlices...)
	for i := range slices {
		slices[i].Index = i + 1
	}
	return slices, nil
}

// floorOf returns the refusal for the largest unit when it exceeds cap.
func floorOf(units []*Unit, cap int, kind SliceKind) *FloorError {
	var largest *Unit
	for _, u := range units {
		if largest == nil || before(u, largest) {
			largest = u
		}
	}
	if largest != nil && largest.Lines > cap {
		return &FloorError{Kind: kind, Unit: largest.Name, Lines: largest.Lines, Cap: cap}
	}
	return nil
}

func pack(units []*Unit, cap int, kind SliceKind) ([]Slice, error) {
	landed := make([]bool, len(units))
	remaining := len(units)
	var slices []Slice
	for remaining > 0 {
		inSlice := make([]bool, len(units))
		s := Slice{Kind: kind}
		for {
			best := -1
			for i, u := range units {
				if landed[i] || inSlice[i] || s.Lines+u.Lines > cap || !ready(u, landed, inSlice) {
					continue
				}
				if best < 0 || before(u, units[best]) {
					best = i
				}
			}
			if best < 0 {
				break
			}
			inSlice[best] = true
			s.Units = append(s.Units, units[best])
			s.Lines += units[best].Lines
			s.Files += len(units[best].Files)
		}
		if len(s.Units) == 0 {
			return nil, fmt.Errorf("no %s unit can land: the dependency graph has a cycle the components did not contract", kind)
		}
		for i := range units {
			if inSlice[i] {
				landed[i] = true
				remaining--
			}
		}
		slices = append(slices, s)
	}
	return slices, nil
}

func ready(u *Unit, landed, inSlice []bool) bool {
	for _, d := range u.deps {
		if !landed[d] && !inSlice[d] {
			return false
		}
	}
	return true
}

// before orders units for the packer: more lines first, then by name.
func before(a, b *Unit) bool {
	if a.Lines != b.Lines {
		return a.Lines > b.Lines
	}
	return a.Name < b.Name
}
