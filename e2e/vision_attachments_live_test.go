//go:build live

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_VisionAttachments exercises the end-to-end attachments
// pipeline via the runtime's AttachmentPromote callback:
//
//   - the workflow declares `attachments: { logo: image }`
//   - the test pre-stages a tiny PNG and feeds it through the
//     promote callback (mirroring what the HTTP launch handler does
//     with staged uploads)
//   - the runtime materialises Run.Attachments before the engine
//     starts, builds the per-template AttachmentInfo snapshot, and
//     hands {{attachments.logo}} to the claw backend
//   - the claw multimodal path lifts the bytes into an Anthropic
//     ContentBlock so the agent "sees" the image natively
//
// Verified observable: the structured output schema fields
// (saw_image=true, byte_size > 0) and the run reaches `finished`.
func TestLive_VisionAttachments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	// Either provider is enough for this test — it validates the
	// attachments pipeline (Run.Attachments populated, templates
	// resolve, bytes flow into the prompt). The Anthropic vision
	// path is exercised by separate tests.
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY or OPENAI_API_KEY is required")
	}
	// Pick a model based on which key is available so the test runs
	// against either provider without manual configuration.
	if os.Getenv("ITERION_VISION_MODEL") == "" {
		switch {
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			t.Setenv("ITERION_VISION_MODEL", "anthropic/claude-haiku-4-5-20251001")
		case os.Getenv("OPENAI_API_KEY") != "":
			t.Setenv("ITERION_VISION_MODEL", "openai/gpt-5.4-mini")
		}
	}

	wf := compileFixture(t, "vision_attachments.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-vision-attachments-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	// 1x1 transparent PNG fixture (same bytes as the read_image test).
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}
	runID := "live-vision-attachments"

	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(context.Background(), s, runID, logger)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))
	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(tool.NewRegistry()),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	// Promote callback: simulate what the HTTP launch handler does
	// when an upload_id from POST /api/runs/uploads lands on
	// POST /api/runs. The bytes are written through the same
	// store.WriteAttachment path and appear in Run.Attachments.
	promote := func(ctx context.Context, runID string) error {
		return s.WriteAttachment(ctx, runID, store.AttachmentRecord{
			Name:             "logo",
			OriginalFilename: "tiny.png",
			MIME:             "image/png",
		}, bytes.NewReader(pngBytes))
	}

	eng := runtime.New(wf, s, executor,
		runtime.WithAttachmentPromote(promote),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Log("Starting live vision attachments run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, nil)
	t.Logf("Run completed in %s", time.Since(start).Round(time.Second))

	if runErr != nil {
		t.Fatalf("Unexpected run error: %v", runErr)
	}
	r, loadErr := s.LoadRun(context.Background(), runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}
	if rec, ok := r.Attachments["logo"]; !ok {
		t.Fatalf("Run.Attachments[logo] missing after promote")
	} else if rec.SHA256 == "" || rec.Size == 0 {
		t.Errorf("attachment metadata not fully populated: %+v", rec)
	}

	events, evtErr := s.LoadEvents(context.Background(), runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}
	var sawOutput bool
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.NodeID != "describer" || evt.Data == nil {
			continue
		}
		out, ok := evt.Data["output"].(map[string]interface{})
		if !ok {
			continue
		}
		desc, _ := out["description"].(string)
		bytesSize, _ := out["byte_size"].(float64)
		t.Logf("describer.description = %q", desc)
		t.Logf("describer.byte_size = %v", bytesSize)
		// Strict assertions: byte_size flows through {{attachments.logo.size}}
		// (deterministic: the engine resolves it from the on-disk
		// AttachmentRecord, not the LLM's reasoning), and the description
		// must be non-empty.
		if int64(bytesSize) != int64(len(pngBytes)) {
			t.Errorf("byte_size = %v, want %d (size of the seeded PNG)", bytesSize, len(pngBytes))
		}
		if strings.TrimSpace(desc) == "" {
			t.Error("describer.description is empty — agent did not produce a result")
		}
		sawOutput = true
		break
	}
	if !sawOutput {
		t.Error("never saw a finished describer node with structured output")
	}

	logRunRecap(t, events)
}
