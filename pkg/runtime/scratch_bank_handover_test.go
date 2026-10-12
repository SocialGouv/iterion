package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bindScratchDriver is docker with host bind mounts: a sandbox whose spec
// binds ${PROJECT_SCRATCH_DIR} to a host directory reads and writes it
// there; otherwise the scratch is the new container's own, under root.
type bindScratchDriver struct {
	root      string
	mu        sync.Mutex
	scratches []string
}

func (d *bindScratchDriver) Name() string { return "docker" }

func (d *bindScratchDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true, SupportsHostBindMounts: true}
}

func (d *bindScratchDriver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	return stallingPrepared{spec: spec}, nil
}

func (d *bindScratchDriver) Start(_ context.Context, p sandbox.PreparedSpec, _ sandbox.RunInfo) (sandbox.Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	scratch := ""
	for _, m := range p.(stallingPrepared).spec.Mounts {
		if !strings.Contains(m, "target="+sandboxScratchContainerPath+",") {
			continue
		}
		for _, kv := range strings.Split(m, ",") {
			if strings.HasPrefix(kv, "source=") {
				scratch = strings.TrimPrefix(kv, "source=")
			}
		}
	}
	if scratch == "" {
		scratch = filepath.Join(d.root, fmt.Sprintf("container-%d", len(d.scratches)), "scratch")
	}
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		return nil, err
	}
	d.scratches = append(d.scratches, scratch)
	return &podRun{scratch: scratch}, nil
}

func (d *bindScratchDriver) scratch() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scratches[len(d.scratches)-1]
}

// handoverWorkflow writes the scratch in measure, rewrites it in report after
// a first pause, and reads it in final after a second.
func handoverWorkflow() *ir.Workflow {
	human := func(id string) *ir.HumanNode {
		return &ir.HumanNode{BaseNode: ir.BaseNode{ID: id}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	}
	return &ir.Workflow{
		Name:  "scratch_handover",
		Entry: "measure",
		Nodes: map[string]ir.Node{
			"measure": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "measure"}},
			"gate":    human("gate"),
			"report":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "report"}},
			"gate2":   human("gate2"),
			"final":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "measure", To: "gate"}, {From: "gate", To: "report"}, {From: "report", To: "gate2"},
			{From: "gate2", To: "final"}, {From: "final", To: "done"},
		},
		Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/iterion-sandbox:test"},
	}
}

// TestResume_aBankRestoredIntoAHostScratchHandsItOver: a run parks with its
// scratch in the container, and its bank is restored into the host directory
// a later sandbox binds (the host state switched on). From then on the host
// directory keeps the scratch: a later resume on that host is not refused
// over the bank, forced or not, and never restores the older bank over what
// the host directory holds.
func TestResume_aBankRestoredIntoAHostScratchHandsItOver(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			t.Setenv("ITERION_MODE", "local")
			t.Setenv("ITERION_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-handover"
			d := &bindScratchDriver{root: t.TempDir()}
			work := t.TempDir()
			x := newStubExecutor()
			file := func() string { return filepath.Join(d.scratch(), "floor.json") }
			x.on("measure", func(map[string]any) (map[string]any, error) {
				if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
					return nil, err
				}
				return map[string]any{}, os.WriteFile(file(), []byte("v1"), 0o644)
			})
			x.on("report", func(map[string]any) (map[string]any, error) {
				b, err := os.ReadFile(file())
				if err != nil || string(b) != "v1" {
					return nil, fmt.Errorf("report: read %q, %v", b, err)
				}
				return map[string]any{}, os.WriteFile(file(), []byte("v2"), 0o644)
			})
			var final string
			x.on("final", func(map[string]any) (map[string]any, error) {
				b, err := os.ReadFile(file())
				if err != nil {
					return nil, fmt.Errorf("final: %w", err)
				}
				final = string(b)
				return map[string]any{}, nil
			})
			eng := func(hostState string, force bool) *Engine {
				return New(handoverWorkflow(), s, x,
					WithLogger(iterlog.Nop()),
					WithWorkDir(work),
					WithSandboxHostStateOverride(hostState),
					WithForceResume(force),
					WithSandboxDrivers(map[string]sandbox.DriverConstructor{
						"docker": func() (sandbox.Driver, error) { return d, nil },
					}),
				)
			}
			if err := eng("none", false).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			if b := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(b) != 1 || b[0].Data["banked"] != true {
				t.Fatalf("precondition: the park did not bank the container's scratch: %v", dataOf(b))
			}
			if err := eng("auto", false).Resume(ctx, runID, map[string]any{"ok": true}); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("the resume into a host scratch: want ErrRunPaused at gate2, got %v", err)
			}
			if strings.HasPrefix(d.scratch(), d.root) {
				t.Fatalf("precondition: the resumed sandbox's scratch is not a host directory (%s)", d.scratch())
			}
			restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
			if len(restored) != 1 || restored[0].Data["restored"] != true || restored[0].Data["host_backed"] != true {
				t.Fatalf("the restore into the host directory: %v, want it recorded host-backed", dataOf(restored))
			}
			if err := eng("auto", force).Resume(ctx, runID, map[string]any{"ok": true}); err != nil || final != "v2" {
				t.Fatalf("a later resume on that host: err=%v final=%q, want it run on the host directory's v2", err, final)
			}
			if restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored); len(restored) != 1 {
				t.Fatalf("the older bank was restored again over the host directory: %v", dataOf(restored))
			}
		})
	}
}
