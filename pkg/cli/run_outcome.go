package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runInteractiveResumeLoop drives the human-pause TTY resume cycle: as
// long as the engine returns [runtime.ErrRunPaused] and stdin is a
// terminal (and the caller hasn't opted out), it loads the pause
// checkpoint, prompts for answers, and resumes. Returns the final
// engine error (which may itself be a fresh ErrRunPaused if the user
// dropped out of the loop via --no-interactive, but in practice the
// condition is rechecked each iteration so we exit cleanly).
//
// Each error path REPLACES the inbound err with a wrapped one so the
// outer reporter sees the real failure instead of the stale
// ErrRunPaused — a corrupt checkpoint or missing interaction file
// would otherwise hide behind a paused_waiting_human status with no
// signal of what actually broke.
func runInteractiveResumeLoop(
	ctx context.Context,
	eng *runtime.Engine,
	s store.RunStore,
	runID string,
	noInteractive bool,
	err error,
) error {
	for errors.Is(err, runtime.ErrRunPaused) && !noInteractive && IsTTY() {
		r, loadErr := s.LoadRun(context.Background(), runID)
		if loadErr != nil {
			return fmt.Errorf("interactive resume: load run: %w", loadErr)
		}
		if r.Checkpoint == nil {
			return fmt.Errorf("interactive resume: run %q has no checkpoint", runID)
		}
		interaction, loadErr := s.LoadInteraction(context.Background(), runID, r.Checkpoint.InteractionID)
		if loadErr != nil {
			return fmt.Errorf("interactive resume: load interaction: %w", loadErr)
		}
		answers, promptErr := PromptHumanAnswers(interaction)
		if promptErr != nil {
			return fmt.Errorf("interactive resume: prompt answers: %w", promptErr)
		}
		err = eng.Resume(ctx, runID, answers)
	}
	return err
}

// reportRunOutcome formats the run's terminal status (or pause) for
// stdout and returns the exit error the CLI should bubble up. Paused
// runs return nil — they're a normal CI lifecycle that an operator
// will resume out-of-band — while cancelled and failed runs surface
// the underlying error.
//
// userFile is the user-facing path used to populate the JSON "file"
// field when present (paused result only); pass "" to omit it.
func reportRunOutcome(
	p *Printer,
	s store.RunStore,
	runID, storeDir, userFile string,
	err error,
	runResult map[string]any,
) error {
	if errors.Is(err, runtime.ErrRunPaused) {
		runResult["status"] = "paused_waiting_human"
		if userFile != "" {
			runResult["file"] = userFile
		}
		enrichPausedResult(s, runID, runResult)
		if p.Format == OutputJSON {
			p.JSON(runResult)
		} else {
			p.Line("  Status: PAUSED (waiting for human input)")
			printPausedQuestions(p, runResult)
			p.Line("  Resume: iterion resume --run-id %s --store-dir %s --answers-file <file>", runID, storeDir)
		}
		return nil
	}
	return reportNonPausedOutcome(p, runID, err, runResult)
}

// reportResumeOutcome is the equivalent of [reportRunOutcome] for the
// `iterion resume` path: the paused branch reports a slightly
// different status string and skips the resume-hint line (the user
// just ran resume — printing the same command back is noise).
// Cancelled / failed / finished branches are identical and shared
// via [reportNonPausedOutcome].
func reportResumeOutcome(
	p *Printer,
	s store.RunStore,
	runID string,
	err error,
	result map[string]any,
) error {
	if errors.Is(err, runtime.ErrRunPaused) {
		result["status"] = "paused_waiting_human"
		enrichPausedResult(s, runID, result)
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Line("  Status: PAUSED (waiting for human input again)")
			printPausedQuestions(p, result)
		}
		return nil
	}
	return reportNonPausedOutcome(p, runID, err, result)
}

// reportNonPausedOutcome handles the cancelled / failed / finished
// branches shared by run + resume. Pause is caller-specific (different
// status text + resume hint) and is handled outside this helper.
func reportNonPausedOutcome(
	p *Printer,
	runID string,
	err error,
	result map[string]any,
) error {
	if err == nil {
		result["status"] = "finished"
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Line("  Status: FINISHED")
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
		p.Line("  Hint:   use 'iterion inspect --run-id %s --events' for details", runID)
	}
	return err
}
