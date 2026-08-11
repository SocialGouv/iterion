package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRecordWorkspaceReset pins the timeline marker for the one state a
// re-executing repo-backed run cannot recover: its working tree.
//
// prepareRepoWorkspace deletes and re-clones the per-run repo dir on every
// claim, and executeRun deletes it again when the run returns. So a run that
// dies between two nodes comes back on a tree that kept nothing — while the
// checkpoint faithfully replays the earlier node's outputs. A downstream node
// then reads "the previous node edited these files" against a tree where they
// no longer exist, and used to have no way to know.
//
// That happened on SocialGouv/iterion#400: Vetty's `align` node wrote an otel
// fix, the run hit a provider usage window, the retry re-cloned, and the fix
// was gone. Nothing anywhere said so.
func TestRecordWorkspaceReset(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{
		RunID:   "019fecf3-bdd4-70ef-8445-9bfba275f6c4",
		RepoURL: "https://github.com/SocialGouv/iterion",
		RepoSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
	}

	r.recordWorkspaceReset(context.Background(), msg, "resume")

	events, err := st.LoadEvents(context.Background(), msg.RunID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var found *store.Event
	for i := range events {
		if events[i].Type == store.EventRunWorkspaceReset {
			found = events[i]
		}
	}
	if found == nil {
		t.Fatalf("no %s event on the timeline: a discarded working tree stays invisible", store.EventRunWorkspaceReset)
	}
	if got := found.Data["reason"]; got != "resume" {
		t.Errorf("reason = %v, want \"resume\"", got)
	}
	// The ref matters: it is what the tree was re-anchored on, so it is how an
	// operator tells "re-cloned at the same head" from "re-cloned at a head
	// that moved under the run".
	if got := found.Data["repo_sha"]; got != msg.RepoSHA {
		t.Errorf("repo_sha = %v, want %q", got, msg.RepoSHA)
	}
}

// failingAppend is a store whose event append always fails. Everything else is
// the embedded real store, so only the one call under test degrades.
type failingAppend struct {
	store.RunStore
}

func (failingAppend) AppendEvent(context.Context, string, store.Event) (*store.Event, error) {
	return nil, errors.New("mongo: connection refused")
}

// A store that refuses the append must not sink the run — the marker is
// observational, and the run is already committed to re-executing.
//
// But "not fatal" must not mean "not reported": the append failing is EXACTLY
// the case where the timeline will lack the fact, so the pod log has to carry
// it. This asserts both lines, because a version that swallowed one would
// otherwise pass while leaving the loss as invisible as before the fix.
func TestRecordWorkspaceReset_storeFailureStillReportsToTheLog(t *testing.T) {
	real, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var logged strings.Builder
	r := &Runner{cfg: Config{
		Store:  failingAppend{RunStore: real},
		Logger: iterlog.New(iterlog.LevelWarn, &logged),
	}}

	r.recordWorkspaceReset(context.Background(), &queue.RunMessage{
		RunID:   "run-1",
		RepoSHA: "ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a",
	}, "resume")

	out := logged.String()
	for _, want := range []string{
		"FRESH clone",                              // the fact itself
		"could not emit run_workspace_reset",       // and that the timeline will not have it
		"ec2ff4b9e54e61f28f2cd43cd8281e18a5e0af0a", // anchored on something identifiable
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not carry %q — the loss is silent again:\n%s", want, out)
		}
	}
}

// TestReExecutionReason covers the predicate that decides whether the marker is
// warranted, and which fact it names. A first claim has nothing to lose; every later one does.
//
// The `running` case is the one `msg.Resume != nil` alone gets wrong: a
// JetStream redelivery of a run whose pod died inside the orphan sweeper's
// window arrives with Resume nil, re-clones all the same, and would have
// discarded an earlier node's work with no marker at all.
func TestReExecutionReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resume     *queue.ResumeSpec
		checkpoint *store.Checkpoint
		want       string
	}{
		{name: "first claim: nothing has run yet", want: ""},
		{name: "explicit resume publish", resume: &queue.ResumeSpec{}, want: "resume"},
		{name: "redelivery with a checkpoint, no resume spec", checkpoint: &store.Checkpoint{NodeID: "commit"}, want: "redelivery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			ctx := context.Background()
			run, err := st.CreateRun(ctx, "run-1", "dep_update_guard", nil)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if tc.checkpoint != nil {
				if err := st.SaveCheckpoint(ctx, run.ID, tc.checkpoint); err != nil {
					t.Fatalf("SaveCheckpoint: %v", err)
				}
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			got := r.reExecutionReason(ctx, &queue.RunMessage{RunID: run.ID, Resume: tc.resume})
			if got != tc.want {
				t.Errorf("reExecutionReason = %q, want %q — a marker that mislabels which fact it saw is worse than none", got, tc.want)
			}
		})
	}
}
