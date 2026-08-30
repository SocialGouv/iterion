package mongo

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// decodeLikeCursor round-trips a value through the exact encode+decode
// pair the store's collections use: driver marshal (the wire bytes
// InsertOne sends — Registry overrides no encoders) and a bson.Decoder
// over the raw document with the client's registry installed, which is
// Cursor.Decode's construction (mongo-driver cursor.go getDecoder).
// No live server is involved because none is needed: BSON bytes → Go
// types is decided entirely client-side by the registry under test.
func decodeLikeCursor(t *testing.T, in, out any) {
	t.Helper()
	raw, err := bson.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.SetRegistry(Registry())
	if err := dec.Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestRunFallbackBSONAcceptsObjectAndArray(t *testing.T) {
	legacy := bson.D{
		{Key: "_id", Value: "legacy-fallback"},
		{Key: "fallback", Value: bson.D{
			{Key: "backend", Value: "codex"},
			{Key: "model", Value: "gpt-5.5"},
		}},
	}
	var fromObject store.Run
	decodeLikeCursor(t, legacy, &fromObject)
	if len(fromObject.Fallback) != 1 || fromObject.Fallback[0].Backend != "codex" || fromObject.Fallback[0].Model != "gpt-5.5" {
		t.Fatalf("legacy BSON fallback = %+v, want one promoted stage", fromObject.Fallback)
	}

	canonical := store.Run{
		ID: "array-fallback",
		Fallback: store.RunFallback{
			{Backend: "codex", Model: "gpt-5.5"},
			{Backend: "claw", Model: "openai/gpt-5.5"},
		},
	}
	var fromArray store.Run
	decodeLikeCursor(t, canonical, &fromArray)
	if len(fromArray.Fallback) != 2 || fromArray.Fallback[1].Backend != "claw" {
		t.Fatalf("array BSON fallback = %+v, want two ordered stages", fromArray.Fallback)
	}
	raw, err := bson.Marshal(canonical)
	if err != nil {
		t.Fatalf("marshal canonical run: %v", err)
	}
	if got := bson.Raw(raw).Lookup("fallback").Type; got != bson.TypeArray {
		t.Fatalf("canonical BSON fallback type = %s, want array", got)
	}
}

// TestEventDataDecodeShape pins the store's decode contract for the
// open-shaped Event.Data payload: nested documents surface as plain
// map[string]any, nested arrays as plain []any, and int32-width wire
// integers as int64. These are the only shapes the consumers shared
// with the filesystem store accept — runview snapshot reducers, subbot
// terminal-output recovery, checkpoint reference resolution, expr
// evaluation, fan-out iteration all type-assert map[string]any /
// []any / the int-int64-float64 family and silently produce nothing on
// anything else. The driver's default registry instead yields bson.D /
// bson.A / int32 — defined types that fail every one of those
// assertions.
func TestEventDataDecodeShape(t *testing.T) {
	in := store.Event{
		Seq:    7,
		Type:   store.EventNodeFinished,
		RunID:  "r-decode-shape",
		NodeID: "deploy",
		Data: map[string]any{
			"output": map[string]any{
				"_backend":     "claude_code",
				"_model":       "claude-opus-4-8",
				"deployed_url": "https://app.example.test",
				"healthy":      true,
				"steps":        []any{map[string]any{"name": "build", "ok": true}},
			},
			"loops":     map[string]any{"fix": 5},
			"iteration": 3,
		},
	}
	var out store.Event
	decodeLikeCursor(t, in, &out)

	output, ok := out.Data["output"].(map[string]any)
	if !ok {
		t.Fatalf("Data[output] decoded as %T, want map[string]any", out.Data["output"])
	}
	if got, _ := output["_backend"].(string); got != "claude_code" {
		t.Errorf("output[_backend] = %v (%T), want claude_code", output["_backend"], output["_backend"])
	}
	steps, ok := output["steps"].([]any)
	if !ok {
		t.Fatalf("output[steps] decoded as %T, want []any", output["steps"])
	}
	if _, ok := steps[0].(map[string]any); !ok {
		t.Fatalf("steps[0] decoded as %T, want map[string]any", steps[0])
	}
	loops, ok := out.Data["loops"].(map[string]any)
	if !ok {
		t.Fatalf("Data[loops] decoded as %T, want map[string]any", out.Data["loops"])
	}
	if v, ok := loops["fix"].(int64); !ok || v != 5 {
		t.Fatalf("loops[fix] decoded as %T (%v), want int64(5)", loops["fix"], loops["fix"])
	}
	if v, ok := out.Data["iteration"].(int64); !ok || v != 3 {
		t.Fatalf("Data[iteration] decoded as %T (%v), want int64(3)", out.Data["iteration"], out.Data["iteration"])
	}
}

// TestCheckpointOutputsDecodeShape is the run-document twin: values
// nested below Checkpoint.Outputs' typed two-map levels must decode to
// plain shapes too — the runner resumes cloud runs from this document,
// and reference resolution ({{outputs.node.field.sub}}), expr
// evaluation, and fan-out `each` iteration all walk these values with
// map[string]any / []any assertions.
func TestCheckpointOutputsDecodeShape(t *testing.T) {
	in := store.Run{
		ID:           "r-ckpt-shape",
		WorkflowName: "wf",
		Checkpoint: &store.Checkpoint{
			NodeID: "plan",
			Outputs: map[string]map[string]any{
				"plan": {
					"items":  []any{map[string]any{"id": "a", "weight": 2}},
					"detail": map[string]any{"nested": map[string]any{"deep": true}},
				},
			},
		},
	}
	var out store.Run
	decodeLikeCursor(t, in, &out)

	planOut := out.Checkpoint.Outputs["plan"]
	items, ok := planOut["items"].([]any)
	if !ok {
		t.Fatalf("outputs.plan.items decoded as %T, want []any", planOut["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] decoded as %T, want map[string]any", items[0])
	}
	if v, ok := item["weight"].(int64); !ok || v != 2 {
		t.Fatalf("items[0].weight decoded as %T (%v), want int64(2)", item["weight"], item["weight"])
	}
	detail, ok := planOut["detail"].(map[string]any)
	if !ok {
		t.Fatalf("outputs.plan.detail decoded as %T, want map[string]any", planOut["detail"])
	}
	if _, ok := detail["nested"].(map[string]any); !ok {
		t.Fatalf("detail.nested decoded as %T, want map[string]any", detail["nested"])
	}
}
