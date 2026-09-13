package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

func runPortsFileCapture(t *testing.T, factory portsTestStoreFactory) {
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
