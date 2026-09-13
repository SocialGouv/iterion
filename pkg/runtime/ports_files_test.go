package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

const portsFileSource = `dsl: 2
contract Root:
  display_name: "Deliver file"
  responsibility: "Deliver a verified file"
  inputs:
    value: string
  outputs:
    report: file
contract Produce:
  display_name: "Write report"
  responsibility: "Produce a file"
  inputs:
    value: string
  outputs:
    report: file
      file:
        media_type: "text/plain"
        min_bytes: 1
contract Pass:
  display_name: "Pass report"
  responsibility: "Forward the verified file"
  inputs:
    report: file
  outputs:
    report: file
tool produce_impl:
  command: "fixture"
tool pass_impl:
  command: "fixture"
workflow deliver:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      produce:
        implementation: produce_impl
        contract: Produce
      pass:
        implementation: pass_impl
        contract: Pass
    bindings:
      input.value -> produce.value
      produce.report -> pass.report
    exports:
      report: pass.report
    products: ["report"]
`

const portsMappedFileSource = `dsl: 2
contract Root:
  display_name: "Deliver files"
  responsibility: "Deliver mapped files"
  inputs:
    items: string[]
  outputs:
    reports: file[]
contract Produce:
  display_name: "Write one report"
  responsibility: "Produce one mapped file"
  inputs:
    item: string
  outputs:
    report: file
      file:
        media_type: "text/plain"
        min_bytes: 1
tool produce_impl:
  command: "fixture"
workflow deliver:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      produce:
        implementation: produce_impl
        contract: Produce
    bindings:
      input.items -> produce.item
    exports:
      reports: produce.report
    products: ["reports"]
`

const portsAttachmentSource = `dsl: 2
contract Root:
  display_name: "Forward attachment"
  responsibility: "Publish a verified attachment"
  inputs:
    source: file
      file:
        media_type: "text/plain"
        min_bytes: 1
  outputs:
    report: file
contract Pass:
  display_name: "Forward file"
  responsibility: "Pass the captured input"
  inputs:
    source: file
  outputs:
    report: file
tool pass_impl:
  command: "fixture"
workflow forward:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      pass:
        implementation: pass_impl
        contract: Pass
    bindings:
      input.source -> pass.source
    exports:
      report: pass.report
    products: ["report"]
`

func TestPortsFileCaptureFilesystem(t *testing.T) { runPortsFileCapture(t, tmpStore) }
func TestPortsFileCaptureMongo(t *testing.T) {
	if os.Getenv("ITERION_TEST_MONGO_URI") == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required Mongo file capture needs a replica set")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	runPortsFileCapture(t, portsTestMongoStore)
}

type transientPortFilesStore struct {
	store.RunStore
	files       store.RunFilesStore
	openError   error
	corruptOnce atomic.Bool
}

func (s *transientPortFilesStore) EnsureRunFilesDir(ctx context.Context, id string) (string, error) {
	return s.files.EnsureRunFilesDir(ctx, id)
}
func (s *transientPortFilesStore) ListRunFiles(ctx context.Context, id string) ([]store.RunFileInfo, error) {
	return s.files.ListRunFiles(ctx, id)
}
func (s *transientPortFilesStore) OpenRunFile(ctx context.Context, id, path string) (io.ReadCloser, store.RunFileInfo, error) {
	if s.openError != nil {
		return nil, store.RunFileInfo{}, s.openError
	}
	if s.corruptOnce.Swap(false) {
		return io.NopCloser(strings.NewReader("corrupt")), store.RunFileInfo{Path: path}, nil
	}
	return s.files.OpenRunFile(ctx, id, path)
}
func (s *transientPortFilesStore) PutPortFile(ctx context.Context, ref store.PortFileRef, body io.Reader) error {
	return store.AsPortFilesStore(s.RunStore).PutPortFile(ctx, ref, body)
}

