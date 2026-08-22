package worktreepool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

// DefaultEnforcementTimeout bounds the maintenance paid synchronously by
// a run launch. The explicit clean command has no aggregate deadline; its
// operator asked to wait for the complete inventory.
const DefaultEnforcementTimeout = 10 * time.Second

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
	// Spared counts candidates examined while the pool was over budget and
	// not taken, by reason. Refusals do not consume the excess, so a pass
	// may examine and count more entries than the initial excess.
	Spared map[string]int
	// Errors are per-entry failures. The pass never aborts on one.
	Errors []error
	// Incomplete means the caller's context stopped classification before
	// every over-budget candidate could be considered. No unchecked entry
	// is deleted or reported under a guessed refusal reason.
	Incomplete bool
	// needsIncludeResumable is independent of Spared: one dirty resumable
	// entry has SkipLevel as its primary reason, but the remedy still needs
	// both --level moderate and --include-resumable.
	needsIncludeResumable bool
	// needsAggressive retains the landing fact when the primary refusal is
	// dirtiness. A dirty own-branch checkout needs aggressive, not the
	// moderate level sufficient for a dirty merged checkout.
	needsAggressive bool
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
	{SkipOrphan, "are directories git cannot account for"},
	{SkipIgnored, "contain gitignored files requiring operator review"},
	{SkipIterionHeldOnly, "are held only by run-scoped checkpoint refs"},
	{SkipUnlanded, "hold commits no ref outside the run's own keeps"},
	{SkipNested, "hold a repository of their own"},
	{SkipRunActive, "are owned by a live run"},
	{SkipRemovalFailed, "could not be removed (see the preceding error)"},
}

// Remedy is the command that would clear what the bound refused, or empty
// when no command would.
//
// It is a DRY RUN — `iterion clean` reports by default and deletes only
// with --apply — because every category the bound leaves behind is one it
// judged too costly to take unattended, and the operator deserves to see
// the list before it goes. The flags are derived from what is actually
// blocking rather than fixed, so the line an operator copies is the one
// that would work on THEIR pool.
//
// `unlanded` and `nested-repo` deliberately produce nothing: no level of
// `iterion clean` takes them either, so offering the command for those
// would promise a reclamation it cannot perform. Summary still names
// them, which is the honest answer — that pool needs git, by hand.
func (r BudgetReport) Remedy(storeDir string) string {
	needsLevel := r.Spared[SkipLevel] > 0
	needsAggressive := r.needsAggressive || r.Spared[SkipOrphan] > 0 || r.Spared[SkipIterionHeldOnly] > 0
	needsResumable := r.needsIncludeResumable || r.Spared[SkipResumable] > 0
	needsIgnoredReview := r.Spared[SkipIgnored] > 0
	if !needsLevel && !needsAggressive && !needsResumable && !needsIgnoredReview {
		return ""
	}
	// The bound deliberately has no age floor. Mirror that policy in the
	// suggested command: `iterion clean` otherwise defaults to 168h and
	// would spare the recent entries that made the pool exceed its budget.
	cmd := "iterion clean --store-dir " + shellQuoteArg(storeDir) + " --older-than 0"
	if needsAggressive {
		cmd += " --level aggressive"
	} else if needsLevel {
		// `moderate` takes a merged worktree's uncommitted files, which is
		// the common case; an orphan needs `aggressive`. Naming the lower
		// one keeps the suggestion the smaller step, and the dry run shows
		// what it still leaves behind.
		cmd += " --level moderate"
	}
	if needsResumable {
		cmd += " --include-resumable"
	}
	return cmd
}

