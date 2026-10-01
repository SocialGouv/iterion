package runview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

const publishGrantSentinel = "sentinel-publish-token-1997"

// Every run view — GET /api/runs/{id}, the WS snapshot and its refresh, the
// remote CLI and MCP reading them — is built from headerFromRun. The grant
// the run holds in its inputs and its checkpoint vars must not ride it out,
// while the run record the engine resumes from keeps it.
func TestSnapshotHeader_MasksThePublishGrantAndLeavesTheRunIntact(t *testing.T) {
	run := &store.Run{
		ID:           "run-grant",
		WorkflowName: "review",
		Status:       store.RunStatusPausedWaitingHuman,
		Inputs: map[string]any{
			"pr_url":                   "https://github.com/o/r/pull/7",
			store.ForgePublishTokenVar: publishGrantSentinel,
		},
		Checkpoint: &store.Checkpoint{
			NodeID: "approve",
			Vars:   map[string]any{store.ForgePublishTokenVar: publishGrantSentinel},
		},
	}
	check := func(t *testing.T, snap *RunSnapshot) {
		t.Helper()
		body, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), publishGrantSentinel) {
			t.Fatalf("the snapshot carries the publish grant: %s", body)
		}
		if got := snap.Run.Inputs[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
			t.Errorf("inputs.%s = %v, want %q", store.ForgePublishTokenVar, got, store.RedactedLaunchVar)
		}
		if snap.Run.Inputs["pr_url"] != "https://github.com/o/r/pull/7" {
			t.Errorf("inputs lost pr_url: %v", snap.Run.Inputs)
		}
		if snap.Run.Checkpoint == nil || snap.Run.Checkpoint.Vars[store.ForgePublishTokenVar] != store.RedactedLaunchVar {
			t.Errorf("checkpoint = %+v, want its vars masked", snap.Run.Checkpoint)
		}
		if snap.Run.Checkpoint != nil && snap.Run.Checkpoint.NodeID != "approve" {
			t.Errorf("checkpoint node = %q, want approve", snap.Run.Checkpoint.NodeID)
		}
	}

	b := NewSnapshotBuilder(run)
	t.Run("the first snapshot", func(t *testing.T) { check(t, b.Snapshot()) })
	b.SetRun(run)
	t.Run("the snapshot refreshed from run.json", func(t *testing.T) { check(t, b.Snapshot()) })

	if run.Inputs[store.ForgePublishTokenVar] != publishGrantSentinel {
		t.Fatalf("the run record's input grant became %v", run.Inputs[store.ForgePublishTokenVar])
	}
	if run.Checkpoint.Vars[store.ForgePublishTokenVar] != publishGrantSentinel {
		t.Fatalf("the run record's checkpoint grant became %v", run.Checkpoint.Vars[store.ForgePublishTokenVar])
	}
}

// The fork dialog pre-fills its editor with the inputs the run view showed,
// grant masked, and submits them back with the operator's edits. Whatever
// arrives under the grant's name — the mask, a stale token — is not the grant:
// the child keeps the parent's from the record, and the real edits apply.
func TestFork_ASentBackGrantNeverReplacesTheParentsGrant(t *testing.T) {
	parentInputs := func() map[string]any {
		return map[string]any{"mode": "fast", "target": "repo", store.ForgePublishTokenVar: publishGrantSentinel}
	}
	loadChild := func(t *testing.T, svc *Service, res *ForkResult) *store.Run {
		t.Helper()
		child, err := svc.store.LoadRun(context.Background(), res.NewRunID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		return child
	}

	for _, sent := range []string{store.RedactedLaunchVar, "stale-publish-token"} {
		t.Run("sent "+sent, func(t *testing.T) {
			svc, id := seedForkGateParent(t, singleFile(), parentInputs())
			res, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{
				"mode": "slow", "target": "repo", store.ForgePublishTokenVar: sent,
			}})
			if err != nil {
				t.Fatalf("fork: %v", err)
			}
			child := loadChild(t, svc, res)
			if got := child.Inputs[store.ForgePublishTokenVar]; got != publishGrantSentinel {
				t.Fatalf("child %s = %v, want the parent's grant — the child would publish with %q", store.ForgePublishTokenVar, got, sent)
			}
			if child.Inputs["mode"] != "slow" || child.Inputs["target"] != "repo" {
				t.Errorf("the operator's edits did not apply: %v", child.Inputs)
			}
		})
	}

	// The grant and its endpoints are one binding: an operator who edits the
	// endpoint while the child keeps the parent's live grant would point that
	// grant at a host of their choosing.
	t.Run("an edited endpoint never carries the parent's grant elsewhere", func(t *testing.T) {
		inputs := parentInputs()
		inputs[store.ForgePublishURLVar] = "https://iterion.example/api/v1/forge/publish-review"
		svc, id := seedForkGateParent(t, singleFile(), inputs)
		res, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{
			"mode": "fast", "target": "repo", store.ForgePublishTokenVar: store.RedactedLaunchVar,
			store.ForgePublishURLVar: "https://collector.example/publish",
		}})
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		child := loadChild(t, svc, res)
		if got := child.Inputs[store.ForgePublishURLVar]; got != "https://iterion.example/api/v1/forge/publish-review" {
			t.Fatalf("child %s = %v, want the parent's endpoint — the parent's grant would be presented there", store.ForgePublishURLVar, got)
		}
		if got := child.Inputs[store.ForgePublishTokenVar]; got != publishGrantSentinel {
			t.Fatalf("child %s = %v, want the parent's grant", store.ForgePublishTokenVar, got)
		}
	})

	// A parent that recorded no source refuses every CHANGED input as
	// unverifiable. The mask differs from the stored grant, so reading it as
	// a change would refuse the dialog's unedited resubmit — the recovery
	// fork an operator reaches for by default.
	t.Run("a source-less parent still forks on the dialog's unedited resubmit", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, parentInputs())
		res, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{
			"mode": "fast", "target": "repo", store.ForgePublishTokenVar: store.RedactedLaunchVar,
		}})
		if err != nil {
			t.Fatalf("the unedited resubmit was refused: %v", err)
		}
		if got := loadChild(t, svc, res).Inputs[store.ForgePublishTokenVar]; got != publishGrantSentinel {
			t.Fatalf("child %s = %v, want the parent's grant", store.ForgePublishTokenVar, got)
		}
	})
}
