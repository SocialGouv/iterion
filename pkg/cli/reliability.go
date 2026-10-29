package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// ReliabilityOptions configures `iterion reliability report|rollback`.
type ReliabilityOptions struct {
	StoreDir string
	// RunID selects one run's compatibility report instead of the
	// fleet baseline. Empty = baseline over the whole store.
	RunID string
	// Source is the run store the baseline reads. Nil means the
	// filesystem store the working directory resolves to (StoreDir
	// overrides which one). A non-nil source is an EXPLICIT operator
	// choice — `iterion reliability report --cloud` wires the Mongo run
	// store in cmd/iterion — and StoreDir is then meaningless, so the
	// two are refused together where the flags are parsed.
	Source ReliabilityRunSource
}

// ReliabilityRunSource is everything a baseline needs from a run store:
// the ids it must try, and the document behind each id.
//
// It exists because step 1 of docs/workflow-reliability-1006.md has to be
// runnable WHERE THE PILOT RUNS. The pilot runs in the cloud, whose runs
// live in Mongo, while `store.ResolveStoreDir` only ever finds the
// filesystem store next to a working directory: a baseline taken there
// counts a developer's local runs and compares them with nothing. A third
// backend (a read replica, a remote API) implements this interface instead
// of adding a branch to the report.
type ReliabilityRunSource interface {
	// Describe names the source in the report. A baseline that does not
	// say which store it measured cannot be compared with the next one,
	// which is the entire purpose of taking it.
	Describe() string
	// CandidateRunIDs returns every id the baseline must TRY to load,
	// including ids whose document may not be readable: a candidate that
	// fails to load is named in Unreadable, never dropped. A baseline
	// that quietly under-counts is worse than none, because the
	// comparison it feeds decides promote-vs-rollback.
	CandidateRunIDs(ctx context.Context) ([]string, error)
	LoadRun(ctx context.Context, id string) (*store.Run, error)
}

// filesystemReliabilitySource is the local store's implementation.
//
// ListRunDirs is intentional, and is the reason CandidateRunIDs is not
// simply ListRuns: ListRuns filters entries whose run.json cannot be
// statted, which is exactly the population the baseline must retain so
// LoadRun can name them in Unreadable.
type filesystemReliabilitySource struct {
	store *store.FilesystemRunStore
	dir   string
}

func (f filesystemReliabilitySource) Describe() string { return "filesystem:" + f.dir }

func (f filesystemReliabilitySource) CandidateRunIDs(ctx context.Context) ([]string, error) {
	return f.store.ListRunDirs(ctx)
}

// LoadRun asks for the LISTING projection. A baseline reads statuses,
// ledgers and context identities — never the text of the workflow a run
// executed — and the listing load leaves that text in the store (#1351).
// On a cloud store that is the difference between a report and a
// document-per-run transfer of recorded sources; here it is the same rule
// stated once, in both backends.
func (f filesystemReliabilitySource) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	return f.store.LoadRunForListing(ctx, id)
}

