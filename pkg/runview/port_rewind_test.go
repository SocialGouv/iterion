package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/internal/s3test"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	storemongo "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const nativeRewindBot = `dsl: 2
contract Root:
  display_name: "Rewind a batch"
  responsibility: "Deliver a batch with an independent report"
  inputs:
    items: string[]
  outputs:
    results: string[]
    report: string
contract Item:
  display_name: "Render an item"
  responsibility: "Render one item"
  inputs:
    item: string
  outputs:
    text: string
contract Report:
  display_name: "Prepare a report"
  responsibility: "Produce an independent report"
  outputs:
    report: string
contract Collect:
  display_name: "Collect a batch"
  responsibility: "Collect the rendered items"
  inputs:
    texts: string[]
    report: string
      required: false
  outputs:
    texts: string[]
tool item_impl:
  command: "fixture"
tool report_impl:
  command: "fixture"
tool collect_impl:
  command: "fixture"
tool finish_impl:
  command: "fixture"
workflow rewind_batch:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      a:
        implementation: item_impl
        contract: Item
      b:
        implementation: report_impl
        contract: Report
      c:
        implementation: collect_impl
        contract: Collect
      d:
        implementation: finish_impl
        contract: Collect
    bindings:
      input.items -> a.item
      a.text -> c.texts
      c.texts -> d.texts
      b.report -> d.report
    exports:
      results: d.texts
      report: b.report
    products: ["results", "report"]
`

func nativeRewindContext() context.Context {
	return store.WithIdentity(context.Background(), "native-rewind", "owner")
}

type nativeRewindExecutor struct {
	mu    sync.Mutex
	calls map[string]int
	fail  bool
}

func (e *nativeRewindExecutor) Execute(_ context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := node.NodeID()
	if strings.HasPrefix(id, "@a") {
		id = "a"
	}
	e.calls[id]++
	switch id {
	case "a":
		return map[string]any{"text": "rendered-" + input["item"].(string)}, nil
	case "b":
		return map[string]any{"report": "retained"}, nil
	case "d":
		if e.fail {
			return nil, errors.New("fixture stops after upstream publication")
		}
	}
	return map[string]any{"texts": input["texts"]}, nil
}

