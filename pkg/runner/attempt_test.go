package runner

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"reflect"
	"testing"
)

// Mutation: remove SourceSnapshotFromTransport in loadWorkflow. The pod then
// executes the AST but loses the source required for its immutable attempt.
func TestLoadWorkflowPreservesAttemptSource(t *testing.T) {
	source := &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": "workflow w:\n  entry: done\n"}}
	body, err := runview.MarshalSourceSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := loadWorkflow(context.Background(), &queue.RunMessage{RunID: "attempt", WorkflowName: "w", IRCompiled: body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wf.SourceSnapshot, source) {
		t.Fatalf("source lost in queue hydration: %+v", wf.SourceSnapshot)
	}
}
