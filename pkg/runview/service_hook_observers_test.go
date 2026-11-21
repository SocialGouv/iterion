package runview

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestHookEventObserversReachTheBroker(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-hook-obs"
	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	ctx := t.Context()
	if _, err := svc.RunStore().CreateRun(ctx, runID, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	var extraSeen bool
	hooks := model.NewStoreEventHooksWithBroker(ctx, svc.RunStore(), runID, iterlog.Nop(), nil, nil, svc.broker.Publish, func(store.Event) { extraSeen = true })
	hooks.OnAssistantText("campaign", model.AssistantTextInfo{Text: "fixture"})
	if !extraSeen {
		t.Error("extra observer not invoked")
	}
	// The observers ran synchronously above, and the broker's own fan-out is a
	// non-blocking send by its caller, so a delivered event is already
	// buffered: read it without a clock.
	select {
	case got := <-sub.C:
		if got.Type != store.EventAssistantText {
			t.Fatalf("broker got %+v; want the published assistant_text", got)
		}
	default:
		t.Fatal("broker subscriber never received the hook event")
	}
}
