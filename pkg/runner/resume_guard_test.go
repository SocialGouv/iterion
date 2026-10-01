package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scrubThroughRunner builds the pod's executor for msg — the real
// executorSpec, whose Store is the metrics wrapper — over a store where
// recordID holds a minted grant, and returns what its guard makes of a
// command rendered with that grant.
func scrubThroughRunner(t *testing.T, msg *queue.RunMessage, recordID string) (scrubbed, grant string) {
	t.Helper()
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) {
			t.Setenv(name, "")
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	grant = hex.EncodeToString(b)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, recordID, "wf", map[string]any{store.ForgePublishTokenVar: grant, "pr_url": "https://forge.invalid/o/r/pull/1"}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	exec, _, err := r.buildExecutor(ctx, msg, &ir.Workflow{Name: "wf"}, iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	claw, ok := exec.(*model.ClawExecutor)
	if !ok {
		t.Fatalf("executor is %T", exec)
	}
	out := claw.ScrubOutput(map[string]any{"cmd": "PUB_TOKEN=" + grant + " python3 publish.py"})
	return fmt.Sprint(out["cmd"]), grant
}

// A resume message carries no launch vars; the pod's guard reads the grant
// from the run's record, through the store the metrics wrapper hides.
func TestRunnerResumeGuardRedactsTheRecordsGrant(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-resume", WorkflowName: "wf", Resume: &queue.ResumeSpec{}}
	got, grant := scrubThroughRunner(t, msg, msg.RunID)
	if strings.Contains(got, grant) {
		t.Fatalf("a resumed run's guard does not know its grant: %q", got)
	}
}

// A sub-bot child has no record yet and may receive the grant under another
// name; its guard knows the parent's grant by value.
func TestRunnerSubbotChildGuardRedactsItsParentsGrant(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-child", ParentRunID: "run-parent", WorkflowName: "wf", Vars: map[string]any{"reviewer": "x"}}
	got, grant := scrubThroughRunner(t, msg, msg.ParentRunID)
	if strings.Contains(got, grant) {
		t.Fatalf("a sub-bot child's guard does not know its parent's grant: %q", got)
	}
}
