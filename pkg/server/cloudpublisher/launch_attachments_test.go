package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The staged uploads a cloud launch names are promoted on the SERVER,
// between the run row and the queue message: the runner pod has no
// staging area, and a run queued without its attachments started without
// them in silence. A promotion that fails fails the launch, the row marked
// LAUNCH_FAILED, nothing published.
func TestSubmitLaunchPromotesTheAttachmentsBetweenTheRowAndTheMessage(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	p := &Publisher{
		store:    st,
		identity: &fakeTeamResolver{orgs: map[string]string{"team-a": "org-1"}},
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			order = append(order, "publish")
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	promote := func(ctx context.Context, runID string) error {
		if _, err := st.LoadRun(ctx, runID); err != nil {
			return errors.New("the run row does not exist yet: " + err.Error())
		}
		order = append(order, "promote")
		return nil
	}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", AttachmentPromote: promote}
	if _, err := p.SubmitLaunch(ctx, "run-1", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if strings.Join(order, ",") != "promote,publish" {
		t.Fatalf("order = %v, want the promotion before the message", order)
	}

	order = nil
	failing := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", AttachmentPromote: func(context.Context, string) error {
		return errors.New(`upload "up_1_x" not found`)
	}}
	if _, err := p.SubmitLaunch(ctx, "run-2", failing, wf, &runview.CompiledSource{Hash: "hash"}); err == nil || !strings.Contains(err.Error(), "promote the launch's attachments") {
		t.Fatalf("err = %v, want the promotion failure", err)
	}
	if len(order) != 0 {
		t.Fatalf("a message was published after a failed promotion: %v", order)
	}
	r, err := st.LoadRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed || !strings.Contains(r.Error, "promote the launch's attachments") {
		t.Fatalf("run-2 = %s / %s / %q, want failed LAUNCH_FAILED naming the promotion", r.Status, r.FailureCode, r.Error)
	}
}
