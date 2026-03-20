package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/recipe"
	"github.com/iterion-ai/iterion/runtime"
	"github.com/iterion-ai/iterion/store"
)

// RunOptions holds the configuration for the run command.
type RunOptions struct {
	File     string               // .iter file path
	Recipe   string               // recipe JSON file path (alternative to File)
	Vars     map[string]string    // --var key=value overrides
	RunID    string               // explicit run ID (auto-generated if empty)
	StoreDir string               // store directory (default: .iterion)
	Timeout  time.Duration        // maximum run duration (0 = no limit)
	Executor runtime.NodeExecutor // pluggable executor (nil = stub)
}

// RunRun executes a workflow or recipe and reports the outcome.
func RunRun(ctx context.Context, opts RunOptions, p *Printer) error {
	// Resolve store.
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}
	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot create store: %w", err)
	}

	// Resolve run ID.
	runID := opts.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}

	// Build engine: either from recipe or raw workflow.
	var eng *runtime.Engine
	var workflowName string

	if opts.Recipe != "" {
		// Load recipe.
		spec, err := recipe.LoadFile(opts.Recipe)
		if err != nil {
			return fmt.Errorf("cannot load recipe: %w", err)
		}

		// Resolve the .iter file path from recipe or option.
		iterFile := opts.File
		if iterFile == "" {
			iterFile = spec.WorkflowRef.Path
		}
		if iterFile == "" {
			return fmt.Errorf("recipe %q does not specify a workflow path; provide --file", spec.Name)
		}

		wf, err := compileWorkflow(iterFile)
		if err != nil {
			return err
		}

		executor := opts.Executor
		if executor == nil {
			executor = &stubExecutor{}
		}

		eng, err = runtime.NewFromRecipe(spec, wf, s, executor)
		if err != nil {
			return err
		}
		workflowName = spec.Name + " (" + wf.Name + ")"
	} else {
		if opts.File == "" {
			return fmt.Errorf("provide a .iter file or --recipe")
		}

		wf, err := compileWorkflow(opts.File)
		if err != nil {
			return err
		}

		executor := opts.Executor
		if executor == nil {
			executor = &stubExecutor{}
		}

		eng = runtime.New(wf, s, executor)
		workflowName = wf.Name
	}

	// Build run inputs from vars.
	inputs := make(map[string]interface{})
	for k, v := range opts.Vars {
		inputs[k] = v
	}

	// Apply timeout to context if specified.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Execute.
	if p.Format == OutputHuman {
		p.Header("Run: " + workflowName)
		p.KV("Run ID", runID)
		p.KV("Store", storeDir)
		if opts.Timeout > 0 {
			p.KV("Timeout", FormatDuration(opts.Timeout))
		}
		p.Blank()
	}

	err = eng.Run(ctx, runID, inputs)

	// Build result.
	runResult := map[string]interface{}{
		"run_id":   runID,
		"workflow": workflowName,
		"store":    storeDir,
	}

	if err != nil {
		if errors.Is(err, runtime.ErrRunPaused) {
			runResult["status"] = "paused_waiting_human"
			if p.Format == OutputJSON {
				p.JSON(runResult)
			} else {
				p.Line("  Status: PAUSED (waiting for human input)")
				p.Line("  Resume: iterion resume --run-id %s --store-dir %s --answers-file <file>", runID, storeDir)
			}
			return nil
		}
		if errors.Is(err, runtime.ErrRunCancelled) {
			runResult["status"] = "cancelled"
			if p.Format == OutputJSON {
				p.JSON(runResult)
			} else {
				p.Line("  Status: CANCELLED")
				p.Line("  Detail: %s", err.Error())
			}
			return err
		}
		runResult["status"] = "failed"
		runResult["error"] = err.Error()
		if p.Format == OutputJSON {
			p.JSON(runResult)
		} else {
			p.Line("  Status: FAILED")
			p.Line("  Error:  %s", err.Error())
			p.Line("  Hint:   use 'iterion inspect --run-id %s --events' for details", runID)
		}
		return err
	}

	runResult["status"] = "finished"
	if p.Format == OutputJSON {
		p.JSON(runResult)
	} else {
		p.Line("  Status: FINISHED")
	}
	return nil
}

// stubExecutor is a no-op executor used when no real executor is provided.
// It returns the input as output, allowing validation of the workflow graph
// traversal without real LLM calls.
type stubExecutor struct{}

func (s *stubExecutor) Execute(_ context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	output := make(map[string]interface{})
	for k, v := range input {
		output[k] = v
	}
	// For judges, provide default boolean fields so edges can be evaluated.
	if node.Kind == ir.NodeJudge {
		output["approved"] = true
		output["compliant"] = true
	}
	return output, nil
}

// ParseVarFlags parses a slice of "key=value" strings into a map.
func ParseVarFlags(flags []string) (map[string]string, error) {
	vars := make(map[string]string)
	for _, f := range flags {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --var format %q (expected key=value)", f)
		}
		vars[parts[0]] = parts[1]
	}
	return vars, nil
}

// ParseAnswersFile reads a JSON file containing answer key-value pairs.
func ParseAnswersFile(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read answers file: %w", err)
	}
	var answers map[string]interface{}
	if err := json.Unmarshal(data, &answers); err != nil {
		return nil, fmt.Errorf("cannot parse answers file: %w", err)
	}
	return answers, nil
}
