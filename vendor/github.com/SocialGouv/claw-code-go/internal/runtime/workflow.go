package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"

	"github.com/SocialGouv/claw-code-go/internal/runtime/task"
	"github.com/SocialGouv/claw-code-go/internal/tools"
)

// The workflow tool runs a JavaScript orchestration script on an embedded
// event loop: agent(prompt, opts) spawns a background subagent (a real
// task-registry entry) and returns a Promise of its transcript; parallel()
// and pipeline() are provided as a JS prelude on top of it, so their
// semantics are exactly the documented ones (thunks/stages that throw drop
// to null; pipeline has NO barrier between stages). Deterministic control
// flow lives in the script; the model decides only what to spawn.

const (
	// workflowScriptCap bounds the script source.
	workflowScriptCap = 512 * 1024
	// workflowDefaultTimeout / workflowMaxTimeout bound one run.
	workflowDefaultTimeout = 15 * time.Minute
	workflowMaxTimeout     = 60 * time.Minute
	// workflowAgentCap is the runaway backstop on total agent() calls.
	workflowAgentCap = 200
	// workflowConcurrency caps concurrently-running agents per workflow.
	workflowConcurrency = 8
	// workflowPollInterval is the task-completion polling cadence.
	workflowPollInterval = 100 * time.Millisecond
)

// workflowPrelude defines parallel() and pipeline() in pure JS on top of the
// promise-returning agent() binding, mirroring the documented semantics.
const workflowPrelude = `
function parallel(thunks) {
	return Promise.all(thunks.map(t => Promise.resolve().then(t).catch(e => { log("parallel: thunk failed: " + e); return null; })));
}
async function pipeline(items, ...stages) {
	return Promise.all(items.map(async (item, i) => {
		let v = item;
		for (const s of stages) {
			if (v === null || v === undefined) return null;
			try { v = await s(v, item, i); }
			catch (e) { log("pipeline: stage failed for item " + i + ": " + e); return null; }
		}
		return v;
	}));
}
`

// exportConstRe tolerates the `export const meta = {...}` opener used by
// workflow scripts written for module contexts (goja evaluates scripts, not
// ES modules).
var exportConstRe = regexp.MustCompile(`(?m)^\s*export\s+const\s+`)

type workflowOutcome struct {
	result interface{}
	err    error
}

// executeWorkflow is the workflow tool's dispatch entry.
func (loop *ConversationLoop) executeWorkflow(ctx context.Context, input map[string]any) (string, error) {
	if loop.TaskRegistry == nil {
		return "", fmt.Errorf("workflow: task registry not available")
	}
	script, _ := input["script"].(string)
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("workflow: 'script' is required")
	}
	if len(script) > workflowScriptCap {
		return "", fmt.Errorf("workflow: script too large (%d bytes > %d)", len(script), workflowScriptCap)
	}
	script = exportConstRe.ReplaceAllString(script, "const ")

	timeout := workflowDefaultTimeout
	if secs, ok := input["timeout_seconds"].(float64); ok && secs > 0 {
		timeout = time.Duration(secs) * time.Second
		if timeout > workflowMaxTimeout {
			timeout = workflowMaxTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		logMu      sync.Mutex
		logLines   []string
		agentCount atomic.Int64
		vmRef      atomic.Pointer[goja.Runtime]
	)
	appendLog := func(line string) {
		logMu.Lock()
		logLines = append(logLines, line)
		logMu.Unlock()
	}

	el := eventloop.NewEventLoop()
	el.Start()
	defer el.Stop()

	done := make(chan workflowOutcome, 1)
	report := func(o workflowOutcome) {
		select {
		case done <- o:
		default:
		}
	}

	el.RunOnLoop(func(vm *goja.Runtime) {
		vmRef.Store(vm)
		_ = vm.Set("args", input["args"])
		_ = vm.Set("log", func(msg string) { appendLog(msg) })
		_ = vm.Set("phase", func(title string) { appendLog("── phase: " + title) })
		_ = vm.Set("agent", loop.workflowAgentBinding(ctx, el, vm, &agentCount, appendLog))
		_ = vm.Set("__wfResolve", func(v goja.Value) { report(workflowOutcome{result: v.Export()}) })
		_ = vm.Set("__wfReject", func(v goja.Value) { report(workflowOutcome{err: fmt.Errorf("workflow script failed: %s", v.String())}) })

		if _, err := vm.RunString(workflowPrelude); err != nil {
			report(workflowOutcome{err: fmt.Errorf("workflow: prelude: %w", err)})
			return
		}
		wrapped := "(async () => {\n" + script + "\n})().then(__wfResolve, __wfReject);"
		if _, err := vm.RunString(wrapped); err != nil {
			report(workflowOutcome{err: fmt.Errorf("workflow: script error: %w", err)})
		}
	})

	var out workflowOutcome
	select {
	case out = <-done:
	case <-ctx.Done():
		if vm := vmRef.Load(); vm != nil {
			vm.Interrupt("workflow timed out or was cancelled")
		}
		return "", fmt.Errorf("workflow: %w after spawning %d agent(s)", ctx.Err(), agentCount.Load())
	}
	if out.err != nil {
		return "", out.err
	}

	logMu.Lock()
	logCopy := append([]string(nil), logLines...)
	logMu.Unlock()

	payload := map[string]any{
		"result": out.result,
		"log":    logCopy,
		"agents": agentCount.Load(),
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("workflow: encode result (script returned a non-serializable value): %w", err)
	}
	return string(encoded), nil
}

