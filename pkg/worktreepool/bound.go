package worktreepool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultBudget is how many worktrees a store may park before the bound
// starts reclaiming.
//
// It counts only worktrees NO LIVE RUN owns: a run that is executing
// always keeps its checkout, so raising parallelism never fights the
// budget and the number is not a cap on concurrency. What it caps is the
// PRESERVED pool — the checkouts a failed or finished run left behind for
// inspection, which nothing else ever reclaims.
//
// Eight is chosen against what a worktree costs rather than what feels
// tidy: it is a full checkout of the repository, and on a repo that
// vendors its dependencies that is hundreds of megabytes each. Eight of
// them is a bad afternoon; the thirty-two that prompted this bound filled
// a 16 GB tmpfs and started killing processes.
const DefaultBudget = 8

// BudgetEnv is the operator's dial. A count, or `off`.
const BudgetEnv = "ITERION_WORKTREE_POOL_MAX"

// BudgetReport is what one pass of the bound did and what it could not do.
// Every field is reported rather than logged inside, so the caller decides
// how loud to be.
type BudgetReport struct {
	// Budget is the resolved ceiling; 0 means the bound is off.
	Budget int
	// Total is every directory in the pool, live runs included.
	Total int
	// Held is how many a live run still owns. They are never candidates
	// and never count against the budget.
	Held int
	// Before and After are the count the budget applies to — the pool
	// minus what live runs hold — on either side of the pass.
	Before int
	After  int
	// Reclaimed names the worktrees the pass took, oldest first.
	Reclaimed []Entry
	// BytesReclaimed is what those directories occupied.
	BytesReclaimed int64
	// Spared counts what was over budget and could not be taken, by the
	// reason it was refused. This is the actionable half of the report:
	// it is what turns "still over budget" into a sentence naming a
	// command.
	Spared map[string]int
	// Errors are per-entry failures. The pass never aborts on one.
	Errors []error
}

// OverBudget reports whether the pool is still above its ceiling after the
// pass — the condition worth telling an operator about.
func (r BudgetReport) OverBudget() bool {
	return r.Budget > 0 && r.After > r.Budget
}

// Summary is a one-line account of what the pass could not reclaim and
// why, ordered so the reason an operator can act on comes first. Empty
// when nothing was spared.
func (r BudgetReport) Summary() string {
	var parts []string
	for _, o := range sparedLabels {
		if n := r.Spared[o.reason]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, o.label))
		}
	}
	return strings.Join(parts, ", ")
}

var sparedLabels = []struct {
	reason string
	label  string
}{
	{SkipLevel, "carry uncommitted work or content git cannot account for"},
	{SkipResumable, "belong to runs `iterion resume` would restart"},
	{SkipUnlanded, "hold commits only the run's own refs keep"},
	{SkipNested, "hold a repository of their own"},
	{SkipRunActive, "are owned by a live run"},
}

// Remedy is the command that would clear what the bound refused.
//
// It is a DRY RUN — `iterion clean` reports by default and deletes only
// with --apply — because every category the bound leaves behind is one it
// judged too costly to take unattended, and the operator deserves to see
// the list before it goes. The flags are derived from what is actually
// blocking rather than fixed, so the line an operator copies is the one
// that would work on THEIR pool.
//
// Empty when nothing was spared for a reason a command could clear.
func (r BudgetReport) Remedy(storeDir string) string {
	needsLevel := r.Spared[SkipLevel] > 0
	needsResumable := r.Spared[SkipResumable] > 0
	if !needsLevel && !needsResumable && r.Spared[SkipUnlanded] == 0 && r.Spared[SkipNested] == 0 {
		return ""
	}
	cmd := "iterion clean --store-dir " + storeDir
	if needsLevel {
		// `moderate` takes a merged worktree's uncommitted files;
		// `aggressive` is what an orphan or an un-adopted branch needs.
		// Naming the lower one keeps the suggestion the smaller step.
		cmd += " --level moderate"
	}
	if needsResumable {
		cmd += " --include-resumable"
	}
	return cmd
}

// ResolveBudget reads the operator's ceiling.
//
// Precedence is the one the repo uses everywhere: env → default. There is
// no DSL field and no CLI flag on purpose — the pool belongs to the STORE,
// not to any one workflow or run, so letting a `.bot` set it would let one
// bot decide how much disk every other bot's leftovers may hold.
//
// `off`, `none`, `0` and `-1` all disable the bound. Anything else that is
// not a non-negative integer is an error rather than a silent fallback: a
// typo'd ceiling that quietly reverts to the default is exactly the shape
// of bug this bound exists to prevent.
func ResolveBudget() (int, error) {
	raw := strings.TrimSpace(os.Getenv(BudgetEnv))
	if raw == "" {
		return DefaultBudget, nil
	}
	switch strings.ToLower(raw) {
	case "off", "none":
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a count (a non-negative integer, or `off`)", BudgetEnv, raw)
	}
	if n <= 0 {
		return 0, nil
	}
	return n, nil
}

