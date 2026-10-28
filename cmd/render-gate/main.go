// Command render-gate reads a rendered Kubernetes manifest stream (`helm
// template`, `kustomize build`, an Argo CD manifest dump) and refuses every
// scheduling or disruption selector that selects nothing — a podAntiAffinity
// term, a topologySpreadConstraint, a podAffinity term or a
// PodDisruptionBudget whose labels no pod of the stream carries (#2012).
//
//	helm template iterion charts/iterion -f values.yaml | go run ./cmd/render-gate
//	go run ./cmd/render-gate -input rendered.yaml
//
// Exit status: 0 when every selector is alive, 1 on a dead selector (or on a
// cross-workload one with -cross=fail), 2 when the stream cannot be read.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/SocialGouv/iterion/internal/rendergate"
)

func main() {
	input := flag.String("input", "-", "rendered manifest stream to judge; - reads stdin")
	cross := flag.String("cross", "warn", "a selector matching only OTHER workloads of its namespace: warn (report, exit 0), fail (exit 1) or ignore")
	flag.Parse()
	switch *cross {
	case "warn", "fail", "ignore":
	default:
		fmt.Fprintf(os.Stderr, "render-gate: -cross must be warn, fail or ignore (got %q)\n", *cross)
		os.Exit(2)
	}
	stream, err := read(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-gate: %v\n", err)
		os.Exit(2)
	}
	report, err := rendergate.Judge(stream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-gate: %v\n", err)
		os.Exit(2)
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	failed := false
	for _, f := range report.Findings {
		if f.Verdict == rendergate.CrossWorkload && *cross == "ignore" {
			continue
		}
		fmt.Println(f.String())
		if f.Verdict == rendergate.Dead || (f.Verdict == rendergate.CrossWorkload && *cross == "fail") {
			failed = true
		}
	}
	fmt.Fprintln(os.Stderr, report.Summary())
	if failed {
		os.Exit(1)
	}
}

func read(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
