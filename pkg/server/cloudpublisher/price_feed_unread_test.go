package cloudpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A price-feed fold that could not read a level refuses the launch and the
// resume alike, its error kept in the chain (the server's typed, retryable
// PriceFeedUnreadError answers through it): the launch publishes nothing and
// leaves no run document, the resume publishes nothing and puts the run back
// as it was. A fold that reads answers the bundle's table.
func TestAnUnreadPriceFeedFoldRefusesTheLaunchAndTheResume(t *testing.T) {
	const src = "workflow wf:\n  start -> done\n"
	unread := errors.New("price feed: platform unread")
	var failing atomic.Bool
	p, buf, rs, published, _ := replayPublisher(t)
	p.priceFeedEffective = func(context.Context, string, string, []string) (json.RawMessage, error) {
		if failing.Load() {
			return nil, unread
		}
		return json.RawMessage(`{"entries":{}}`), nil
	}
	wf := func() *ir.Workflow { return &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes} }
	ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", "owner1"), gwRepo)
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: src, BotID: gwBot, ProjectPath: gwRepo}

	failing.Store(true)
	_, err := p.SubmitLaunch(ctx, "run-pf-unread", spec, wf(), &runview.CompiledSource{Hash: "hash"})
	if !errors.Is(err, unread) || len(*published) != 0 {
		t.Fatalf("launch = %v with %d published, want the fold's error and nothing published (log:\n%s)", err, len(*published), buf.String())
	}
	if runview.RunMayHaveStarted(err) {
		t.Errorf("the refused launch reads as a run that may have started: %v", err)
	}
	if run, lerr := rs.LoadRun(ctx, "run-pf-unread"); lerr == nil && run != nil {
		t.Errorf("the refused launch left a run document (status %s)", run.Status)
	}

	failing.Store(false)
	if _, err := p.SubmitLaunch(ctx, "run-pf-resume", spec, wf(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("launch over a fold that reads: %v (log:\n%s)", err, buf.String())
	}
	if len(*published) != 1 || string(messageBundle(t, p, (*published)[0], "run-pf-resume").GatewayPriceFeed) != `{"entries":{}}` {
		t.Fatalf("the launch over a fold that reads did not seal its table (%d published)", len(*published))
	}
	pauseForResume(t, rs, ctx, "run-pf-resume", "implement")
	failing.Store(true)
	err = p.SubmitResume(ctx, runview.ResumeSpec{RunID: "run-pf-resume", FilePath: "wf.bot", Source: src}, wf(), &runview.CompiledSource{Hash: "hash"})
	if !errors.Is(err, unread) || len(*published) != 1 {
		t.Fatalf("resume = %v with %d published, want the fold's error and nothing more published (log:\n%s)", err, len(*published), buf.String())
	}
	if run, lerr := rs.LoadRun(ctx, "run-pf-resume"); lerr != nil || run.Status != store.RunStatusFailedResumable {
		t.Errorf("the refused resume left the run %+v (%v), want it back as failed_resumable", run, lerr)
	}
}
