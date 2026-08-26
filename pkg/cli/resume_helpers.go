package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// buildResumeAnswers merges --answers-file and --answer flag entries
// into a single map suitable for [runtime.Engine.Resume]. The CLI
// flags win over the file (callers can patch a single answer without
// re-editing the JSON). Returns an error when no answers are provided
// for a paused-waiting-human resume; failed-resumable resumes don't
// require answers since the engine just re-executes the failing node.
func buildResumeAnswers(opts ResumeOptions, resumingFromFailure bool) (map[string]any, error) {
	answers := make(map[string]any)
	if opts.AnswersFile != "" {
		fileAnswers, err := ParseAnswersFile(opts.AnswersFile)
		if err != nil {
			return nil, err
		}
		for k, v := range fileAnswers {
			answers[k] = v
		}
	}
	for k, v := range opts.Answers {
		answers[k] = v
	}
	if !resumingFromFailure && len(answers) == 0 {
		return nil, fmt.Errorf("no answers provided; use --answers-file or --answer key=value")
	}
	return answers, nil
}

// resumeOpenWorkflow re-materialises the workflow source for a resume
// session. When the run was launched from a `.botz` archive or
// directory, the bundle is re-opened so prompts/skills/attachments
// flow back into the resumed engine; the returned iterFile is then
// the in-bundle .bot path. Otherwise the .bot file is compiled
// directly from disk.
//
// The caller MUST defer the returned cleanup (no-op on the
// non-bundle path).
func resumeOpenWorkflow(r *store.Run, iterFile string) (*ir.Workflow, string, string, *bundle.Bundle, func() error, error) {
	cleanup := func() error { return nil }
	if r != nil && r.BundlePath != "" {
		bundleHandle, bundleCleanup, openErr := openResumeBundle(r.BundlePath)
		if openErr != nil {
			return nil, "", iterFile, nil, cleanup, openErr
		}
		if bundleHandle != nil {
			cleanup = bundleCleanup
			wf, hash, compileErr := runview.CompileBundleWorkflow(bundleHandle.IterPath, bundleHandle)
			if compileErr != nil {
				return nil, "", bundleHandle.IterPath, bundleHandle, cleanup, compileErr
			}
			return wf, hash, bundleHandle.IterPath, bundleHandle, cleanup, nil
		}
	}
	wf, hash, compileErr := runview.CompileWorkflowWithHash(iterFile)
	if compileErr != nil {
		return nil, "", iterFile, nil, cleanup, compileErr
	}
	return wf, hash, iterFile, nil, cleanup, nil
}

// openResumeBundle re-opens a previously-launched bundle by path,
// distinguishing archive (.botz) from extracted directory layouts.
// Returns (nil, no-op, nil) when the path is neither — the caller
// then falls back to a plain .bot compile.
func openResumeBundle(path string) (*bundle.Bundle, func() error, error) {
	opened, _, kind, cleanup, err := openBundleOrFile(path)
	if err != nil {
		switch kind {
		case bundle.KindBundle:
			return nil, cleanup, fmt.Errorf("resume: re-open bundle %s: %w (original archive may have moved — re-supply with --file)", path, err)
		case bundle.KindBundleDir:
			return nil, cleanup, fmt.Errorf("resume: re-open bundle dir %s: %w", path, err)
		default:
			return nil, cleanup, fmt.Errorf("resume: re-detect bundle: %w", err)
		}
	}
	return opened, cleanup, nil
}

// buildResumeExecutor constructs the default ClawExecutor for the
// resume path unless opts.Executor already supplies one. The
// executor's `vars` map is re-seeded from the run's stored Inputs so
// prompt templates can still resolve `{{vars.X}}` after resume —
// without this, the executor's vars map starts nil and references
// render as the literal `{{vars.X}}` string, silently breaking any
// prompt that points at a workspace_dir/scope_notes/etc. The engine
// reloads vars from the checkpoint into rs.vars, but the executor
// keeps its own copy for prompt rendering.
func buildResumeExecutor(
	opts ResumeOptions,
	wf *ir.Workflow,
	s store.RunStore,
	storeDir string,
	logger *iterlog.Logger,
	r *store.Run,
	hookObservers []func(store.Event),
) (runtime.NodeExecutor, error) {
	if opts.Executor != nil {
		return opts.Executor, nil
	}
	modelOverrides, err := model.ParseModelOverrides(opts.ModelFor, opts.BackendFor, opts.EffortFor)
	if err != nil {
		return nil, err
	}
	runFallback, err := ir.ParseRunFallbackFlag(opts.Fallback)
	if err != nil {
		return nil, err
	}
	// Inherit the model the run was LAUNCHED with, then let this attempt's
	// flags win per field. Without the inheritance a resume silently drops
	// back to the .bot's own model:/backend:/reasoning_effort:, so an
	// operator's launch-time choice would last exactly one node — and the
	// detached studio path resumes through this same CLI.
	if r != nil {
		modelOverrides = modelOverrides.MergeOver(runview.ModelOverridesFromRun(r.ModelOverrides))
	}
	localStore, localSealer, err := localSecretsForRun(len(wf.Secrets) > 0, storeDir, logger)
	if err != nil {
		return nil, err
	}
	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Workflow: wf,
		Vars:     nil,
		Store:    s,
		RunID:    opts.RunID,
		Logger:   logger,
		StoreDir: storeDir,
		// Backend-hook events (assistant_text, tool_*, llm_*) fire only
		// this seam — the declared-supervisor hub rides it (run.go's
		// executor does the same).
		EventObservers: hookObservers,
		// Mirror the run path's inbox wiring: without these binders a
		// resumed CLI run never DRAINS queued messages, so supervisor
		// steering (and operator chat) would sit `queued` forever.
		Inbox:           &model.StoreInboxBinder{Store: s},
		AsyncAsk:        &model.StoreAsyncAskBinder{Store: s},
		Permission:      opts.Permission,
		PermissionAllow: opts.PermissionAllow,
		PermissionAsk:   opts.PermissionAsk,
		PermissionDeny:  opts.PermissionDeny,
		RunFallback:     runFallback,
		AutoMemory:      opts.AutoMemory,
		// Same resolution the studio resume path uses, so the two surfaces
		// key a bot's memory identically.
		BotID:          runview.BotIDForRun(r),
		ModelOverrides: modelOverrides,
		LocalSecrets:   localStore,
		LocalSealer:    localSealer,
	})
	if err != nil {
		return nil, err
	}
	if r != nil && len(r.Inputs) > 0 {
		exec.SetVars(r.Inputs)
	}
	return exec, nil
}