// workflowAgentBinding builds the agent(prompt, opts) JS function: it spawns
// a background subagent (quiet — the workflow aggregates, no per-agent
// completion reminder) and returns a Promise of its transcript, resolved on
// the event loop when the task reaches a terminal state.
func (loop *ConversationLoop) workflowAgentBinding(ctx context.Context, el *eventloop.EventLoop, vm *goja.Runtime, agentCount *atomic.Int64, appendLog func(string)) func(goja.FunctionCall) goja.Value {
	sem := make(chan struct{}, workflowConcurrency)
	return func(call goja.FunctionCall) goja.Value {
		prompt := strings.TrimSpace(call.Argument(0).String())
		if prompt == "" || prompt == "undefined" {
			panic(vm.NewTypeError("agent: prompt (string) is required"))
		}
		label := ""
		subagentType := "general-purpose"
		model := ""
		if opts, ok := call.Argument(1).Export().(map[string]interface{}); ok {
			if v, ok := opts["label"].(string); ok {
				label = v
			}
			if v, ok := opts["subagent_type"].(string); ok && v != "" {
				subagentType = v
			}
			if v, ok := opts["model"].(string); ok {
				model = v
			}
		}
		if label == "" {
			label = tools.SlugifyAgentLabel(prompt)
		}
		n := agentCount.Add(1)
		if n > workflowAgentCap {
			panic(vm.NewTypeError(fmt.Sprintf("agent: workflow agent cap reached (%d)", workflowAgentCap)))
		}

		promise, resolve, reject := vm.NewPromise()
		settle := func(fn func(interface{}) error, v interface{}) {
			el.RunOnLoop(func(*goja.Runtime) { _ = fn(v) })
		}

		spec := &tools.AgentSpec{
			AgentID:      fmt.Sprintf("wf-agent-%d", n),
			Name:         label,
			Description:  label,
			Prompt:       prompt,
			SubagentType: subagentType,
			Model:        model,
		}

		go func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				settle(reject, "workflow cancelled before agent start")
				return
			}
			defer func() { <-sem }()

			appendLog(fmt.Sprintf("agent #%d %q started", n, label))
			t, err := loop.spawnSubagent(spec, false)
			if err != nil {
				settle(reject, err.Error())
				return
			}
			for {
				got, ok := loop.TaskRegistry.Get(t.TaskID)
				if ok && got.Status.IsTerminal() {
					output, _ := loop.TaskRegistry.Output(t.TaskID)
					if got.Status == task.StatusCompleted {
						appendLog(fmt.Sprintf("agent #%d %q completed", n, label))
						settle(resolve, output)
					} else {
						appendLog(fmt.Sprintf("agent #%d %q %s", n, label, got.Status))
						settle(reject, fmt.Sprintf("agent %q %s: %s", label, got.Status, excerpt(output, 200)))
					}
					return
				}
				select {
				case <-ctx.Done():
					settle(reject, "workflow cancelled while agent was running")
					return
				case <-time.After(workflowPollInterval):
				}
			}
		}()

		return vm.ToValue(promise)
	}
}