// EnforceBudget brings a store's worktree pool back under its ceiling by
// reclaiming what can be reclaimed with nothing lost, oldest first.
//
// It is deliberately not a cap: it never refuses to create a worktree, and
// it never deletes anything an operator might still want. When it cannot
// get back under the ceiling it says so, with the reasons — because the
// alternative shapes are both worse. Refusing would fail a run the
// operator asked for, over a directory some OTHER run left behind. Taking
// more would destroy the uncommitted output that "preserved for
// inspection" exists to keep.
//
// The cheap path is the common one: a ReadDir, and nothing else when the
// pool is small. Classification costs several git calls and a full tree
// walk per entry, so it is only spent once the pool is already over.
func EnforceBudget(storeDir string, budget int, opts SweepOptions) (BudgetReport, error) {
	report := BudgetReport{Budget: budget, Spared: map[string]int{}}
	if budget <= 0 {
		return report, nil
	}

	// The count gate. A store at or under its ceiling pays one ReadDir per
	// run and never touches git — which is what makes it acceptable to ask
	// this question on the path that creates every worktree.
	names, err := poolEntries(storeDir)
	if err != nil || len(names) == 0 {
		return report, err
	}
	report.Total = len(names)
	if len(names) <= budget {
		// Held stays 0 here: under the ceiling nothing needs to be told
		// apart, and reading every run.json to say so would spend the
		// cost this early return exists to avoid.
		report.Before, report.After = len(names), len(names)
		return report, nil
	}

	opts.Apply = true
	opts.WithRuns = false // a run's record is history; only its checkout is the cost
	opts.Admit = evictionAdmission()
	opts.OlderThan = 0 // the pool is over NOW; an age floor would defer the whole point
	opts.KeepLast = 0
	// Sizes are reported for what is DELETED, never for what is spared:
	// measuring costs a walk of every file in a full checkout, and in the
	// degraded state — a pool over budget that nothing can reclaim — that
	// walk would be paid on every launch to produce a number no one reads.
	opts.MeasureSpared = false

	// A live run's worktree is not a leftover, so it neither counts
	// against the budget nor gets offered to the sweep. Without this a
	// store running nine bots at once would report itself permanently over
	// budget and warn on every single launch. The question is answered
	// from run status alone — no git — because it decides whether the
	// expensive half runs at all.
	statuses := loadRunStatuses(storeDir)
	now := opts.now()
	candidates := make([]Entry, 0, len(names))
	for _, name := range names {
		path := filepath.Join(PoolDir(storeDir), name)
		if st, ok := statuses[name]; ok && ownsWorktree(st) {
			report.Held++
			continue
		}
		e := Entry{RunID: name, Path: path, StoreDir: storeDir}
		if info, err := os.Stat(path); err == nil {
			e.ModTime = info.ModTime()
		}
		candidates = append(candidates, e)
	}
	report.Before = len(candidates)
	report.After = report.Before
	if report.Before <= budget {
		return report, nil
	}
	SortOldestFirst(candidates)

	// Classification is lazy, and that is the difference between a bound
	// that is affordable and one that taxes every launch: it costs several
	// git calls per entry, so a healthy pool pays for the few it actually
	// reclaims rather than for all of them. Only a pool that cannot be
	// brought back down walks its whole length — the price of a warning
	// that can name what is holding it up.
	excess := report.Before - budget
	taken := 0
	for i := range candidates {
		if taken >= excess {
			break
		}
		e := classify(candidates[i].Path, candidates[i].RunID, storeDir, statuses, opts.ScanOptions, now)
		if e.SkipReason != "" {
			report.Spared[e.SkipReason]++
			continue
		}
		swept := Sweep([]Entry{e}, opts)
		report.Errors = append(report.Errors, swept.Errors...)
		for _, sp := range swept.Spared {
			report.Spared[sp.SkipReason]++
		}
		if len(swept.Deleted) == 0 {
			continue
		}
		report.Reclaimed = append(report.Reclaimed, swept.Deleted...)
		report.BytesReclaimed += swept.BytesReclaimed
		taken++
	}
	report.After = report.Before - taken
	return report, nil
}

// poolEntries lists the per-run directories a store parks, skipping what
// is not one. Dot-prefixed entries hold state that belongs to the store
// rather than to any run — the same exclusion the sweep makes — so they
// must not count against a budget the sweep could never bring back down.
func poolEntries(storeDir string) ([]string, error) {
	entries, err := os.ReadDir(PoolDir(storeDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read worktree pool %s: %w", PoolDir(storeDir), err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
