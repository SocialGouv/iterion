package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ReliabilityOptions configures `iterion reliability report|rollback`.
type ReliabilityOptions struct {
	StoreDir string
	// RunID selects one run's compatibility report instead of the
	// fleet baseline. Empty = baseline over the whole store.
	RunID string
}

// ReliabilityReport is the payload of `iterion reliability report`, in
// both human and --json modes.
//
// It exists because the rollout procedure in docs/workflow-reliability-1006.md
// is addressed to an operator, and its step 1 ("capture a baseline") was
// otherwise reachable only from Go code. A pilot whose evidence needs a
// compiler is not a pilot anyone can actually run or roll back.
type ReliabilityReport struct {
	StoreDir string `json:"store_dir"`
	// Mode / ContextPolicy are the RESOLVED values — what the launch
	// surfaces apply right now for this process's environment, through the
	// same reliability.ContextPolicyFromEnv the launches use. Reading them
	// back is the point: an operator promoting or rolling back needs to see
	// which of the two variables won.
	//
	// Watcher-cursor persistence is deliberately NOT reported here as a
	// resolved knob: it is not one. A coordinator persists its cursor only
	// when its launch surface wired a cursor store (Coordinator.cursorStore),
	// so the honest answer is per-run — watcher_cursor_count on a run's own
	// report and runs_with_watcher_cursors in the baseline — never a global
	// flag an operator would read as "on everywhere".
	Mode                  string `json:"mode"`
	ContextPolicy         string `json:"context_policy"`
	RetryCircuitThreshold int    `json:"retry_circuit_threshold"`
	RetryCircuitCooldown  string `json:"retry_circuit_cooldown"`

	Baseline *reliability.Baseline            `json:"baseline,omitempty"`
	Run      *reliability.CompatibilityReport `json:"run,omitempty"`
	// Unreadable names run directories whose run.json could not be loaded.
	// They are reported rather than skipped in silence: a baseline that
	// quietly under-counts is worse than none, because the comparison it
	// feeds is what decides promote-vs-rollback.
	Unreadable []string `json:"unreadable,omitempty"`
}

// RunReliabilityReport implements `iterion reliability report`. It is
// strictly read-only — it never writes a run, and never changes the mode
// it reports.
func RunReliabilityReport(opts ReliabilityOptions, p *Printer) error {
	cfg := reliability.FromEnv()
	cwd, _ := os.Getwd()
	out := ReliabilityReport{
		StoreDir:              store.ResolveStoreDir(cwd, opts.StoreDir),
		Mode:                  string(cfg.Mode),
		ContextPolicy:         string(cfg.ContextPolicy()),
		RetryCircuitThreshold: cfg.RetryCircuitThreshold,
		RetryCircuitCooldown:  cfg.RetryCircuitCooldown.String(),
	}

	// A store with no runs is a legitimate baseline (a fresh deployment
	// about to start a pilot), not an error — but it cannot answer a
	// question about a specific run.
	hasRuns, err := reliabilityStoreHasRuns(out.StoreDir)
	if err != nil {
		return fmt.Errorf("inspect run store: %w", err)
	}
	if !hasRuns {
		if opts.RunID != "" {
			return UserInputError(fmt.Errorf("run %q: no run store at %s", opts.RunID, out.StoreDir))
		}
		empty := reliability.Summarize(nil)
		out.Baseline = &empty
		return emitReliabilityReport(out, p)
	}

	s, err := store.New(out.StoreDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}
	ctx := context.Background()

	if opts.RunID != "" {
		run, err := s.LoadRun(ctx, opts.RunID)
		if err != nil {
			return fmt.Errorf("load run %s: %w", opts.RunID, err)
		}
		report := reliability.ReportForRun(run)
		out.Run = &report
		return emitReliabilityReport(out, p)
	}

	// ListRunDirs is intentional: ListRuns filters entries whose run.json
	// cannot be statted. A baseline must retain those candidates so the load
	// below can name them in Unreadable instead of silently under-counting.
	ids, err := s.ListRunDirs(ctx)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	runs := make([]*store.Run, 0, len(ids))
	for _, id := range ids {
		run, err := s.LoadRun(ctx, id)
		if err != nil {
			out.Unreadable = append(out.Unreadable, id)
			continue
		}
		runs = append(runs, run)
	}
	baseline := reliability.Summarize(runs)
	out.Baseline = &baseline
	return emitReliabilityReport(out, p)
}

