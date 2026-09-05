package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// podStartState is what the cluster says about a pod that never reached
// Ready. Read ONCE on the wait's error path and used twice: to decorate
// the error with the scheduling reason an operator needs, and to decide
// whether the failure is a placement the run may retry.
type podStartState struct {
	// Readable is false when the pod could not be inspected at all (RBAC,
	// an apiserver blip, the pod already gone). Nothing is then claimed
	// about the failure — no evidence, no classification.
	Readable bool
	// Phase is `.status.phase` (Pending, Running, …).
	Phase string
	// Scheduled mirrors the PodScheduled condition being True: the
	// scheduler found a node for this pod.
	Scheduled bool
	// ScheduledReason / ScheduledMessage carry the PodScheduled
	// condition's reason ("Unschedulable") and its message ("0/12 nodes
	// are available: … Insufficient cpu"), which is the operator-facing
	// evidence.
	ScheduledReason  string
	ScheduledMessage string
	// WaitingReasons are the `state.waiting.reason` of every container and
	// init container — where a broken image reference or an invalid spec
	// shows up.
	WaitingReasons []string
}

// terminalWaitingReasons name container states a redelivery re-hits
// identically: the image reference does not resolve, the spec cannot
// build a container, the container starts and dies. Retrying spends a pod
// per delivery and ends on the DLQ with the same cause, so these stay
// terminal failures the operator has to fix.
//
// A slow pull is deliberately NOT here: while the kubelet is pulling, the
// waiting reason is `ContainerCreating` — `ErrImagePull` and its backoff
// only appear once the pull has actually failed.
var terminalWaitingReasons = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"ImageInspectError":          true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
	"CrashLoopBackOff":           true,
}

// podPhasePending is Kubernetes' own guarantee that nothing of the run
// executed: the pod was accepted but at least one container has not been
// created yet. It is the evidence the resumable classification rests on,
// stronger than enumerating waiting reasons (a pod whose kubelet has not
// reported any container status yet carries none at all).
const podPhasePending = "Pending"

// classifyPodStart decides whether a pod that never reached Ready failed
// on PLACEMENT — resumable, because the run executed nothing and a later
// attempt on a cluster with room does the whole thing from scratch — or
// on something a redelivery would re-hit.
//
// It answers `true` only on POSITIVE evidence of a placement failure. No
// evidence, or evidence of anything else, keeps the historical terminal
// classification: "everything at sandbox start is resumable" would turn a
// mistyped image into eight pods and a DLQ park.
//
// The rule is the pod's PHASE: still `Pending` past the deadline means no
// container ever started — the scheduler found no node (`Unschedulable` /
// `Insufficient cpu`, what a full fleet produces), or the node it landed
// on was still bringing it up (CNI, volumes, image pull). `Running` past
// the deadline is a container that came up and failed its readiness, and
// `Unknown` is a node iterion cannot see: neither says the run did
// nothing, so both stay terminal.
//
// A terminal container reason OVERRIDES the phase: a pod sits `Pending`
// with `ImagePullBackOff` too, and there the image reference is what the
// operator must fix — retrying it spends a pod per delivery to re-learn
// the same thing.
func classifyPodStart(st podStartState) (resumable bool, why string) {
	if !st.Readable {
		return false, ""
	}
	for _, r := range st.WaitingReasons {
		if terminalWaitingReasons[r] {
			return false, ""
		}
	}
	if st.Phase != podPhasePending {
		return false, ""
	}
	if !st.Scheduled {
		return true, "the pod was never scheduled" + reasonSuffix(st.ScheduledReason, st.ScheduledMessage)
	}
	return true, "the pod was scheduled but its node had not started the container" +
		reasonSuffix(strings.Join(st.WaitingReasons, ", "), "")
}

func reasonSuffix(reason, message string) string {
	parts := make([]string, 0, 2)
	if r := strings.TrimSpace(reason); r != "" {
		parts = append(parts, r)
	}
	if m := strings.TrimSpace(message); m != "" {
		parts = append(parts, m)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ": ") + ")"
}

// schedulingSuffix renders the PodScheduled condition as the
// "\nscheduling: …" tail of a wait error — "" when the pod could not be
// read. `kubectl wait` only says the condition never came; the cluster
// knows WHY, and the reason decides between two different fixes.
func (st podStartState) schedulingSuffix() string {
	if !st.Readable {
		return ""
	}
	status := "False"
	if st.Scheduled {
		status = "True"
	}
	line := status
	if r := strings.TrimSpace(st.ScheduledReason); r != "" {
		line += " " + r
	}
	if m := strings.TrimSpace(st.ScheduledMessage); m != "" {
		line += ": " + m
	}
	return "\nscheduling: " + line
}

// podStatusJSON is the slice of a Pod's status this package reads.
type podStatusJSON struct {
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses     []podContainerStatusJSON `json:"containerStatuses"`
		InitContainerStatuses []podContainerStatusJSON `json:"initContainerStatuses"`
	} `json:"status"`
}

type podContainerStatusJSON struct {
	State struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	} `json:"state"`
}

// probePodStart reads the pod's status once. Only `get pod` is needed —
// the Events the autoscaler writes (`TriggeredScaleUp`) would say the
// same thing about capacity but need an RBAC verb the runner's namespaced
// Role deliberately does not grant.
func probePodStart(ctx context.Context, namespace, podName string) podStartState {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace, "get", "pod", podName, "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return podStartState{}
	}
	var doc podStatusJSON
	if err := json.Unmarshal(out, &doc); err != nil {
		return podStartState{}
	}
	st := podStartState{Readable: true, Phase: doc.Status.Phase}
	for _, c := range doc.Status.Conditions {
		if c.Type != "PodScheduled" {
			continue
		}
		st.Scheduled = c.Status == "True"
		st.ScheduledReason = c.Reason
		st.ScheduledMessage = c.Message
	}
	for _, cs := range append(append([]podContainerStatusJSON{}, doc.Status.InitContainerStatuses...), doc.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			st.WaitingReasons = append(st.WaitingReasons, cs.State.Waiting.Reason)
		}
	}
	return st
}

// podStartFailure wraps the raw `kubectl wait` error with what the cluster
// showed, and — when that evidence says the pod never got PLACED — with
// sandbox.ErrCapacity, which the runtime maps to failed_resumable +
// SANDBOX_CAPACITY and the runner re-offers after a delay.
//
// The sentinel rides errors.Join like the driver's phase timeout does, so
// the underlying cause stays reachable through errors.Is/As alongside it.
func podStartFailure(waitErr error, st podStartState) error {
	err := fmt.Errorf("%w%s", waitErr, st.schedulingSuffix())
	resumable, why := classifyPodStart(st)
	if !resumable {
		return err
	}
	return fmt.Errorf("sandbox not placed — %s: %w", why, errors.Join(sandbox.ErrCapacity, err))
}
