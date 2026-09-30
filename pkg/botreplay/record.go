//go:build goldens_record

// This file is compiled only under the `goldens_record` build tag, so
// the default `go test ./...` never pulls in the heavy production
// executor stack (runview → model → delegate → sandbox …) and never
// needs LLM credentials. Record mode is the maintenance path that hits
// the real provider and rewrites the committed golden fixtures:
//
//	task test:goldens:record   # requires LLM credentials
package botreplay

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Record drives a single bot node through the production ClawExecutor
// against the real LLM and returns the captured (input → output) fixture.
//
// It strips sandbox specs so the node runs in-process (no docker) and
// chdir's into the repo root so the claw backend's built-in
// filesystem/shell tools read the intended tree — runview.BuildExecutor
// derives its tool workspace from the process working directory. Because
// of the chdir, Record must run serially (the record test does not
// parallelise).
func Record(ctx context.Context, s Scenario) (*Fixture, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}

	wf, err := CompileBot(s.Bot)
	if err != nil {
		return nil, err
	}
	stripSandbox(wf)

	node, ok := wf.Nodes[s.Node]
	if !ok {
		return nil, fmt.Errorf("botreplay: node %q not found in bot %q", s.Node, s.Bot)
	}

	storeDir, err := os.MkdirTemp("", "botreplay-record-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(storeDir)

	st, err := store.New(storeDir)
	if err != nil {
		return nil, err
	}
	runID := "record-" + s.Bot + "-" + s.Name
	if _, err := st.CreateRun(ctx, runID, wf.Name, s.Input); err != nil {
		return nil, err
	}

	// Point the in-process tool workspace at the repo root.
	prevWD, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(root); err != nil {
		return nil, err
	}
	defer func() { _ = os.Chdir(prevWD) }()

	logger := iterlog.New(iterlog.LevelWarn, os.Stderr)
	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: wf,
		// stripSandbox cleared every sandbox declaration above, and no
		// tier is set on this engine: the replay runs in-process, and the
		// executor may predict exactly that.
		SandboxTiersKnown: true,
		Vars:              s.Vars,
		Store:             st,
		RunID:             runID,
		Logger:            logger,
		StoreDir:          storeDir,
	})
	if err != nil {
		return nil, err
	}

	out, err := exec.Execute(ctx, node, s.Input)
	if err != nil {
		return nil, fmt.Errorf("botreplay: execute %s/%s: %w", s.Bot, s.Node, err)
	}

	backend, modelID := nodeBackendModel(node)
	return &Fixture{
		Bot:        s.Bot,
		Scenario:   s.Name,
		Node:       s.Node,
		Backend:    backend,
		Model:      modelID,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Vars:       s.Vars,
		Input:      s.Input,
		Output:     out,
	}, nil
}

// stripSandbox clears workflow- and node-level sandbox specs so a single
// node can run in-process without docker (mirrors the e2e
// compileFixtureStubSafe technique, minus the tool stripping — record
// keeps tools so read-only reviewer/proposer nodes can inspect the repo).
func stripSandbox(wf *ir.Workflow) {
	wf.Sandbox = nil
	for _, node := range wf.Nodes {
		switch n := node.(type) {
		case *ir.AgentNode:
			n.Sandbox = nil
		case *ir.JudgeNode:
			n.Sandbox = nil
		}
	}
}

// nodeBackendModel reports the configured backend + model for provenance
// stamping. Returns empty strings for node kinds without LLM fields.
func nodeBackendModel(node ir.Node) (backend, modelID string) {
	switch n := node.(type) {
	case *ir.AgentNode:
		return n.Backend, n.Model
	case *ir.JudgeNode:
		return n.Backend, n.Model
	}
	return "", ""
}
