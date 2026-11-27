package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestR2WarningOmitsValuesAndStrictEnvRejects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, strict := range []string{"", "true"} {
		t.Setenv(EnvOutputValidation, strict)
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "'private-output-sentinel'")
		wf.Nodes["gate"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, SchemaFields: ir.SchemaFields{OutputSchema: "gauge"}}
		exec := newStubExecutor()
		exec.on("gate", func(map[string]any) (map[string]any, error) {
			return map[string]any{"used_pct": "private-output-sentinel"}, nil
		})
		st := tmpStore(t)
		err := New(wf, st, exec).Run(context.Background(), "warning", nil)
		if strict == "true" {
			var typed *RuntimeError
			if !errors.As(err, &typed) || typed.Code != ErrCodeToolFailedPermanent {
				t.Fatalf("strict env accepted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.LoadEvents(context.Background(), "warning")
		if err != nil {
			t.Fatal(err)
		}
		warnings := 0
		for _, event := range events {
			if event.Type == store.EventNodeOutputSchemaMismatch {
				warnings++
				raw, _ := json.Marshal(event.Data)
				if strings.Contains(string(raw), "private-output-sentinel") || !strings.Contains(string(raw), "used_pct") {
					t.Errorf("unsafe or incomplete warning: %s", raw)
				}
			}
		}
		if warnings != 1 {
			t.Errorf("warnings=%d", warnings)
		}
	}
}
