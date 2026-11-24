// Command sandboxbenchpod renders the pod the kubernetes sandbox driver
// creates for a run, so the codemode posture bench measures THAT pod and not a
// hand-written copy of it (#3444, ADR-165).
//
// WHY A RENDERER AND NOT A TEMPLATE. The bench's runner arm writes its pod by
// hand because the pod it mirrors is the chart's, which a Go builder does not
// produce. A sandbox pod is produced by kubernetes.BuildPodManifest, and a copy
// of its securityContext drifts the day the driver changes one field — the
// bench would then certify a posture no run has. So this calls the driver's
// own builder and changes only what a bench has to change.
//
// WHAT IT CHANGES, and why each change leaves the confinement posture alone:
//
//   - the image: the bench image, built FROM the deployment's sandbox image
//     pinned by digest (task bench:codemode-bench-image), so the measured
//     `iterion` is the shipped one;
//   - the container's command and args: the bench instead of `sleep infinity`.
//     A run's claw-runner is exec'd into the sleeping container, and an exec'd
//     process inherits the container's seccomp profile, capabilities,
//     no_new_privs and user — the bench checks those four on itself
//     (`-require-uid`), so a difference would refuse the measurement rather
//     than pass it;
//   - the labels: the driver's `iterion.io/managed` label is what the runner's
//     orphan reaper lists, and its component label is what the spread
//     constraint counts. A bench pod carrying them in a runner namespace would
//     be reaped mid-measurement, or counted as a run;
//   - the resources: a bench pod is small and short by contract. Requests and
//     limits place and bound the pod; they are not part of the confinement,
//     and the bench refuses its memory leg rather than misreport it when the
//     limit is too low to hold the bombs;
//   - the pod-level seccompProfile, for the Unconfined twin only: the control
//     that proves the posture readback can tell a filter from none;
//   - the image pull policy, only when asked: the kind witness lane loads a
//     TAGGED image no node may pull, which the driver's policy for a tag
//     (Always) would try to.
//
// What it leaves out: the per-run CA, file secrets, proxy endpoint and owner
// reference the driver adds for a real run. They are environment and volumes,
// not posture, and a bench has none of them to give.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
)

// Input is everything the bench decides about a pod. Image, User, Namespace
// and Name reach the driver's builder; the rest replaces what a bench must
// replace.
type Input struct {
	Namespace string
	Name      string
	// RunLabel identifies the bench run, so its pods are found and removed by
	// label as well as by name.
	RunLabel string
	Image    string
	// User is the sandbox spec's `user`. Empty is what a spec that names only
	// an image resolves to: the image's own USER runs, as in a real run.
	User      string
	Seccomp   string
	Command   []string
	Args      []string
	Resources kubernetes.PodResources
	// DeadlineSeconds bounds the pod's lifetime, so a bench pod the driver
	// script fails to delete stops on its own.
	DeadlineSeconds int64
	// PullPolicy, when set, replaces the driver's imagePullPolicy.
	PullPolicy string
}

// The labels every bench pod carries in place of the driver's.
const (
	labelApp   = "app"
	appBench   = "codemode-posture-bench"
	labelRun   = "run"
	seccompRD  = "RuntimeDefault"
	seccompUnc = "Unconfined"
)

