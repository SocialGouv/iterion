package runview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRunStatus_QueuedString validates the new lifecycle state added for
// cloud-mode submission. A typo or accidental rename here breaks the
// editor + the server's queue-position handler at once.
func TestRunStatus_QueuedString(t *testing.T) {
	if string(store.RunStatusQueued) != "queued" {
		t.Errorf("RunStatusQueued = %q want %q", store.RunStatusQueued, "queued")
	}
}

func TestRunSummary_OmitsQueuePositionWhenNil(t *testing.T) {
	rs := RunSummary{
		ID:           "run_1",
		WorkflowName: "demo",
		Status:       store.RunStatusRunning,
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
		UpdatedAt:    time.Unix(1700000000, 0).UTC(),
	}
	body, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "queue_position") {
		t.Errorf("queue_position must be omitempty when nil: %s", body)
	}
}

func TestRunSummary_IncludesQueuePositionWhenSet(t *testing.T) {
	pos := 3
	rs := RunSummary{
		ID:            "run_1",
		WorkflowName:  "demo",
		Status:        store.RunStatusQueued,
		CreatedAt:     time.Unix(1700000000, 0).UTC(),
		UpdatedAt:     time.Unix(1700000000, 0).UTC(),
		QueuePosition: &pos,
	}
	body, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"queue_position":3`) {
		t.Errorf("expected queue_position:3 in %s", body)
	}
}