// reliabilityStoreHasRuns distinguishes a genuinely absent store from one
// the operator cannot inspect. The reliability report is rollout evidence:
// treating EACCES, EIO, or any other stat failure as an empty baseline would
// turn missing evidence into a false success.
func reliabilityStoreHasRuns(storeDir string) (bool, error) {
	if _, err := os.Stat(storeDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(filepath.Join(storeDir, "runs")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func emitReliabilityReport(out ReliabilityReport, p *Printer) error {
	if p.Format == OutputJSON {
		p.JSON(out)
		return nil
	}
	p.Header("Reliability rollout")
	p.KV("store", out.StoreDir)
	p.KV("mode", out.Mode)
	p.KV("context policy", out.ContextPolicy)
	p.KV("retry circuit", fmt.Sprintf("threshold %d, cooldown %s", out.RetryCircuitThreshold, out.RetryCircuitCooldown))

	if r := out.Run; r != nil {
		p.Blank()
		p.Header("Run " + r.RunID)
		p.KV("workflow hash", r.WorkflowHash)
		p.KV("context", describeRunContext(r))
		p.KV("admission", strconv.FormatBool(r.AdmissionRecorded))
		p.KV("publishing nodes", strconv.Itoa(r.PublishingNodeCount))
		p.KV("corrections", strconv.Itoa(r.CorrectionEpisodeCount))
		p.KV("watcher cursors", strconv.Itoa(r.WatcherCursorCount))
		p.KV("rollback safe", strconv.FormatBool(r.RollbackSafe))
	}

	if b := out.Baseline; b != nil {
		p.Blank()
		p.Header("Baseline")
		p.Table(
			[]string{"TOTAL", "FINISHED", "FAILED", "FAILED_RESUMABLE", "PAUSED", "QUEUED", "RETRY_ARMED", "CORRECTIONS", "WATCHERS"},
			[][]string{{
				strconv.Itoa(b.Total), strconv.Itoa(b.Finished), strconv.Itoa(b.Failed),
				strconv.Itoa(b.FailedResumable), strconv.Itoa(b.Paused), strconv.Itoa(b.Queued),
				strconv.Itoa(b.RetryArmed), strconv.Itoa(b.RunsWithCorrections),
				strconv.Itoa(b.RunsWithWatcherCursors),
			}},
		)
	}

	if len(out.Unreadable) > 0 {
		p.Blank()
		p.Line("  %d run directories could not be read and are NOT counted above: %v",
			len(out.Unreadable), out.Unreadable)
	}
	return nil
}

// describeRunContext spells out the pre-pilot state rather than letting a
// zero version read as "successful" — the compatibility rule the rollout
// doc states: a missing field means pre-pilot, never green.
func describeRunContext(r *reliability.CompatibilityReport) string {
	if r.LegacyContext {
		return "legacy (pre-pilot: no execution context recorded)"
	}
	return fmt.Sprintf("v%d, policy %s", r.ContextVersion, r.ContextPolicy)
}

// RunReliabilityRollback implements `iterion reliability rollback`. It
// PRINTS the plan; it never mutates the environment or any run, because
// the variables are set where the launch surfaces read them (a pod spec, a
// service unit, a shell) and not by a one-shot CLI process.
func RunReliabilityRollback(p *Printer) error {
	plan := reliability.FromEnv().Rollback()
	if p.Format == OutputJSON {
		p.JSON(plan)
		return nil
	}
	p.Header("Reliability rollback plan")
	p.KV("disable enforcement", strconv.FormatBool(plan.DisableEnforcement))
	p.KV("preserve evidence", strconv.FormatBool(plan.PreserveEvidence))
	p.Blank()
	for _, action := range plan.Actions {
		p.Line("  - %s", action)
	}
	return nil
}