// Render returns the driver's pod for in, with the bench's substitutions.
func Render(in Input) (map[string]any, error) {
	switch in.Seccomp {
	case seccompRD, seccompUnc:
	default:
		return nil, fmt.Errorf("seccomp %q: want %s or %s", in.Seccomp, seccompRD, seccompUnc)
	}
	if len(in.Command) == 0 {
		return nil, errors.New("a command is required: without one the pod sleeps and measures nothing")
	}
	if in.RunLabel == "" {
		return nil, errors.New("a run label is required: it is how the bench's pods are found and removed")
	}
	// A tag lets the image move under the verdict. The one escape is the one
	// the driver script allows: nothing pulls under Never, so nothing moves.
	if !strings.Contains(in.Image, "@sha256:") && in.PullPolicy != "Never" {
		return nil, fmt.Errorf("image %q is not pinned by digest: a tag can move under the verdict (a tag is accepted only with pull policy Never)", in.Image)
	}
	raw, err := kubernetes.BuildPodManifest(kubernetes.PodManifestInput{
		Namespace:             in.Namespace,
		Name:                  in.Name,
		RunID:                 in.RunLabel,
		Spec:                  sandbox.Spec{Image: in.Image, User: in.User},
		Resources:             in.Resources,
		ActiveDeadlineSeconds: in.DeadlineSeconds,
	})
	if err != nil {
		return nil, err
	}
	var pod map[string]any
	if err := json.Unmarshal(raw, &pod); err != nil {
		return nil, fmt.Errorf("the driver's manifest is not JSON: %w", err)
	}

	meta, ok := pod["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("the driver's manifest has no metadata object")
	}
	meta["labels"] = map[string]any{labelApp: appBench, labelRun: in.RunLabel}

	spec, ok := pod["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("the driver's manifest has no spec object")
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) != 1 {
		return nil, fmt.Errorf("the driver's manifest has %d containers; the bench replaces the command of exactly one", len(containers))
	}
	workload, ok := containers[0].(map[string]any)
	if !ok {
		return nil, errors.New("the driver's workload container is not an object")
	}
	workload["command"] = toAny(in.Command)
	if in.PullPolicy != "" {
		workload["imagePullPolicy"] = in.PullPolicy
	}
	if len(in.Args) > 0 {
		workload["args"] = toAny(in.Args)
	}

	if in.Seccomp == seccompUnc {
		podSec, ok := spec["securityContext"].(map[string]any)
		if !ok {
			return nil, errors.New("the driver's manifest has no pod securityContext to vary")
		}
		podSec["seccompProfile"] = map[string]any{"type": seccompUnc}
	}
	return pod, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func main() {
	var (
		in                             Input
		command, args                  string
		cpuReq, memReq, cpuLim, memLim string
	)
	flag.StringVar(&in.Namespace, "namespace", "", "the pod's namespace (required)")
	flag.StringVar(&in.Name, "name", "", "the pod's name (required)")
	flag.StringVar(&in.RunLabel, "run", "", "the bench run's label value (required)")
	flag.StringVar(&in.Image, "image", "", "the bench image, pinned by digest (required)")
	flag.StringVar(&in.User, "user", "", "the sandbox spec's user; empty runs the image's own USER")
	flag.StringVar(&in.Seccomp, "seccomp", seccompRD, "RuntimeDefault (the driver's) or Unconfined (the twin)")
	flag.StringVar(&command, "command", "", "the container command, as a JSON array (required)")
	flag.StringVar(&args, "args", "[]", "the container args, as a JSON array")
	flag.StringVar(&cpuReq, "cpu-request", "", "the container's CPU request")
	flag.StringVar(&memReq, "memory-request", "", "the container's memory request")
	flag.StringVar(&cpuLim, "cpu-limit", "", "the container's CPU limit")
	flag.StringVar(&memLim, "memory-limit", "", "the container's memory limit")
	flag.Int64Var(&in.DeadlineSeconds, "deadline", 900, "activeDeadlineSeconds: the pod stops on its own past this")
	flag.StringVar(&in.PullPolicy, "pull-policy", "", "replace the driver's imagePullPolicy (empty keeps it)")
	flag.Parse()

	if err := json.Unmarshal([]byte(command), &in.Command); err != nil {
		fail(fmt.Errorf("-command %q is not a JSON array of strings: %w", command, err))
	}
	if err := json.Unmarshal([]byte(args), &in.Args); err != nil {
		fail(fmt.Errorf("-args %q is not a JSON array of strings: %w", args, err))
	}
	in.Resources = kubernetes.PodResources{
		Requests: kubernetes.ResourceList{CPU: cpuReq, Memory: memReq},
		Limits:   kubernetes.ResourceList{CPU: cpuLim, Memory: memLim},
	}
	pod, err := Render(in)
	if err != nil {
		fail(err)
	}
	out, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "sandboxbenchpod: %v\n", err)
	os.Exit(2)
}
