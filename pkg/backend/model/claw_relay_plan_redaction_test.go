package model

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// planTodoWireKeys maps every string field of the PERSISTED todo to the
// `todos[]` key a tool call fills it from — claude_code's `activeForm` and
// claw's `id`/`priority` included, the union parsePlanTodos decodes.
//
// Read against the type below, so a field added to store.PlanTodo reddens
// this witness until someone drives it: the record is written by a sink
// Layer 0 does not wrap, so a far-side string it keeps reaches disk through
// this hook alone.
var planTodoWireKeys = map[string]string{
	"Content":    "content",
	"Status":     "status",
	"ActiveForm": "activeForm",
	"Priority":   "priority",
	"ID":         "id",
}

// A relayed plan write is the second relayed event whose payload is
// PERSISTED rather than logged: the tool-started hook snapshots the agent's
// todo list into the run's plan store (runs/<id>/plans/, the Mongo
// run_plans collection), a sink outside Layer 0's event wrapper.
//
// Driven through the real chain — the crossing's event handler, the relay
// decoder, the production store hooks, the filesystem store — with the
// secret moved to ONE todo field per subtest and the verdict read off the
// whole tree the run wrote.
func TestClawRelay_PersistsNoRelayedPlanInClear(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-PLANWITNESS0123456789abcdefghij"

	// Every string field of the persisted todo has a wire key that drives it.
	rt := reflect.TypeOf(store.PlanTodo{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		if planTodoWireKeys[f.Name] == "" {
			t.Fatalf("store.PlanTodo.%s is a persisted far-side string with no wire key in planTodoWireKeys — the witness drives it nowhere", f.Name)
		}
	}

	for field, key := range planTodoWireKeys {
		t.Run(field, func(t *testing.T) {
			const runID = "run-plan-redaction"
			g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}}, secretguard.DefaultConfig())
			placeholder := g.Redact(secret)
			if placeholder == secret || placeholder == "" {
				t.Fatalf("scenario broken: the guard does not redact the witness value (%q)", placeholder)
			}

			root := t.TempDir()
			st, err := store.New(root)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			logger := iterlog.Nop()
			hooks := NewStoreEventHooks(context.Background(), st, runID, logger, g, nil)
			b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{}, WithClawLogger(logger))

			// A todo the plan panel would render, with the secret in the one
			// field under test: every other field keeps a legitimate value, so
			// no subtest proves its point on a snapshot the hook refused.
			todo := map[string]any{"content": "ship it", "status": "pending"}
			todo[key] = secret
			input, err := json.Marshal(map[string]any{"todos": []any{todo}})
			if err != nil {
				t.Fatal(err)
			}
			handler, _ := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "campaign", Iteration: 1}, sessionSeed{})
			handler.OnEvent(string(store.EventToolStarted), map[string]any{
				"tool":        "todo_write",
				"tool_use_id": "tu_9",
				"input":       json.RawMessage(input),
				"iteration":   1,
				"input_size":  len(input),
			})

			snaps, err := st.ListPlanSnapshots(context.Background(), runID)
			if err != nil {
				t.Fatalf("ListPlanSnapshots: %v", err)
			}
			if len(snaps) != 1 || len(snaps[0].Todos) != 1 {
				t.Fatalf("scenario broken: the relayed plan write left %d snapshots — nothing to read the verdict off", len(snaps))
			}

			var withSecret, withPlaceholder []string
			if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				if strings.Contains(string(body), secret) {
					withSecret = append(withSecret, path)
				}
				if strings.Contains(string(body), placeholder) {
					withPlaceholder = append(withPlaceholder, path)
				}
				return nil
			}); err != nil {
				t.Fatalf("walk the run's tree: %v", err)
			}
			if len(withSecret) > 0 {
				t.Errorf("a relayed plan write's %s reached disk with the run's secret in clear, in %v\n  snapshot: %+v",
					key, withSecret, snaps[0].Todos)
			}
			if len(withPlaceholder) == 0 {
				t.Errorf("nothing the run wrote carries the placeholder for %s — the walk proves nothing", key)
			}
		})
	}
}

// The pass leaves the plan's own vocabulary alone: the status the studio
// counts on, the canonicalisation of claw's `done`, and a model-authored id
// that holds no secret all reach the store as produced.
func TestClawRelay_APlanWritesOwnVocabularyCrossesUntouched(t *testing.T) {
	t.Parallel()
	const runID = "run-plan-vocabulary"
	g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: "sk-proj-PLANVOCAB0123456789abcdefghijk"}}, secretguard.DefaultConfig())
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	logger := iterlog.Nop()
	hooks := NewStoreEventHooks(context.Background(), st, runID, logger, g, nil)
	b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{}, WithClawLogger(logger))

	input := `{"todos":[
	  {"id":"t-1","content":"wire the relay","status":"done","priority":"high","activeForm":"wiring the relay"},
	  {"id":"t-2","content":"ship it","status":"in_progress","priority":"medium"}
	]}`
	handler, _ := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "campaign", Iteration: 1}, sessionSeed{})
	handler.OnEvent(string(store.EventToolStarted), map[string]any{
		"tool": "todo_write", "tool_use_id": "tu_9", "iteration": 1,
		"input": json.RawMessage(input), "input_size": len(input),
	})

	snaps, err := st.ListPlanSnapshots(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPlanSnapshots: %v", err)
	}
	if len(snaps) != 1 || len(snaps[0].Todos) != 2 {
		t.Fatalf("the relayed plan write left %+v, want one snapshot of two todos", snaps)
	}
	want := []store.PlanTodo{
		{ID: "t-1", Content: "wire the relay", Status: "completed", Priority: "high", ActiveForm: "wiring the relay"},
		{ID: "t-2", Content: "ship it", Status: "in_progress", Priority: "medium"},
	}
	for i, w := range want {
		if got := snaps[0].Todos[i]; got != w {
			t.Errorf("todo %d = %+v, want %+v — the pass rewrote a value that holds no secret", i, got, w)
		}
	}
}
