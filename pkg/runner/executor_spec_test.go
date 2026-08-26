package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pod's executor must bind the inbox + async-ask hooks: supervisor
// steering and operator chat are appended as store.QueuedUserMessage and
// only an InboxBinder delivers them into the agent's live turn. The pod
// was the one launch surface without them — supervisors evaluated and
// injected on cloud runs while nothing ever drained (found by the Persy
// cloud dogfood, run 01a03d70).
func TestExecutorSpecBindsInboxAndAsyncAsk(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-spec", WorkflowName: "wf"}

	spec, usage, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if usage == nil {
		t.Fatal("metrics emitter missing")
	}
	if spec.Inbox == nil {
		t.Fatal("ExecutorSpec.Inbox not bound — queued supervisor/operator messages would never deliver on a pod")
	}
	if spec.AsyncAsk == nil {
		t.Fatal("ExecutorSpec.AsyncAsk not bound — ask_user_async would have no store on a pod")
	}
	if spec.Inbox.Bind(context.Background(), msg.RunID) == nil {
		t.Fatal("inbox binder refuses to bind for the run — store not wired through")
	}
}
