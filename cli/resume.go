package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/runtime"
	"github.com/SocialGouv/iterion/store"
)

// ResumeOptions holds the configuration for the resume command.
type ResumeOptions struct {
	RunID       string
	StoreDir    string
	AnswersFile string            // path to JSON answers file
	Answers     map[string]string // --answer key=value overrides
	LogLevel    string            // log level (default: "info", env: ITERION_LOG_LEVEL)
	Executor    runtime.NodeExecutor
}

// RunResume resumes a paused run with human answers.
func RunResume(ctx context.Context, opts ResumeOptions, p *Printer) error {
	if opts.RunID == "" {
		return fmt.Errorf("--run-id is required")
	}

	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	// Load run to get workflow info and validate state.
	r, err := s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	if r.Status != store.RunStatusPausedWaitingHuman {
		return fmt.Errorf("run %q is not paused (status: %s)", opts.RunID, r.Status)
	}
	if r.Checkpoint == nil {
		return fmt.Errorf("run %q has no checkpoint", opts.RunID)
	}

	// Build answers from file and/or flags.
	answers := make(map[string]interface{})

	if opts.AnswersFile != "" {
		fileAnswers, err := ParseAnswersFile(opts.AnswersFile)
		if err != nil {
			return err
		}
		for k, v := range fileAnswers {
			answers[k] = v
		}
	}

	// CLI --answer flags override file values.
	for k, v := range opts.Answers {
		answers[k] = v
	}

	if len(answers) == 0 {
		return fmt.Errorf("no answers provided; use --answers-file or --answer key=value")
	}

	// Recompile the workflow. We need the workflow IR to build the engine.
	// For now, we need the .iter file path. We'll try to find it from the store
	// or require it as an argument.
	// Since the run stores the workflow name but not the file path, we need
	// the user to provide the .iter file again.
	return fmt.Errorf("resume requires the original .iter file; use: iterion resume --run-id %s --file <path.iter> --answers-file <answers.json>", opts.RunID)
}

// RunResumeWithFile resumes a paused run using a workflow file and answers.
func RunResumeWithFile(ctx context.Context, iterFile string, opts ResumeOptions, p *Printer) error {
	if opts.RunID == "" {
		return fmt.Errorf("--run-id is required")
	}

	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	// Load run to validate state.
	r, err := s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	if r.Status != store.RunStatusPausedWaitingHuman {
		return fmt.Errorf("run %q is not paused (status: %s)", opts.RunID, r.Status)
	}

	// Build answers.
	answers := make(map[string]interface{})

	if opts.AnswersFile != "" {
		fileAnswers, err := ParseAnswersFile(opts.AnswersFile)
		if err != nil {
			return err
		}
		for k, v := range fileAnswers {
			answers[k] = v
		}
	}

	for k, v := range opts.Answers {
		answers[k] = v
	}

	if len(answers) == 0 {
		return fmt.Errorf("no answers provided; use --answers-file or --answer key=value")
	}

	// Compile workflow.
	wf, err := compileWorkflow(iterFile)
	if err != nil {
		return err
	}

	// Resolve log level.
	level, err := iterlog.ResolveLevel(opts.LogLevel, "ITERION_LOG_LEVEL")
	if err != nil {
		return err
	}
	logger := iterlog.New(level, os.Stderr)

	executor := opts.Executor
	if executor == nil {
		executor = newDefaultExecutor(wf, nil, s, opts.RunID, logger)
	}

	eng := runtime.New(wf, s, executor, runtime.WithLogger(logger))

	if p.Format == OutputHuman {
		p.Header("Resume: " + opts.RunID)
		p.KV("Workflow", wf.Name)
		p.KV("Node", r.Checkpoint.NodeID)
		p.KV("Log Level", level.String())
		p.Blank()
	}

	err = eng.Resume(ctx, opts.RunID, answers)

	result := map[string]interface{}{
		"run_id":   opts.RunID,
		"workflow": wf.Name,
	}

	if err != nil {
		if errors.Is(err, runtime.ErrRunPaused) {
			result["status"] = "paused_waiting_human"
			if p.Format == OutputJSON {
				p.JSON(result)
			} else {
				p.Line("  Status: PAUSED (waiting for human input again)")
			}
			return nil
		}
		if errors.Is(err, runtime.ErrRunCancelled) {
			result["status"] = "cancelled"
			if p.Format == OutputJSON {
				p.JSON(result)
			} else {
				p.Line("  Status: CANCELLED")
				p.Line("  Detail: %s", err.Error())
			}
			return err
		}
		result["status"] = "failed"
		result["error"] = err.Error()
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Line("  Status: FAILED")
			p.Line("  Error:  %s", err.Error())
			p.Line("  Hint:   use 'iterion inspect --run-id %s --events' for details", opts.RunID)
		}
		return err
	}

	result["status"] = "finished"
	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Line("  Status: FINISHED")
	}
	return nil
}

// ParseAnswerFlags parses a slice of "key=value" strings into a map.
func ParseAnswerFlags(flags []string) (map[string]string, error) {
	answers := make(map[string]string)
	for _, f := range flags {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --answer format %q (expected key=value)", f)
		}
		answers[parts[0]] = parts[1]
	}
	return answers, nil
}