func nativeRewindStore(t *testing.T, cloud bool) store.RunStore {
	t.Helper()
	ctx := nativeRewindContext()
	var result store.RunStore
	if !cloud {
		fs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		result = fs
	} else {
		if os.Getenv("ITERION_TEST_MONGO_URI") == "" {
			if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
				t.Fatal("required native rewind tests need ITERION_TEST_MONGO_URI")
			}
			t.Skip("ITERION_TEST_MONGO_URI not set")
		}
		_, gateway := s3test.New(t, "rewind")
		objects, err := blob.NewS3(ctx, blob.Config{Bucket: "rewind", Region: "us-east-1", Endpoint: gateway.URL, UsePathStyle: true, AccessKeyID: "fixture", SecretAccessKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		ms, err := storemongo.New(ctx, storemongo.Config{URI: os.Getenv("ITERION_TEST_MONGO_URI"), Database: "iterion_ports_rewind_" + bson.NewObjectID().Hex(), Blob: objects, RunFilesScratchDir: filepath.Join(t.TempDir(), "files")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := mongotest.TeardownCtx()
			defer cancel()
			if err := ms.DB().Drop(ctx); err != nil {
				t.Error(err)
			}
			_ = ms.Close(ctx)
			_ = objects.Close()
		})
		result = &nativeRewindMongo{Store: ms}
	}
	identity, err := store.PortStoreIdentity(result)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := &store.PortActivation{Version: store.PortActivationVersion, ProofRevision: 1, Revision: 1, Enabled: true, Scope: store.PortActivationLocal,
		StoreIdentity: identity, ProofDigest: strings.Repeat("a", 64), VerifiedAt: now, ExpiresAt: now.Add(time.Hour)}
	if cloud {
		record.Scope, record.QueueVersion, record.ConsumerAccessEvidence = store.PortActivationDistributed, 15, "isolated rewind fixture"
		record.ExpiresAt = now.Add(store.PortDistributedProofMaxAge)
	}
	record.CapabilityDigest = portsactivation.CapabilityDigest(record.Scope)
	if err := store.AsPortActivationStore(result).SavePortActivation(ctx, 0, record); err != nil {
		t.Fatal(err)
	}
	return result
}

type nativeRewindMongo struct{ *storemongo.Store }

func (s *nativeRewindMongo) VerifyPortDistributedActivation(_ context.Context, a *store.PortActivation, now time.Time) error {
	if a.ConsumerAccessEvidence != "isolated rewind fixture" || a.StoreIdentity != s.PortBackendIdentity() || a.QueueVersion != 15 || now.Before(a.VerifiedAt) || !now.Before(a.ExpiresAt) {
		return store.ErrPortActivation
	}
	return nil
}

func nativeRewindFixture(t *testing.T, cloud bool, items []any, sources ...string) (*Service, *runtime.Engine, *nativeRewindExecutor, *store.Run, string) {
	t.Helper()
	source := nativeRewindBot
	if len(sources) > 0 {
		source = sources[0]
	}
	st := nativeRewindStore(t, cloud)
	dir := t.TempDir()
	path := filepath.Join(dir, "rewind.bot")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, hash, _, err := CompileWorkflowPath(path)
	if err != nil {
		t.Fatal(err)
	}
	executor := &nativeRewindExecutor{calls: map[string]int{}, fail: true}
	engine := runtime.New(wf, st, executor, runtime.WithLogger(iterlog.Nop()), runtime.WithWorkDir(dir), runtime.WithSandboxOverride("none"),
		runtime.WithWorkflowHash(hash), runtime.WithWorkflowSource(source), runtime.WithFilePath(path))
	const id = "pc1_rewind"
	runErr := engine.Run(nativeRewindContext(), id, map[string]any{"items": items})
	if runErr == nil {
		t.Fatal("fixture did not stop at the terminal node")
	}
	run, err := st.LoadRun(nativeRewindContext(), id)
	if err != nil || run.Status != store.RunStatusFailedResumable || run.PortExecution == nil || run.PortExecution.Invocations["c"].Status != store.PortSucceeded {
		t.Fatalf("fixture did not publish upstream work: run=%+v err=%v execution=%v", run, err, runErr)
	}
	svc, err := NewService("", WithStore(st), WithLogger(iterlog.Nop()), WithWorkDir(dir), WithoutWorkspaceTracking())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.StopBackground(nativeRewindContext()) })
	return svc, engine, executor, run, path
}

func TestNativeRewindFilesystem(t *testing.T) { testNativeRewind(t, false) }
func TestNativeRewindMongo(t *testing.T)      { testNativeRewind(t, true) }

