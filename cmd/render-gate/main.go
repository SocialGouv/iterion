// Command render-gate reads a rendered Kubernetes manifest stream (`helm
// template`, `kustomize build`, an Argo CD manifest dump) and refuses every
// scheduling or disruption selector that selects nothing — a podAntiAffinity
// term, a topologySpreadConstraint, a podAffinity term or a
// PodDisruptionBudget whose labels no pod of the stream carries (#2012).
//
//	helm template iterion charts/iterion -f values.yaml | go run ./cmd/render-gate -namespace iterion
//	go run ./cmd/render-gate -input rendered.yaml
//
// Exit status: 0 when every selector is alive, 1 on a dead selector (or on a
// cross-workload one with -cross=fail), 1 on a stream without a single pod
// template (a gate that judged nothing proves nothing — a failed `helm
// template` upstream of the pipe looks exactly like that), 2 when the stream
// cannot be read.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/SocialGouv/iterion/internal/rendergate"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("render-gate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "-", "rendered manifest stream to judge; - reads stdin")
	cross := flags.String("cross", "warn", "a selector matching only OTHER workloads of its namespace: warn (report, exit 0), fail (exit 1) or ignore")
	namespace := flags.String("namespace", "default", "the namespace an object without metadata.namespace lands in: the release namespace of the render")
	allowEmpty := flags.Bool("allow-empty", false, "exit 0 on a stream without a pod template instead of refusing the verdict")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	switch *cross {
	case "warn", "fail", "ignore":
	default:
		fmt.Fprintf(stderr, "render-gate: -cross must be warn, fail or ignore (got %q)\n", *cross)
		return 2
	}
	stream, err := read(*input, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "render-gate: %v\n", err)
		return 2
	}
	report, err := rendergate.JudgeWithOptions(stream, rendergate.Options{DefaultNamespace: *namespace})
	if err != nil {
		fmt.Fprintf(stderr, "render-gate: %v\n", err)
		return 2
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	failed := false
	for _, f := range report.Findings {
		if f.Verdict == rendergate.CrossWorkload && *cross == "ignore" {
			continue
		}
		fmt.Fprintln(stdout, f.String())
		if f.Verdict == rendergate.Dead || (f.Verdict == rendergate.CrossWorkload && *cross == "fail") {
			failed = true
		}
	}
	fmt.Fprintln(stderr, report.Summary())
	if report.PodTemplates == 0 && !*allowEmpty {
		fmt.Fprintln(stderr, "render-gate: no pod template in the stream — nothing was judged, so nothing is proven (an empty or failed render upstream?); -allow-empty accepts that")
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

func read(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}
