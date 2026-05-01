package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ResumeOptions holds the configuration for the resume command.
type ResumeOptions struct {
	RunID       string
	StoreDir    string
	AnswersFile string            // path to JSON answers file
	Answers     map[string]string // --answer key=value overrides
	LogLevel    string            // log level (default: "info", env: ITERION_LOG_LEVEL)
	Force       bool              // allow resume despite workflow hash change
	Executor    runtime.NodeExecutor
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

	// Resolve log level early so the logger is available for store creation.
	level, err := iterlog.ResolveLevel(opts.LogLevel, "ITERION_LOG_LEVEL")
	if err != nil {
		return err
	}
	logger := iterlog.New(level, os.Stderr)

	s, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	// Load run to validate state.
	r, err := s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	resumingFromFailure := false
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		// OK — requires answers
	case store.RunStatusFailedResumable, store.RunStatusCancelled:
		resumingFromFailure = true
	default:
		return fmt.Errorf("run %q cannot be resumed (status: %s)", opts.RunID, r.Status)
	}

	// Build answers (required for paused runs, ignored for failed-resumable).
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

	if !resumingFromFailure && len(answers) == 0 {
		return fmt.Errorf("no answers provided; use --answers-file or --answer key=value")
	}

	// Compile workflow and compute hash for change detection.
	wf, wfHash, err := runview.CompileWorkflowWithHash(iterFile)
	if err != nil {
		return err
	}

	executor := opts.Executor
	if executor == nil {
		exec, execErr := runview.BuildExecutor(runview.ExecutorSpec{
			Workflow: wf,
			Vars:     nil,
			Store:    s,
			RunID:    opts.RunID,
			Logger:   logger,
			StoreDir: storeDir,
		})
		if execErr != nil {
			return execErr
		}
		// Re-seed the executor's `vars` from the run's stored inputs so
		// prompt templates can resolve `{{vars.X}}` after resume.
		// Without this, the executor's vars map is nil and references
		// render as the literal `{{vars.X}}` string, silently breaking
		// any prompt that points at a workspace_dir/scope_notes/etc.
		// (RunStarted persisted the same map under run.Inputs; the
		// engine reloads them into rs.vars from the checkpoint, but
		// the executor has its own copy used for prompt rendering.)
		if r != nil && len(r.Inputs) > 0 {
			exec.SetVars(r.Inputs)
		}
		executor = exec
	}

	eng := runtime.New(wf, s, executor, runtime.WithLogger(logger), runtime.WithWorkflowHash(wfHash), runtime.WithFilePath(iterFile), runtime.WithForceResume(opts.Force))

	// Acquire exclusive run lock to prevent concurrent processes.
	lock, err := s.LockRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot acquire run lock: %w", err)
	}
	defer lock.Unlock()

	// Re-check run status under the lock to prevent TOCTOU race.
	r, err = s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot reload run: %w", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman && r.Status != store.RunStatusFailedResumable && r.Status != store.RunStatusCancelled {
		return fmt.Errorf("run %q can no longer be resumed (status: %s)", opts.RunID, r.Status)
	}

	if p.Format == OutputHuman {
		p.Header("Resume: " + opts.RunID)
		p.KV("Workflow", wf.Name)
		if r.Checkpoint != nil {
			p.KV("Node", r.Checkpoint.NodeID)
		}
		if resumingFromFailure {
			p.KV("Resuming from", "failed (re-executing failed node)")
			if r.Error != "" {
				p.KV("Previous error", r.Error)
			}
		}
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
			enrichPausedResult(s, opts.RunID, result)

			if p.Format == OutputJSON {
				p.JSON(result)
			} else {
				p.Line("  Status: PAUSED (waiting for human input again)")
				printPausedQuestions(p, result)
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