func testNativeRewind(t *testing.T, cloud bool) {
	for _, tc := range []struct {
		name       string
		items      []any
		removeEdge bool
	}{
		{"mapped", []any{"one", "two"}, false},
		{"empty_collection", []any{}, false},
		{"captured_dependency_after_edit", []any{"one"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, engine, executor, before, path := nativeRewindFixture(t, cloud, tc.items)
			if tc.removeEdge {
				edited := strings.Replace(nativeRewindBot, "a.text -> c.texts", "input.items -> c.texts", 1)
				if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := svc.Rewind(nativeRewindContext(), RewindSpec{RunID: before.ID, NodeID: "a", RestoreScope: RestoreScopeNone})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.DroppedNodes, []string{"a", "c", "d"}) {
				t.Fatalf("wrong data-graph suffix: %+v", result)
			}
			after, err := svc.store.LoadRun(nativeRewindContext(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.PortExecution.Budget, before.PortExecution.Budget) || after.Status != store.RunStatusCancelled {
				t.Fatalf("rewind changed paid usage or failed to park: %+v", after.PortExecution.Budget)
			}
			if !reflect.DeepEqual(after.PortExecution.Invocations["b"], before.PortExecution.Invocations["b"]) || after.PortExecution.Collections["a"] != nil {
				t.Fatal("rewind discarded independent work or retained the mapped collection")
			}
			for _, publication := range after.PortExecution.Publications {
				if publication.Producer != "input" && publication.Producer != "b" {
					t.Fatalf("rewind retained an invalidated publication: %+v", publication)
				}
			}
			if tc.removeEdge {
				// Restoring the original source isolates rewind's captured-edge
				// behavior from force-resume's separate source migration policy.
				if err := os.WriteFile(path, []byte(nativeRewindBot), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			executor.fail = false
			if err := engine.Resume(nativeRewindContext(), before.ID, nil); err != nil {
				t.Fatal(err)
			}
			finished, err := svc.store.LoadRun(nativeRewindContext(), before.ID)
			if err != nil || finished.Status != store.RunStatusFinished {
				t.Fatalf("rewound run did not finish: %+v %v", finished, err)
			}
			if executor.calls["a"] != 2*len(tc.items) || executor.calls["b"] != 1 || executor.calls["c"] != 2 || executor.calls["d"] != 2 {
				t.Fatalf("rewind replayed the wrong work: %v", executor.calls)
			}
			if finished.PortExecution.Budget.Consumed.Iterations != before.PortExecution.Budget.Consumed.Iterations+int64(len(tc.items)+2) {
				t.Fatal("rewind/resume refunded or lost execution accounting")
			}
		})
	}
}

func TestNativeRewindRefusalsPreserveCheckpoint(t *testing.T) {
	svc, _, _, before, _ := nativeRewindFixture(t, false, []any{"one"})
	for _, spec := range []RewindSpec{
		{RunID: before.ID, Auto: true},
		{RunID: before.ID, NodeID: "a", RestoreScope: RestoreScopeFull},
		{RunID: before.ID, NodeID: "missing", RestoreScope: RestoreScopeNone},
	} {
		if _, err := svc.Rewind(nativeRewindContext(), spec); err == nil {
			t.Fatalf("unsupported rewind accepted: %+v", spec)
		}
		after, err := svc.store.LoadRun(nativeRewindContext(), before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("refused rewind mutated the run: %v", err)
		}
	}
}

func TestNativeRewindCannotEraseUncertainEffect(t *testing.T) {
	effect := "  effects:\n    request:\n      description: \"Fixture request\"\n      paid: false\n"
	source := strings.Replace(nativeRewindBot, "contract Item:", effect+"contract Item:", 1)
	source = strings.Replace(source, "tool item_impl:", effect+"port_policy recovery:\n  effects:\n    request:\n      recovery: manual\ntool item_impl:", 1)
	source = strings.Replace(source, "  contract: Root\n", "  contract: Root\n  port_policy: recovery\n", 1)
	svc, _, _, before, _ := nativeRewindFixture(t, false, []any{"one"}, source)
	if before.PortExecution.Invocations["d"].Status != store.PortUncertain {
		t.Fatal("fixture did not persist an uncertain effect")
	}
	if _, err := svc.Rewind(nativeRewindContext(), RewindSpec{RunID: before.ID, NodeID: "a", RestoreScope: RestoreScopeNone}); err == nil || !strings.Contains(err.Error(), "unresolved effect") {
		t.Fatalf("rewind bypassed effect recovery: %v", err)
	}
	after, err := svc.store.LoadRun(nativeRewindContext(), before.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rewind erased uncertain effect or accounting: %v", err)
	}
}

type nativeRewindRaceStore struct {
	store.RunStore
	raced bool
}

func (s *nativeRewindRaceStore) SaveRun(ctx context.Context, run *store.Run) error {
	if !s.raced {
		s.raced = true
		current, err := s.RunStore.LoadRun(ctx, run.ID)
		if err != nil {
			return err
		}
		current.Status = store.RunStatusRunning
		if err := s.RunStore.SaveRun(ctx, current); err != nil {
			return err
		}
	}
	return s.RunStore.SaveRun(ctx, run)
}

func TestNativeRewindLosesCASWithoutInvalidating(t *testing.T) {
	svc, _, _, before, _ := nativeRewindFixture(t, false, []any{"one"})
	svc.store = &nativeRewindRaceStore{RunStore: svc.store}
	if _, err := svc.Rewind(nativeRewindContext(), RewindSpec{RunID: before.ID, NodeID: "a", RestoreScope: RestoreScopeNone}); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("rewind ignored concurrent resume: %v", err)
	}
	after, err := svc.store.LoadRun(nativeRewindContext(), before.ID)
	if err != nil || after.Status != store.RunStatusRunning || !reflect.DeepEqual(before.PortExecution, after.PortExecution) {
		t.Fatalf("losing rewind corrupted current work: %v", err)
	}
}