func shellQuoteArg(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

const refusalMemoTTL = 5 * time.Minute

type memoizedRefusal struct {
	reason    string
	resumable bool
	landing   string
	expires   time.Time
}

// refusalMemo remembers only decisions NOT to delete. Reusing a stale
// refusal can delay reclamation until the short TTL expires, but it can
// never turn a changed worktree into a deletion. That one-way safety is
// what makes memoization acceptable on the run-start path.
var refusalMemo = struct {
	sync.Mutex
	entries map[string]memoizedRefusal
}{entries: map[string]memoizedRefusal{}}

func memoizedPoolRefusal(path string, now time.Time) (memoizedRefusal, bool) {
	refusalMemo.Lock()
	defer refusalMemo.Unlock()
	v, ok := refusalMemo.entries[path]
	if ok && !now.Before(v.expires) {
		delete(refusalMemo.entries, path)
		return memoizedRefusal{}, false
	}
	return v, ok
}

func rememberPoolRefusal(e Entry, now time.Time) {
	if e.SkipReason == "" || e.SkipReason == SkipVanished || e.SkipReason == SkipRunActive {
		return
	}
	refusalMemo.Lock()
	if len(refusalMemo.entries) >= 256 {
		for path, cached := range refusalMemo.entries {
			if !now.Before(cached.expires) {
				delete(refusalMemo.entries, path)
			}
		}
		// Keep 256 as a hard cap even when every entry is still fresh.
		if len(refusalMemo.entries) >= 256 {
			var earliestPath string
			var earliestExpiry time.Time
			for path, cached := range refusalMemo.entries {
				if earliestPath == "" || cached.expires.Before(earliestExpiry) {
					earliestPath, earliestExpiry = path, cached.expires
				}
			}
			delete(refusalMemo.entries, earliestPath)
		}
	}
	refusalMemo.entries[e.Path] = memoizedRefusal{
		reason: e.SkipReason, resumable: e.resumable, landing: e.Landing, expires: now.Add(refusalMemoTTL),
	}
	refusalMemo.Unlock()
}

func forgetPoolRefusal(path string) {
	refusalMemo.Lock()
	delete(refusalMemo.entries, path)
	refusalMemo.Unlock()
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
	// Git reports absolute paths. Normalise at the package boundary so a
	// documented relative --store-dir cannot make every linked worktree
	// look like an unrelated orphan.
	storeDir = AbsPath(storeDir)
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
	ctx := opts.ctx()

	opts.Apply = true
	opts.WithRuns = false // a run's record is history; only its checkout is the cost
	// Resuming restarts in this checkout. Giving that up must remain an
	// explicit operator choice, regardless of the options a caller passed.
	opts.IncludeResumable = false
	opts.reclaimStaleNonTerminal = true
	opts.Admit = evictionAdmission()
	opts.OlderThan = 0 // the pool is over NOW; an age floor would defer the whole point
	opts.KeepLast = 0
	// Sizes are reported for what is DELETED, never for what is spared:
	// measuring costs a walk of every file in a full checkout, and in the
	// degraded state — a pool over budget that nothing can reclaim — that
	// walk would be paid on every launch to produce a number no one reads.
	opts.MeasureSpared = false
	// Ignored files can be generated output or an operator's .env. Read
	// them in the same status pass as dirtiness and leave that judgment to
	// an explicit clean invocation rather than deleting them unattended.
	opts.SkipIgnoredEntries = false
	opts.RefuseIgnoredEntries = true

	// A live run's worktree is not a leftover, so it neither counts
	// against the budget nor gets offered to the sweep. Without this a
	// store running nine bots at once would report itself permanently over
	// budget and warn on every single launch. The question is answered
	// from run status alone — no git — because it decides whether the
	// expensive half runs at all.
	statuses := loadRunStatuses(storeDir, names)
	now := opts.now()
	candidates := make([]Entry, 0, len(names))
	for _, name := range names {
		path := filepath.Join(PoolDir(storeDir), name)
		if st, ok := statuses[name]; ok && ownsWorktree(st) && runLockHeld(storeDir, name) {
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
	markIncomplete := func(err error) {
		if report.Incomplete {
			return
		}
		report.Incomplete = true
		report.Errors = append(report.Errors,
			fmt.Errorf("worktree pool classification stopped before completion: %w", err))
	}

	// Classification is lazy, and refusals alone are memoized briefly. A
	// healthy pool pays for the few entries it actually reclaims; a degraded
	// pool pays once per long-lived process to name what is holding it up,
	// then reuses those safe refusals on launches during the TTL. Eligibility
	// is never cached and one-shot CLI processes deliberately share no state.
	excess := report.Before - budget
	taken, vanished := 0, 0
	for i := range candidates {
		if err := ctx.Err(); err != nil {
			markIncomplete(err)
			break
		}
		if taken+vanished >= excess {
			break
		}
		if memo, ok := memoizedPoolRefusal(candidates[i].Path, now()); ok {
			e := candidates[i]
			e.SkipReason, e.resumable, e.Landing = memo.reason, memo.resumable, memo.landing
			report.recordSpared(e)
			continue
		}
		if opts.beforeClassification != nil {
			opts.beforeClassification(candidates[i].Path)
		}
		e := classify(candidates[i].Path, candidates[i].RunID, storeDir, statuses, opts.ScanOptions, now)
		if err := ctx.Err(); err != nil {
			markIncomplete(err)
			break
		}
		// A concurrent pass may have removed the directory since ReadDir.
		// Git silence for a missing cwd looks unlanded; report a harmless
		// disappearance instead of memoizing that alarm verdict.
		if _, statErr := os.Lstat(candidates[i].Path); os.IsNotExist(statErr) {
			vanished++
			forgetPoolRefusal(candidates[i].Path)
			continue
		}
		if e.SkipReason != "" {
			rememberPoolRefusal(e, now())
			report.recordSpared(e)
			continue
		}
		swept := Sweep([]Entry{e}, opts)
		report.Errors = append(report.Errors, swept.Errors...)
		// Cancellation inside the last-moment re-check is a stopped pass,
		// not evidence that the entry is unlanded or nested. A completed
		// deletion is still reported accurately; otherwise leave the entry
		// unclassified and stop here.
		if err := ctx.Err(); err != nil && len(swept.Deleted) == 0 {
			markIncomplete(err)
			break
		}
		for _, sp := range swept.Spared {
			if sp.SkipReason == SkipVanished {
				vanished++
				forgetPoolRefusal(sp.Path)
				continue
			}
			rememberPoolRefusal(sp, now())
			report.recordSpared(sp)
		}
		// A failed removal is not a classification refusal and must not be
		// memoized, but it still explains why this pass remains over budget.
		for range swept.Failed {
			report.Spared[SkipRemovalFailed]++
		}
		if len(swept.Deleted) == 0 {
			continue
		}
		report.Reclaimed = append(report.Reclaimed, swept.Deleted...)
		report.BytesReclaimed += swept.BytesReclaimed
		forgetPoolRefusal(e.Path)
		taken++
		if err := ctx.Err(); err != nil {
			markIncomplete(err)
			break
		}
	}
	report.After = report.Before - taken - vanished
	return report, nil
}

func (r *BudgetReport) recordSpared(e Entry) {
	r.Spared[e.SkipReason]++
	if e.Landing == LandingOwnBranch || e.Landing == LandingOrphan {
		r.needsAggressive = true
	}
	if e.resumable && e.SkipReason != SkipUnlanded && e.SkipReason != SkipNested {
		r.needsIncludeResumable = true
	}
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
