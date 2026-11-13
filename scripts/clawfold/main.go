// Command clawfold plans, applies and proves the fold of claw-code-go into
// pkg/claw (see internal/clawfold). It is what `task claw:fold:plan` runs.
//
//	clawfold plan [-cap N] [-asset-cap N] [-json]  # the membership, the slices, the switch; writes nothing
//	clawfold apply -slice K -plan ID               # land slice K of plan ID, then go mod tidy + go mod vendor
//	clawfold apply -switch                         # move iterion's imports, drop the module
//	clawfold verify [-slice K]                     # replay the per-file proofs on the tree
//	clawfold rewrite PATH...                       # the import rewrite alone, in place
//	clawfold unrewrite PATH...                     # its inverse
//
// plan, apply and verify share -root, -source, -pin, -cap and -asset-cap.
// The caps decide what slice K is, so `apply -slice` takes the ID `plan`
// printed and refuses to land a slice of any other plan.
//
// Exit status of the binary: 0 on success, 1 when a proof fails or the fold
// refuses, 2 on a usage error or a refused precondition (uncommitted
// changes). Through `go run` or `task`, every failure reads as non-zero.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"

	"github.com/SocialGouv/iterion/internal/clawfold"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: clawfold plan|apply|verify|rewrite|unrewrite [flags]
  plan       print the membership, the slices and the switch; writes nothing
  apply      -slice K: land one slice; -switch: the last step
  verify     replay the per-file proofs on the tree ([-slice K])
  rewrite    rewrite the module's import paths under pkg/claw, in place
  unrewrite  the inverse of rewrite`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "plan", "apply", "verify":
		return runPlanned(cmd, rest, stdout, stderr)
	case "rewrite", "unrewrite":
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "clawfold %s: name the files or directories to %s\n", cmd, cmd)
			return 2
		}
		m := clawfold.Fold
		if cmd == "unrewrite" {
			m = m.Inverse()
		}
		changed, err := clawfold.RewritePaths(rest, m)
		for _, f := range changed {
			fmt.Fprintln(stdout, f)
		}
		if err != nil {
			fmt.Fprintf(stderr, "clawfold %s: %v\n", cmd, err)
			return 1
		}
		fmt.Fprintf(stderr, "clawfold %s: %d file(s) changed\n", cmd, len(changed))
		return 0
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "clawfold: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
}

func runPlanned(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clawfold "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the iterion checkout (default: the git toplevel of the working directory)")
	source := fs.String("source", "", "a claw-code-go clone holding the pin (default: <primary checkout>/.repos/claw-code-go)")
	pin := fs.String("pin", "", "the claw-code-go revision to read (default: the one vendor/modules.txt holds); its tree must hash to go.sum's h1: for the vendored version")
	lineCap := fs.Int("cap", 16000, "the most lines a Go slice may add")
	assetCap := fs.Int("asset-cap", 0, "the most lines an asset slice may add (default: -cap)")
	asJSON := fs.Bool("json", false, "plan: print the plan as JSON, the per-file manifest included")
	slice := fs.Int("slice", 0, "apply/verify: the slice, 1-based")
	planID := fs.String("plan", "", "apply -slice: the ID `plan` printed — the slice must be that plan's")
	doSwitch := fs.Bool("switch", false, "apply: the last step — iterion's imports move, the module leaves go.mod")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "clawfold %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}
	allowed := map[string][]string{
		"plan":   {"json"},
		"verify": {"slice"},
		"apply":  {"slice", "plan", "switch"},
	}
	common := []string{"root", "source", "pin", "cap", "asset-cap"}
	misplaced := ""
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = true
		if !slices.Contains(allowed[cmd], f.Name) && !slices.Contains(common, f.Name) {
			misplaced = f.Name
		}
		// The switch lands every slice's files, whatever cut them: a cap or
		// a plan id given with it would read as a constraint it ignores.
		if *doSwitch && (f.Name == "plan" || f.Name == "cap" || f.Name == "asset-cap") {
			misplaced = f.Name
		}
	})
	if misplaced != "" {
		fmt.Fprintf(stderr, "clawfold %s: -%s does not apply here\n", cmd, misplaced)
		return 2
	}
	if *slice < 0 || (set["slice"] && *slice == 0) {
		fmt.Fprintf(stderr, "clawfold %s: -slice %d: slices are numbered from 1\n", cmd, *slice)
		return 2
	}
	if *assetCap == 0 {
		*assetCap = *lineCap
	}
	if *doSwitch {
		*lineCap, *assetCap = math.MaxInt32, math.MaxInt32
	}
	if cmd == "apply" && (*slice > 0) == *doSwitch {
		fmt.Fprintln(stderr, "clawfold apply: give exactly one of -slice K and -switch")
		return 2
	}

	in, err := resolve(*root, *source, *pin)
	if err != nil {
		fmt.Fprintf(stderr, "clawfold %s: %v\n", cmd, err)
		return 1
	}
	census, err := clawfold.Load(in)
	if err != nil {
		fmt.Fprintf(stderr, "clawfold %s: %v\n", cmd, err)
		return 1
	}
	plan, err := clawfold.BuildPlan(census, *lineCap, *assetCap)
	if err != nil {
		clawfold.WriteCensus(stdout, census)
		fmt.Fprintf(stderr, "clawfold %s: %v\n", cmd, err)
		return 1
	}
	if *slice > len(plan.Slices) {
		fmt.Fprintf(stderr, "clawfold %s: -slice %d: plan %s has %d slices\n", cmd, *slice, plan.ID, len(plan.Slices))
		return 2
	}
	if cmd == "apply" && *slice > 0 && *planID != plan.ID {
		fmt.Fprintf(stderr, "clawfold apply: this invocation computes plan %s (pin %s, -cap %d, -asset-cap %d); pass -plan %s if that is the plan you reviewed (given: %q)\n",
			plan.ID, census.Pin[:12], plan.Cap, plan.AssetCap, plan.ID, *planID)
		return 2
	}

	switch cmd {
	case "plan":
		// The census and the slices stand without the switch: a tree the
		// switch could not read (a sparse checkout) still gets its plan.
		sp, err := clawfold.PlanSwitch(in.Root)
		switch {
		case errors.Is(err, clawfold.ErrDirtyTree):
			fmt.Fprintf(stderr, "clawfold plan: the switch is not planned: %v\n", err)
		case err != nil:
			fmt.Fprintf(stderr, "clawfold plan: %v\n", err)
			return 1
		}
		plan.Switch = sp
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(plan); err != nil {
				fmt.Fprintf(stderr, "clawfold plan: %v\n", err)
				return 1
			}
			return 0
		}
		fmt.Fprintf(stdout, "source %s at %s\n", in.Source, in.Pin)
		clawfold.WritePlan(stdout, plan)
		return 0
	case "verify":
		var slices []int
		if *slice > 0 {
			slices = []int{*slice}
		}
		r, err := plan.Verify(in.Root, slices...)
		if err != nil {
			fmt.Fprintf(stderr, "clawfold verify: %v\n", err)
			return 2
		}
		clawfold.WriteVerify(stdout, r)
		if len(r.Findings) > 0 {
			return 1
		}
		return 0
	default: // apply
		tool, err := clawfold.NewGoTool()
		if err != nil {
			fmt.Fprintf(stderr, "clawfold apply: %v\n", err)
			return 1
		}
		var report *clawfold.ApplyReport
		if *doSwitch {
			var sp *clawfold.SwitchPlan
			report, sp, err = plan.ApplySwitch(in.Root, tool)
			if sp != nil {
				plan.Switch = sp
			}
		} else {
			report, err = plan.Apply(in.Root, *slice, tool)
		}
		if report != nil {
			clawfold.WriteApply(stdout, report)
		}
		if *doSwitch && plan.Switch != nil {
			for _, r := range plan.Switch.PathResiduals {
				fmt.Fprintf(stdout, "  residual %s:%d  %s\n", r.File, r.Line, r.Text)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "clawfold apply: %v\n", err)
			if errors.Is(err, clawfold.ErrDirtyModule) || errors.Is(err, clawfold.ErrDirtyTree) {
				return 2
			}
			return 1
		}
		return 0
	}
}

func resolve(root, source, pin string) (clawfold.Inputs, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return clawfold.Inputs{}, err
		}
		if root, err = clawfold.Toplevel(wd); err != nil {
			return clawfold.Inputs{}, err
		}
	}
	if pin == "" {
		p, err := clawfold.PinFromVendor(root)
		if err != nil {
			return clawfold.Inputs{}, err
		}
		pin = p
	}
	if source == "" {
		s, err := clawfold.DefaultSource(root)
		if err != nil {
			return clawfold.Inputs{}, err
		}
		source = s
	}
	return clawfold.Inputs{Root: root, Source: source, Pin: pin}, nil
}
