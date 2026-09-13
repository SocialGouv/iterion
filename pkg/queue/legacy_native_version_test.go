package queue_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

func TestOldQueueExecutableRejectsNativeBeforeStoreAccess(t *testing.T) {
	probe := storetest.LegacyTool(t, "ITERION_TEST_LEGACY_PROBE")
	message := queue.RunMessage{V: queue.SchemaVersion, RunID: "pc1_old_queue", RuntimeSemantics: "ports-v1", WorkflowName: "native", IRCompiled: json.RawMessage(`{}`), TenantID: "team"}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	output := storetest.RunLegacyTool(t, probe, gittest.SourceRepo(t), false, "-action", "queue-check", "-message", string(body))
	if !strings.Contains(output, "schema version") || !strings.Contains(output, "unsupported") {
		t.Fatalf("supported old queue executable did not reject v15 before store construction: %s", output)
	}
}
