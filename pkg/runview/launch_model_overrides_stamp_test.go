package runview

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An in-process launch stamps its own rows on the run document twice: the
// folded list it executes with (ModelOverrides, the policy's cells
// included) and the raw rows the launch body carried
// (LaunchModelOverrides), which the subbot children of its tree fold again
// for their own bot. A policy cell never lands among the raw rows.
func TestLaunchInProcessStampsTheRawLaunchRows(t *testing.T) {
	repo := gittest.SourceRepo(t)
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithWorkDir(repo))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath:       "plain.bot",
		Source:         toolOnlyBot,
		ModelOverrides: []ModelOverrideEntry{{Selector: "noop", Model: "openai/gpt-6-sol"}},
		LLMRoutePolicy: &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "*/other", Model: "openai/gpt-5.5-mini"}}},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	<-res.Done
	run, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	raw := []store.RunModelOverride{{Selector: "noop", Model: "openai/gpt-6-sol"}}
	if !reflect.DeepEqual(run.LaunchModelOverrides, raw) {
		t.Fatalf("run launch rows = %+v, want the launch body's raw rows %+v", run.LaunchModelOverrides, raw)
	}
	folded := []store.RunModelOverride{{Selector: "other", Model: "openai/gpt-5.5-mini"}, {Selector: "noop", Model: "openai/gpt-6-sol"}}
	if !reflect.DeepEqual(run.ModelOverrides, folded) {
		t.Fatalf("run override rows = %+v, want the fold %+v", run.ModelOverrides, folded)
	}
}
