package model

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A tool payload too large to ride inline goes to a sidecar blob whose
// DIRECTORY is named with the call's tool_use_id, and the event keeps that
// id as `<kind>_ref` — the key the studio and the files-touched route fetch
// the blob with. On a sandboxed node the id is the container's text: the
// far side picks it (claw's own schema has the model author tool ids), so
// it crosses the run's redaction once, and the two copies stay the same
// string — a blob the event points at, under a name that holds no secret.
func TestClawRelay_ARelayedToolBlobIsKeyedByWhatTheEventPointsAt(t *testing.T) {
	const secret = "sk-proj-BLOBWITNESS0123456789abcdefghij"
	const runID = "run-blob-key"

	for _, kind := range []string{"input", "output"} {
		t.Run(kind, func(t *testing.T) {
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
			handler := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "campaign", Iteration: 1}, sessionSeed{})

			// Past toolInlineThreshold, so the payload takes the sidecar.
			bulk := strings.Repeat("w", toolInlineThreshold+1)
			switch kind {
			case "input":
				input, merr := json.Marshal(map[string]any{"body": bulk})
				if merr != nil {
					t.Fatal(merr)
				}
				handler.OnEvent(string(store.EventToolStarted), map[string]any{
					"tool": "write_file", "tool_use_id": secret, "iteration": 1,
					"input": json.RawMessage(input), "input_size": len(input),
				})
			case "output":
				handler.OnEvent(string(store.EventToolCalled), map[string]any{
					"tool": "write_file", "tool_use_id": secret,
					"output": bulk, "duration_ms": 12,
				})
			}

			evts, err := st.LoadEvents(context.Background(), runID)
			if err != nil {
				t.Fatalf("LoadEvents: %v", err)
			}
			var ref string
			for _, e := range evts {
				if r, ok := e.Data[kind+"_ref"].(string); ok && r != "" {
					ref = r
				}
			}
			if ref == "" {
				t.Fatalf("scenario broken: no event carries a %s_ref — the payload rode inline and the witness watches nothing", kind)
			}
			if strings.Contains(ref, secret) {
				t.Errorf("the event's %s_ref carries the run's secret in clear: %q", kind, ref)
			}

			// What the readers fetch with is the ref, so the blob has to be
			// under exactly that name.
			blobDir := filepath.Join(root, "runs", runID, "tools", ref, kind)
			if _, err := os.Stat(blobDir); err != nil {
				t.Errorf("the event points at %s_ref=%q and nothing is there (%v) — the blob is unreachable", kind, ref, err)
			}

			// Read the verdict off every path AND every byte the run wrote.
			var inPath, inBody []string
			if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.Contains(path, secret) {
					inPath = append(inPath, path)
				}
				if d.IsDir() {
					return nil
				}
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				if strings.Contains(string(body), secret) {
					inBody = append(inBody, path)
				}
				return nil
			}); err != nil {
				t.Fatalf("walk the run's tree: %v", err)
			}
			if len(inPath) > 0 {
				t.Errorf("a relayed tool_use_id reached the filesystem as a path with the run's secret in clear: %v", inPath)
			}
			if len(inBody) > 0 {
				t.Errorf("a relayed tool_use_id reached disk with the run's secret in clear, in %v", inBody)
			}
		})
	}
}

// The pass leaves a real provider's call id alone: the blob key is the id
// itself for every shape the backends actually produce, so an operator
// reading a run's tools/ tree still sees the ids the model used.
func TestClawRelay_ARealToolUseIDIsTheBlobKey(t *testing.T) {
	const runID = "run-blob-real-ids"
	ids := []string{"toolu_01A09q90qw90lq917835lq9", "toolu_bdrk_01ABCdefGH", "call_abc123XYZ", "fc_9f8e7d6c", "tu_9"}
	g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: "sk-proj-BLOBREAL0123456789abcdefghijkl"}}, secretguard.DefaultConfig())
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
	handler := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "campaign", Iteration: 1}, sessionSeed{})

	for _, id := range ids {
		handler.OnEvent(string(store.EventToolCalled), map[string]any{
			"tool": "write_file", "tool_use_id": id,
			"output": strings.Repeat("w", toolInlineThreshold+1), "duration_ms": 12,
		})
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(root, "runs", runID, "tools", id, "output")); err != nil {
			t.Errorf("the blob of call %q is not under its own id (%v) — the pass rewrote an id that holds no secret", id, err)
		}
	}
}
