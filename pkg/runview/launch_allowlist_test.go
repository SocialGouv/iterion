package runview

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const pinnedOpenAIBot = `
agent work:
  backend: "claw"
  model: "openai/gpt-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow locked:
  entry: work
  work -> done
`

const toolOnlyBot = `
tool noop:
  command: "true"

workflow plain:
  entry: noop
  noop -> done
`

func teamProviders(list ...string) *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{Providers: list, Sources: map[string]string{"providers": "team"}}
}

// The in-process lane holds the tenant's providers lock (#2665 item 7): a
// primary route outside the list refuses the launch naming node, provider
// and the level that set the list — the cloud publisher's refusal, read on
// the local lane. Mutant: the gate call dropped from startInProcess → the
// launch proceeds and this reddens.
func TestLaunchInProcessRefusesAPrimaryRouteOutsideTheProvidersList(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath:       "locked.bot",
		Source:         pinnedOpenAIBot,
		LLMRoutePolicy: teamProviders("anthropic"),
	})
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("in-process launch outside the list: err = %v, want the outside-allowlist refusal", err)
	}
	if outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
		t.Fatalf("refusal = site %q provider %q level %q, want work/openai/team", outside.Site, outside.Provider, outside.Level)
	}
}

// A policy whose list names no provider, and a CLI-shaped launch with no
// policy at all, pass the gate untouched.
func TestLaunchInProcessPassesWithoutAList(t *testing.T) {
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}}
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Backend, agent.Model = "claw", "openai/gpt-5"
	wf.Nodes["work"] = agent
	for name, pol := range map[string]*store.RunLLMRoutePolicy{"nil policy": nil, "no list": {Sources: map[string]string{}}} {
		if err := refuseOutsideAllowlist(wf, &LaunchSpec{LLMRoutePolicy: pol}); err != nil {
			t.Errorf("%s: refused %v, want the gate to pass", name, err)
		}
	}
}

// The run's frozen list reaches the resumed executor the way the cloud
// runner hands it to its own (RunMessage.LLMProviders → ExecutorSpec):
// a local resume reads it back from the run document. Mutant: the resume
// threading dropped → the spec carries no list and this reddens.
func TestResumeExecutorSpecThreadsTheRunsProviderAllowlist(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	run := &store.Run{ID: "r1", LLMRoutePolicy: teamProviders("anthropic", "zai")}
	spec := svc.resumeExecutorSpec(&ir.Workflow{Name: "w"}, run, iterlog.Nop(), "")
	if !reflect.DeepEqual(spec.LLMProviders, []string{"anthropic", "zai"}) {
		t.Fatalf("resume spec LLMProviders = %v, want the run document's list", spec.LLMProviders)
	}
	if spec = svc.resumeExecutorSpec(&ir.Workflow{Name: "w"}, &store.Run{ID: "r2"}, iterlog.Nop(), ""); spec.LLMProviders != nil {
		t.Fatalf("a run with no policy threads a list: %v", spec.LLMProviders)
	}
}

// The in-process lane stamps the launch's resolved policy on its run
// document, so the resume above has something to replay. Mutant: the stamp
// dropped → the loaded document carries no policy and this reddens.
func TestLaunchInProcessStampsTheRoutePolicyOnTheRunDocument(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath:       "plain.bot",
		Source:         toolOnlyBot,
		LLMRoutePolicy: teamProviders("anthropic"),
	})
	if err != nil {
		t.Fatalf("tool-only launch under a list: %v", err)
	}
	run, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.LLMRoutePolicy == nil || !reflect.DeepEqual(run.LLMRoutePolicy.Providers, []string{"anthropic"}) {
		t.Fatalf("run document policy = %+v, want the launch's list stamped", run.LLMRoutePolicy)
	}
}
