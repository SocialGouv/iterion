package runview

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A fork child carries its parent's launch-frozen credential frame — the
// four fields every resume replays beside the content boundary: the key
// pins, the routing policy, the pinned providers and the attached pool — on
// the repo-targeted arm and the local one alike. The resume half (the child
// seals the pinned gateway key) is
// cloudpublisher.TestFork_TheChildsResumeSealsThePinnedGatewayKey.
func TestFork_CarriesTheLaunchFrozenCredentialFrame(t *testing.T) {
	for _, row := range []struct {
		name    string
		repoURL string
	}{
		{"repo-targeted parent", "https://github.com/o/r.git"},
		{"local parent", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			logger := iterlog.Nop()
			st, err := store.New(dir, store.WithLogger(logger))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const parentID = "run-frame-parent"
			if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			parent, err := st.LoadRun(ctx, parentID)
			if err != nil {
				t.Fatal(err)
			}
			parent.RepoURL = row.repoURL
			parent.KeyOverrides = map[string]string{"openai_compatible": "key-pinned-K1"}
			parent.ContentBoundary = &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "key-pinned-K1", GatewayHost: "gw.example"}
			parent.LLMRoutePolicy = &store.RunLLMRoutePolicy{Providers: []string{"openai_compatible"}, PairOrder: []string{"claw+openai_compatible_key"}}
			parent.PinnedProviders = []string{"openai_compatible"}
			parent.PoolAttachment = &store.RunPoolAttachment{PoolID: "pool-1"}
			parent.Status = store.RunStatusCancelled
			parent.Checkpoint = &store.Checkpoint{NodeID: "step1", ArtifactsKnown: true}
			if err := st.SaveRun(ctx, parent); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
				RunID: parentID, NodeID: "step1", Backend: "claw", FinishReason: "tool_use",
				MessagesRef: "step1/0/0.messages.json",
				Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
				WrittenAt:   time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			svc, err := NewService(dir, WithLogger(logger))
			if err != nil {
				t.Fatal(err)
			}
			res, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "step1", TurnIndex: -1})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			child, err := st.LoadRun(ctx, res.NewRunID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(child.KeyOverrides, parent.KeyOverrides) {
				t.Errorf("child key_overrides = %v, want the parent's %v — the stamp names the pinned key", child.KeyOverrides, parent.KeyOverrides)
			}
			if !reflect.DeepEqual(child.LLMRoutePolicy, parent.LLMRoutePolicy) {
				t.Errorf("child llm_route_policy = %+v, want the parent's %+v", child.LLMRoutePolicy, parent.LLMRoutePolicy)
			}
			if !reflect.DeepEqual(child.PinnedProviders, parent.PinnedProviders) {
				t.Errorf("child pinned_providers = %v, want the parent's %v", child.PinnedProviders, parent.PinnedProviders)
			}
			if !reflect.DeepEqual(child.PoolAttachment, parent.PoolAttachment) {
				t.Errorf("child pool_attachment = %+v, want the parent's %+v", child.PoolAttachment, parent.PoolAttachment)
			}
		})
	}
}
