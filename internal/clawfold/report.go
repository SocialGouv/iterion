package clawfold

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Totals summarises a census.
type Totals struct {
	Files, Lines           int
	GoFiles, GoTests       int
	GoLines                int
	AssetFiles, AssetLines int
	ByOrigin               map[Origin]int
	ByPackageOrigin        map[PackageOrigin]int
}

// Totals counts the census's members.
func (c *Census) Totals() Totals {
	t := Totals{ByOrigin: map[Origin]int{}, ByPackageOrigin: map[PackageOrigin]int{}}
	for _, m := range c.Members {
		t.Files++
		t.Lines += m.Lines
		t.ByOrigin[m.Origin]++
		switch m.Kind {
		case KindAsset:
			t.AssetFiles++
			t.AssetLines += m.Lines
		case KindGoTest:
			t.GoTests++
			fallthrough
		case KindGo:
			t.GoFiles++
			t.GoLines += m.Lines
		}
	}
	for _, p := range c.Packages {
		t.ByPackageOrigin[p.Origin]++
	}
	return t
}

// WriteCensus prints the membership: what folds, and by which rule.
func WriteCensus(w io.Writer, c *Census) {
	t := c.Totals()
	fmt.Fprintf(w, "pin %s\n", c.Pin)
	fmt.Fprintf(w, "membership: %d packages (%d vendored, %d by import closure, %d test-only) · %d files · %d lines\n",
		len(c.Packages), t.ByPackageOrigin[PackageVendored], t.ByPackageOrigin[PackageClosure], t.ByPackageOrigin[PackageTestOnly], t.Files, t.Lines)
	fmt.Fprintf(w, "  go:     %d files (%d tests) · %d lines · %d import sites rewritten\n", t.GoFiles, t.GoTests, t.GoLines, c.ImportSites)
	fmt.Fprintf(w, "  assets: %d files · %d lines (verbatim)\n", t.AssetFiles, t.AssetLines)
	fmt.Fprintf(w, "  by origin: %d vendored · %d package · %d asset · %d fixture\n",
		t.ByOrigin[OriginVendored], t.ByOrigin[OriginPackage], t.ByOrigin[OriginAsset], t.ByOrigin[OriginFixture])
	if len(c.Residuals) > 0 {
		fmt.Fprintf(w, "residuals — the module path outside an import, left verbatim (%d):\n", len(c.Residuals))
		for _, r := range c.Residuals {
			fmt.Fprintf(w, "  %s:%d  %s\n", r.File, r.Line, r.Text)
		}
	}
	for _, warning := range c.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
}

// WritePlan prints the census, the slices and the switch.
func WritePlan(w io.Writer, p *Plan) {
	WriteCensus(w, p.Census)
	for _, b := range p.Blocks {
		lines := 0
		for _, d := range b {
			lines += p.Packages[d].Lines
		}
		fmt.Fprintf(w, "bound block (tests import back): %s — %d lines, land together\n", strings.Join(b, ", "), lines)
	}
	floor := floorUnit(p)
	if floor != nil {
		fmt.Fprintf(w, "floor: %s — %d lines, the largest unit no slicing can split\n", floor.Name, floor.Lines)
	}
	fmt.Fprintf(w, "plan %s — slices at cap %d (assets at %d): %d\n", p.ID, p.Cap, p.AssetCap, len(p.Slices))
	for _, s := range p.Slices {
		fmt.Fprintf(w, "  %2d  %-6s  %6d lines  %4d files  %s\n", s.Index, s.Kind, s.Lines, s.Files, sliceSummary(s))
		if len(s.NewExternal) > 0 {
			fmt.Fprintf(w, "        + tidy/vendor add, beyond the cap, with their own dependencies: %s\n", strings.Join(s.NewExternal, ", "))
		}
	}
	if sp := p.Switch; sp != nil {
		fmt.Fprintf(w, "switch: %d iterion files · %d import sites · %d file(s) mention the name outside an import\n", len(sp.Consumers), sp.ImportSites, sp.ProseFiles)
		if len(sp.PathResiduals) > 0 {
			fmt.Fprintf(w, "switch residuals — paths that still name the module or its vendored tree (%d):\n", len(sp.PathResiduals))
			for _, r := range sp.PathResiduals {
				fmt.Fprintf(w, "  %s:%d  %s\n", r.File, r.Line, r.Text)
			}
		}
	}
}

func floorUnit(p *Plan) *Unit {
	var largest *Unit
	for _, s := range p.Slices {
		if s.Kind != SliceGo {
			continue
		}
		for _, u := range s.Units {
			if largest == nil || before(u, largest) {
				largest = u
			}
		}
	}
	return largest
}

func sliceSummary(s Slice) string {
	if s.Kind == SliceAssets {
		dirs := map[string]int{}
		for _, u := range s.Units {
			dirs[topDirs(u.Name, 3)]++
		}
		keys := sortedKeys(dirs)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s (%d)", k, dirs[k]))
		}
		return strings.Join(firstN(parts, 4), ", ")
	}
	var pkgs []string
	for _, u := range s.Units {
		pkgs = append(pkgs, u.Packages...)
	}
	sort.Strings(pkgs)
	return strings.Join(pkgs, ", ")
}

func topDirs(p string, n int) string {
	parts := strings.Split(dirOf(p), "/")
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "/")
}

// WriteVerify prints a verify report.
func WriteVerify(w io.Writer, r *VerifyReport) {
	fmt.Fprintf(w, "verify: %d file(s) checked, %d absent, %d finding(s)\n", r.Checked, r.Absent, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(w, "  %s: %s\n", f.Dest, f.Problem)
	}
}

// WriteApply prints an apply report.
func WriteApply(w io.Writer, r *ApplyReport) {
	step := fmt.Sprintf("slice %d", r.Slice)
	if r.Slice == 0 {
		step = "the switch"
	}
	fmt.Fprintf(w, "apply: %s — %d file(s) written, %d already in place\n", step, len(r.Written), r.Skipped)
	for _, args := range r.GoRan {
		fmt.Fprintf(w, "  ran go %s\n", strings.Join(args, " "))
	}
	if len(r.Resorted) > 0 {
		fmt.Fprintf(w, "  %d consumer(s) re-sorted by gofmt after the rewrite\n", len(r.Resorted))
	}
	for _, d := range r.ModDelta {
		fmt.Fprintf(w, "  go.mod: %s\n", d)
	}
	if len(r.NewExternal) > 0 {
		fmt.Fprintf(w, "  tidy/vendor brought: %s\n", strings.Join(r.NewExternal, ", "))
	}
	for _, o := range r.Owes {
		fmt.Fprintf(w, "  owes: %s\n", o)
	}
}