func runPortsFileCapture(t *testing.T, factory portsTestStoreFactory) {
	t.Run("corrupt read invalidates the actual file producer", func(t *testing.T) {
		executor := portsExecutorFunc(func(ctx context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
			if _, producer := input["value"]; producer {
				scope, ok := model.InvocationFilesFromContext(ctx)
				if !ok {
					return nil, fmt.Errorf("missing producer area")
				}
				if err := os.WriteFile(filepath.Join(scope.HostDir, "report.txt"), []byte("saved\n"), 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"report": "report.txt"}, nil
			}
			return nil, errors.New("consumer stays failed")
		})
		engine, s := portsTestEngine(t, factory, portsFileSource, executor)
		ctx := portsTestContext(t)
		const id = "pc1_file_corrupt"
		if err := engine.Run(ctx, id, map[string]any{"value": "saved"}); err == nil {
			t.Fatal("first attempt unexpectedly succeeded")
		}
		wrapped := &transientPortFilesStore{RunStore: s, files: store.AsRunFilesStore(s)}
		wrapped.corruptOnce.Store(true)
		resumer := New(portsTestWorkflow(t, portsFileSource), wrapped, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
		if err := resumer.Resume(ctx, id, nil); err == nil {
			t.Fatal("consumer unexpectedly succeeded")
		}
		after := portsTestRun(t, s, id)
		if producer := after.PortExecution.Invocations["produce"]; producer.Attempt != 2 || producer.Status != store.PortSucceeded {
			t.Fatalf("corrupt file did not invalidate its producer: %+v", producer)
		}
	})
	t.Run("transient file read preserves committed producer on resume", func(t *testing.T) {
		executor := portsExecutorFunc(func(ctx context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
			if _, producer := input["value"]; producer {
				scope, ok := model.InvocationFilesFromContext(ctx)
				if !ok {
					return nil, fmt.Errorf("missing producer area")
				}
				if err := os.WriteFile(filepath.Join(scope.HostDir, "report.txt"), []byte("saved\n"), 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"report": "report.txt"}, nil
			}
			return nil, errors.New("fail consumer after producer commit")
		})
		engine, s := portsTestEngine(t, factory, portsFileSource, executor)
		ctx := portsTestContext(t)
		const id = "pc1_file_recover"
		if err := engine.Run(ctx, id, map[string]any{"value": "saved"}); err == nil {
			t.Fatal("first attempt unexpectedly succeeded")
		}
		before := portsTestRun(t, s, id)
		if before.PortExecution.Invocations["produce"].Status != store.PortSucceeded {
			t.Fatal("producer did not commit before failure")
		}
		temporary := errors.New("temporary object-store outage")
		wrapped := &transientPortFilesStore{RunStore: s, files: store.AsRunFilesStore(s), openError: temporary}
		resumer := New(portsTestWorkflow(t, portsFileSource), wrapped, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
		if err := resumer.Resume(ctx, id, nil); !errors.Is(err, temporary) {
			t.Fatalf("transient read was not preserved: %v", err)
		}
		after := portsTestRun(t, s, id)
		if after.PortExecution.Invocations["produce"].Attempt != 1 || after.PortExecution.Invocations["produce"].Status != store.PortSucceeded {
			t.Fatalf("transient read invalidated successful work: %+v", after.PortExecution.Invocations["produce"])
		}
	})
	t.Run("root attachment is captured before consumption", func(t *testing.T) {
		executor := portsExecutorFunc(func(ctx context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
			scope, ok := model.InvocationFilesFromContext(ctx)
			if !ok {
				return nil, fmt.Errorf("file consumer has no invocation scope")
			}
			descriptor := input["source"].(map[string]any)
			mapped, ok := scope.Inputs[descriptor["path"].(string)]
			if !ok {
				return nil, fmt.Errorf("file consumer has no private input materialization")
			}
			body, err := os.ReadFile(mapped.HostPath)
			if err != nil || string(body) != "attached\n" {
				return nil, fmt.Errorf("private input is unavailable or invalid: %q %v", body, err)
			}
			return map[string]any{"report": mapped.HostPath}, nil
		})
		engine, s := portsTestEngine(t, factory, portsAttachmentSource, executor)
		ctx := portsTestContext(t)
		const id = "pc1_input_file"
		createCtx, err := portsactivation.AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRun(store.WithRuntimeSemantics(createCtx, ir.RuntimeSemanticsPortsV1), id, "forward", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteAttachment(ctx, id, store.AttachmentRecord{Name: "source", OriginalFilename: "source.txt", MIME: "text/plain"}, strings.NewReader("attached\n")); err != nil {
			t.Fatal(err)
		}
		if err := engine.Run(ctx, id, map[string]any{"source": "attachment:source"}); err != nil {
			t.Fatal(err)
		}
		run := portsTestRun(t, s, id)
		inputRef := run.PortExecution.Publications["input.source"].Files
		outputRef := run.PortExecution.Publications[run.PortExecution.Exports["report"]].Files
		if len(inputRef) != 1 || len(outputRef) != 1 || outputRef[0] != inputRef[0] || inputRef[0].Producer != "input" {
			t.Fatalf("attachment lineage: input=%+v output=%+v", inputRef, outputRef)
		}
		body, _, err := store.AsRunFilesStore(s).OpenRunFile(ctx, id, inputRef[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(body)
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if readErr != nil || string(data) != "attached\n" {
			t.Fatalf("attachment capture: %q %v", data, readErr)
		}
	})
	t.Run("mapped files keep item order and origin", func(t *testing.T) {
		executor := portsExecutorFunc(func(ctx context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
			area, ok := model.InvocationFilesFromContext(ctx)
			if !ok {
				return nil, fmt.Errorf("mapped producer has no output area")
			}
			item := input["item"].(string)
			name := item + ".txt"
			if err := os.WriteFile(filepath.Join(area.HostDir, name), []byte(item+"\n"), 0o644); err != nil {
				return nil, err
			}
			return map[string]any{"report": name}, nil
		})
		engine, s := portsTestEngine(t, factory, portsMappedFileSource, executor)
		ctx := portsTestContext(t)
		if err := engine.Run(ctx, "pc1_file_map", map[string]any{"items": []any{"a", "b", "c"}}); err != nil {
			t.Fatal(err)
		}
		run := portsTestRun(t, s, "pc1_file_map")
		publication := run.PortExecution.Publications[run.PortExecution.Exports["reports"]]
		values := portsTestExport(t, run, "reports").([]any)
		if len(values) != 3 || len(publication.Files) != 3 {
			t.Fatalf("mapped output count: %d descriptors, %d refs", len(values), len(publication.Files))
		}
		for index, item := range []string{"a", "b", "c"} {
			ref := publication.Files[index]
			if ref.Producer != fmt.Sprintf("@produce[%d]", index) || values[index].(map[string]any)["path"] != ref.Path {
				t.Fatalf("mapped file %d lost its producer: %+v %v", index, ref, values[index])
			}
			body, _, err := store.AsRunFilesStore(s).OpenRunFile(ctx, run.ID, ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(body)
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if readErr != nil || string(data) != item+"\n" {
				t.Fatalf("mapped file %d: %q %v", index, data, readErr)
			}
		}
	})
	t.Run("verified pass through", func(t *testing.T) {
		executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
			if _, ok := input["value"]; ok {
				area, ok := model.InvocationFilesFromContext(ctx)
				if !ok || area.HostDir == "" {
					return nil, fmt.Errorf("file producer has no invocation-scoped area")
				}
				if err := os.WriteFile(filepath.Join(area.HostDir, "report.txt"), []byte("hello\n"), 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"report": "report.txt"}, nil
			}
			return map[string]any{"report": input["report"]}, nil
		})
		engine, s := portsTestEngine(t, factory, portsFileSource, executor)
		ctx := portsTestContext(t)
		if err := engine.Run(ctx, "pc1_file_pass", map[string]any{"value": "hello"}); err != nil {
			t.Fatal(err)
		}
		run := portsTestRun(t, s, "pc1_file_pass")
		result := portsTestExport(t, run, "report").(map[string]any)
		ref := run.PortExecution.Publications[run.PortExecution.Exports["report"]].Files
		if len(ref) != 1 || ref[0].Producer != "produce" || result["path"] != ref[0].Path {
			t.Fatalf("bad file lineage: %v %+v", result, ref)
		}
		files := store.AsRunFilesStore(s)
		listed, err := files.ListRunFiles(ctx, run.ID)
		if err != nil || len(listed) != 1 || listed[0].Path != ref[0].Path {
			t.Fatalf("published files: %+v %v", listed, err)
		}
		body, _, err := files.OpenRunFile(ctx, run.ID, ref[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(body)
		if readErr != nil || body.Close() != nil || string(data) != "hello\n" {
			t.Fatalf("captured body %q: %v", data, readErr)
		}
		if raw, _, err := files.OpenRunFile(ctx, run.ID, "invocations/produce/1/outputs/report.txt"); err == nil {
			_ = raw.Close()
			t.Fatal("unpublished scratch was visible")
		}
	})
	t.Run("missing declared file", func(t *testing.T) {
		executor := portsExecutorFunc(func(_ context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
			if _, ok := input["value"]; ok {
				return map[string]any{"report": "missing.txt"}, nil
			}
			return map[string]any{"report": input["report"]}, nil
		})
		engine, s := portsTestEngine(t, factory, portsFileSource, executor)
		err := engine.Run(portsTestContext(t), "pc1_file_missing", map[string]any{"value": "hello"})
		if err == nil || !strings.Contains(err.Error(), "was not produced") {
			t.Fatalf("missing file error: %v", err)
		}
		run := portsTestRun(t, s, "pc1_file_missing")
		if run.PortExecution.Invocations["produce"].Status != store.PortFailed || len(run.PortExecution.Publications) != 1 {
			t.Fatalf("missing file published: %+v", run.PortExecution)
		}
	})
}