// ReliabilityReport is the payload of `iterion reliability report`, in
// both human and --json modes.
//
// It exists because the rollout procedure in docs/workflow-reliability-1006.md
// is addressed to an operator, and its step 1 ("capture a baseline") was
// otherwise reachable only from Go code. A pilot whose evidence needs a
// compiler is not a pilot anyone can actually run or roll back.
type ReliabilityReport struct {
	// Source names the run store the baseline was taken over
	// (`filesystem:<dir>`, `mongo:<db>[ tenant=<id>]`). StoreDir keeps
	// its place for the local case and is absent for every other
	// backend, where a store directory does not exist: an empty path
	// would read as "the current directory" to the next operator.
	Source   string `json:"source"`
	StoreDir string `json:"store_dir,omitempty"`
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
//
// ctx is the command's: a baseline over a cloud store is a full scan of a
// remote collection, so the operator's interrupt has to reach it.
func RunReliabilityReport(ctx context.Context, opts ReliabilityOptions, p *Printer) error {
	cfg := reliability.FromEnv()
	out := ReliabilityReport{
		Mode:                  string(cfg.Mode),
		ContextPolicy:         string(cfg.ContextPolicy()),
		RetryCircuitThreshold: cfg.RetryCircuitThreshold,
		RetryCircuitCooldown:  cfg.RetryCircuitCooldown.String(),
	}

	if src := opts.Source; src != nil {
		out.Source = src.Describe()
		return reliabilityReportOver(ctx, src, opts.RunID, out, p)
	}

	cwd, _ := os.Getwd()
	out.StoreDir = store.ResolveStoreDir(cwd, opts.StoreDir)
	out.Source = "filesystem:" + out.StoreDir

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
	return reliabilityReportOver(ctx, filesystemReliabilitySource{store: s, dir: out.StoreDir}, opts.RunID, out, p)
}

// reliabilityReportOver is the ONE walk every backend shares — the
// per-run question or the fleet baseline, with unreadable candidates
// named. Keeping it backend-agnostic is what makes a cloud baseline
// comparable with a local one: the same counters, computed the same way,
// over whichever store the operator pointed at.
func reliabilityReportOver(ctx context.Context, src ReliabilityRunSource, runID string, out ReliabilityReport, p *Printer) error {
	if runID != "" {
		run, err := src.LoadRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("load run %s: %w", runID, err)
		}
		report := reliability.ReportForRun(run)
		out.Run = &report
		return emitReliabilityReport(out, p)
	}

	ids, err := src.CandidateRunIDs(ctx)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	runs := make([]*store.Run, 0, len(ids))
	for _, id := range ids {
		run, err := src.LoadRun(ctx, id)
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
	p.KV("store", out.Source)
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
		p.Blank()
		// The admission row is printed separately, and denials are stated
		// out of how many decisions were recorded: the pilot criterion is
		// that a report cannot hide blocked admissions, and "0 denied"
		// over a population the gate never visited would do exactly that.
		p.Table(
			[]string{"ADMISSIONS_RECORDED", "ADMISSIONS_DENIED", "LEGACY_CONTEXT_RUNS"},
			[][]string{{
				strconv.Itoa(b.AdmissionsRecorded), strconv.Itoa(b.AdmissionsDenied),
				strconv.Itoa(b.LegacyContextRuns),
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

// NewMongoReliabilitySource adapts an open cloud run store into the
// baseline's source.
//
// The store is opened by the caller — cmd/iterion, which owns the single
// cloud wire-up every cloud subcommand shares (cloud_wireup.go) — so a
// baseline cannot reach a cluster through a second, divergent
// configuration path. db names the database in the report's source line;
// empty resolves to the default the store itself applies.
func NewMongoReliabilitySource(ms *mongostore.Store, db, tenantID string) ReliabilityRunSource {
	if db == "" {
		db = mongoDefaultDatabase
	}
	return mongoReliabilitySource{store: ms, db: db, tenant: tenantID}
}

// mongoDefaultDatabase mirrors the default mongostore.New applies to an
// empty Config.Database. It is repeated rather than exported from there
// so the source names the database it ACTUALLY read, which is the whole
// value of the report's source line.
const mongoDefaultDatabase = "iterion"

// mongoReliabilitySource is the cloud run store's half of
// ReliabilityRunSource.
type mongoReliabilitySource struct {
	store  *mongostore.Store
	db     string
	tenant string
}

func (m mongoReliabilitySource) Describe() string {
	if m.tenant == "" {
		return "mongo:" + m.db + " (every tenant)"
	}
	return "mongo:" + m.db + " tenant=" + m.tenant
}

// CandidateRunIDs asks the cloud store for every run id. Unlike the
// filesystem store there is no "entry present, document unreadable"
// population to retain here — a document that fails to decode fails in
// LoadRun, which is where the baseline names it in Unreadable.
func (m mongoReliabilitySource) CandidateRunIDs(ctx context.Context) ([]string, error) {
	return m.store.ListRuns(m.scoped(ctx))
}

// LoadRun asks for the listing projection, for the reason spelled out on
// the filesystem source: the recorded workflow source is the largest
// field on a run document and the baseline reads none of it.
func (m mongoReliabilitySource) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	return m.store.LoadRunForListing(m.scoped(ctx), id)
}

// scoped carries the tenant the way every cloud caller does — on the
// context, where the store's tenant filter reads it. BOTH methods go
// through it: a scoped listing whose loads were unscoped would pair one
// tenant's ids with another tenant's documents.
func (m mongoReliabilitySource) scoped(ctx context.Context) context.Context {
	if m.tenant == "" {
		return ctx
	}
	return store.WithIdentity(ctx, m.tenant, "")
}
