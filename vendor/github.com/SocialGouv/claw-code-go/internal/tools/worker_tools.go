package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/config"
	"github.com/SocialGouv/claw-code-go/internal/runtime/worker"
)

// --- Worker tool definitions ---

func WorkerCreateTool() api.Tool {
	return api.Tool{
		Name:        "worker_create",
		Description: "Create a new worker subprocess.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"cwd":                             {Type: "string", Description: "Working directory for the worker."},
				"trusted_roots":                   {Type: "array", Description: "Trusted root paths for the worker."},
				"auto_recover_prompt_misdelivery": {Type: "boolean", Description: "Auto-recover from prompt misdelivery (default true)."},
			},
			Required: []string{"cwd"},
		},
	}
}

func WorkerGetTool() api.Tool {
	return api.Tool{
		Name:        "worker_get",
		Description: "Get the current state of a worker.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerObserveTool() api.Tool {
	return api.Tool{
		Name:        "worker_observe",
		Description: "Observe screen text from a worker to advance its state machine.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id":   {Type: "string", Description: "The worker ID."},
				"screen_text": {Type: "string", Description: "The screen text to observe."},
			},
			Required: []string{"worker_id", "screen_text"},
		},
	}
}

func WorkerResolveTrustTool() api.Tool {
	return api.Tool{
		Name:        "worker_resolve_trust",
		Description: "Manually resolve the trust gate for a worker.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerAwaitReadyTool() api.Tool {
	return api.Tool{
		Name:        "worker_await_ready",
		Description: "Check whether a worker is ready for a prompt.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerSendPromptTool() api.Tool {
	return api.Tool{
		Name:        "worker_send_prompt",
		Description: "Send a prompt to a worker.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
				"prompt":    {Type: "string", Description: "The prompt to send (optional; uses replay prompt if omitted)."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerRestartTool() api.Tool {
	return api.Tool{
		Name:        "worker_restart",
		Description: "Restart a worker, resetting it to spawning state.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerTerminateTool() api.Tool {
	return api.Tool{
		Name:        "worker_terminate",
		Description: "Terminate a worker.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id": {Type: "string", Description: "The worker ID."},
			},
			Required: []string{"worker_id"},
		},
	}
}

func WorkerObserveCompletionTool() api.Tool {
	return api.Tool{
		Name:        "worker_observe_completion",
		Description: "Observe a completion signal from the provider for a worker.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"worker_id":     {Type: "string", Description: "The worker ID."},
				"finish_reason": {Type: "string", Description: "The completion finish reason."},
				"tokens_output": {Type: "number", Description: "Number of output tokens."},
			},
			Required: []string{"worker_id", "finish_reason", "tokens_output"},
		},
	}
}

// --- Worker Execute functions ---

// workerIDFromInput validates the registry and extracts worker_id. The toolName
// is used only for error prefixing.
func workerIDFromInput(toolName string, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("%s: worker registry not available", toolName)
	}
	id, ok := input["worker_id"].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("%s: 'worker_id' is required", toolName)
	}
	return id, nil
}

// marshalJSON marshals v to indented JSON.
func marshalJSON(v any) string {
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}

func ExecuteWorkerCreate(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("worker_create: worker registry not available")
	}
	cwd, ok := input["cwd"].(string)
	if !ok || cwd == "" {
		return "", fmt.Errorf("worker_create: 'cwd' is required")
	}

	// Merge config-level trusted_roots with per-call overrides.
	// Config provides the default allowlist; per-call roots add on top.
	// Matches Rust's ConfigLoader::default_for(cwd) behavior.
	settings := config.LoadForDir(cwd)
	var configRoots []string
	if len(settings.RawJSON) > 0 {
		fc := config.ExtractFeatureConfig(settings.RawJSON)
		configRoots = fc.TrustedRoots
	}

	var callRoots []string
	if roots, ok := input["trusted_roots"].([]any); ok {
		for _, r := range roots {
			if s, ok := r.(string); ok {
				callRoots = append(callRoots, s)
			}
		}
	}

	// Chain: config roots first, then per-call roots on top.
	// Explicit allocation avoids mutating configRoots' backing array.
	mergedRoots := make([]string, 0, len(configRoots)+len(callRoots))
	mergedRoots = append(mergedRoots, configRoots...)
	mergedRoots = append(mergedRoots, callRoots...)

	autoRecover := true
	if ar, ok := input["auto_recover_prompt_misdelivery"].(bool); ok {
		autoRecover = ar
	}

	return marshalJSON(reg.Create(cwd, mergedRoots, autoRecover)), nil
}

func ExecuteWorkerGet(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_get", input, reg)
	if err != nil {
		return "", err
	}
	w := reg.Get(id)
	if w == nil {
		return "", fmt.Errorf("worker_get: worker not found: %s", id)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerObserve(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_observe", input, reg)
	if err != nil {
		return "", err
	}
	screenText, ok := input["screen_text"].(string)
	if !ok {
		return "", fmt.Errorf("worker_observe: 'screen_text' is required")
	}
	w, err := reg.Observe(id, screenText)
	if err != nil {
		return "", fmt.Errorf("worker_observe: %w", err)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerResolveTrust(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_resolve_trust", input, reg)
	if err != nil {
		return "", err
	}
	w, err := reg.ResolveTrust(id)
	if err != nil {
		return "", fmt.Errorf("worker_resolve_trust: %w", err)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerAwaitReady(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_await_ready", input, reg)
	if err != nil {
		return "", err
	}
	snap, err := reg.AwaitReady(id)
	if err != nil {
		return "", fmt.Errorf("worker_await_ready: %w", err)
	}
	return marshalJSON(snap), nil
}

func ExecuteWorkerSendPrompt(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_send_prompt", input, reg)
	if err != nil {
		return "", err
	}
	var prompt *string
	if p, ok := input["prompt"].(string); ok {
		prompt = &p
	}
	w, err := reg.SendPrompt(id, prompt)
	if err != nil {
		return "", fmt.Errorf("worker_send_prompt: %w", err)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerRestart(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_restart", input, reg)
	if err != nil {
		return "", err
	}
	w, err := reg.Restart(id)
	if err != nil {
		return "", fmt.Errorf("worker_restart: %w", err)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerTerminate(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_terminate", input, reg)
	if err != nil {
		return "", err
	}
	w, err := reg.Terminate(id)
	if err != nil {
		return "", fmt.Errorf("worker_terminate: %w", err)
	}
	return marshalJSON(w), nil
}

func ExecuteWorkerObserveCompletion(input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	id, err := workerIDFromInput("worker_observe_completion", input, reg)
	if err != nil {
		return "", err
	}
	finishReason, ok := input["finish_reason"].(string)
	if !ok || finishReason == "" {
		return "", fmt.Errorf("worker_observe_completion: 'finish_reason' is required")
	}

	var tokensOutput uint64
	switch v := input["tokens_output"].(type) {
	case float64:
		tokensOutput = uint64(v)
	case json.Number:
		n, _ := v.Int64()
		tokensOutput = uint64(n)
	default:
		return "", fmt.Errorf("worker_observe_completion: 'tokens_output' is required and must be a number")
	}

	w, err := reg.ObserveCompletion(id, finishReason, tokensOutput)
	if err != nil {
		return "", fmt.Errorf("worker_observe_completion: %w", err)
	}
	return marshalJSON(w), nil
}
