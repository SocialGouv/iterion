package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bankedChain names where an earlier attempt of a run left its commits on
// the forge: the storage branch a terminal bank recorded on the run doc, or
// the attempt ref a pause / interrupted delivery parked its work on.
type bankedChain struct {
	Source string // "bank" (FinalBranch/FinalCommit) | "parked" (run_bank_attempt ref)
	Ref    string // branch name under refs/heads/
	Head   string // the sha the attempt ended on
}

// findBankedChain reads the run doc first — a recorded FinalBranch/FinalCommit
// pair is what `runs merge` trusts — then falls back to the newest parked
// attempt ref on the timeline. Nothing found is the ordinary re-execution of
// a run that never committed, not an error.
func (r *Runner) findBankedChain(ctx context.Context, runID string) (bankedChain, error) {
	run, err := r.cfg.Store.LoadRun(ctx, runID)
	if err != nil {
		return bankedChain{}, err
	}
	if run != nil && run.FinalBranch != "" && run.FinalCommit != "" {
		return bankedChain{Source: "bank", Ref: run.FinalBranch, Head: run.FinalCommit}, nil
	}
	var parked bankedChain
	err = r.cfg.Store.ScanEvents(ctx, runID, func(e *store.Event) bool {
		if e.Type != store.EventRunBankAttempt {
			return true
		}
		ref, _ := e.Data["ref"].(string)
		head, _ := e.Data["head"].(string)
		if ref != "" && head != "" {
			parked = bankedChain{Source: "parked", Ref: ref, Head: head}
		}
		return true
	})
	return parked, err
}

// restoreBankedChain puts a re-executing run back on the commits its earlier
// attempt banked or parked on the forge, so the resumed nodes continue where
// the run stopped instead of on a bare clone of the target branch — the
// checkpoint replays their upstream OUTPUTS, but a campaign that committed
// three passes and reads `git log` to plan the fourth must find them in the
// tree, and the PR tail must have them to deliver.
//
// The checkout is deliberately two-step — the chain's own base first, then a
// fast-forward to its head — so the clone's reflog reads "started from
// <base>": a bot that derives what THIS run changed from the newest reflog
// entry that is not its own commit (docs-refresh's scope gate does) must not
// see the commits the target branch gained meanwhile as the run's work, and
// fail its own scope on them.
//
// Every refusal is loud and leaves the fresh clone as it was: the chain stays
// where the forge holds it, the run just does not continue from it. Returns
// the chain's base when a chain was restored — the baseline the run's commit
// view and its bank are measured against — and "" otherwise.
func (r *Runner) restoreBankedChain(ctx context.Context, msg *queue.RunMessage, dir, tok string, gitEnv []string) string {
	chain, err := r.findBankedChain(ctx, msg.RunID)
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not look for a banked chain to restore (%v) — continuing on the fresh clone", msg.RunID, err)
		r.recordBankRestore(ctx, msg, map[string]any{"restored": false, "reason": "lookup_failed", "error": err.Error()})
		return ""
	}
	if chain.Ref == "" {
		return ""
	}
	data := map[string]any{"source": chain.Source, "ref": chain.Ref, "head": chain.Head}
	refuse := func(reason, detail string) string {
		data["restored"] = false
		data["reason"] = reason
		if detail != "" {
			data["error"] = detail
		}
		r.cfg.Logger.Warn("runner: run %s: NOT restoring the %s chain %s @ %.12s (%s: %s) — continuing on the fresh clone", msg.RunID, chain.Source, chain.Ref, chain.Head, reason, detail)
		r.recordBankRestore(ctx, msg, data)
		return ""
	}
	if err := r.runGitEnv(ctx, dir, tok, gitEnv, "-c", "http.followRedirects=false", "fetch", "--no-tags", "--quiet", "origin", "refs/heads/"+chain.Ref); err != nil {
		return refuse("fetch_failed", err.Error())
	}
	fetched, err := r.runGitOutEnv(ctx, dir, "", nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return refuse("fetch_unreadable", err.Error())
	}
	// The doc names the exact commit the attempt ended on; a ref that moved
	// past it belongs to someone else's push, and continuing from it would
	// silently adopt work this run never did.
	if fetched = strings.TrimSpace(fetched); fetched != chain.Head {
		return refuse("ref_moved", fmt.Sprintf("origin/%s is at %.12s, the run recorded %.12s", chain.Ref, fetched, chain.Head))
	}
	current, err := r.runGitOutEnv(ctx, dir, "", nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return refuse("head_unreadable", err.Error())
	}
	current = strings.TrimSpace(current)
	base, err := r.runGitOutEnv(ctx, dir, "", nil, "merge-base", current, chain.Head)
	if err != nil {
		return refuse("unrelated_history", err.Error())
	}
	base = strings.TrimSpace(base)
	branch, err := r.runGitOutEnv(ctx, dir, "", nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return refuse("branch_unreadable", err.Error())
	}
	if branch = strings.TrimSpace(branch); branch == "" || branch == "HEAD" {
		branch = "iterion/resume"
	}
	if err := r.runGit(ctx, dir, "", "checkout", "--quiet", "-B", branch, base); err != nil {
		return refuse("checkout_failed", err.Error())
	}
	if err := r.runGit(ctx, dir, "", "merge", "--ff-only", "--quiet", chain.Head); err != nil {
		// Put the tree back where the clone left it: a half-restored
		// workspace is worse than a fresh one.
		if rerr := r.runGit(ctx, dir, "", "checkout", "--quiet", "-B", branch, current); rerr != nil {
			r.cfg.Logger.Error("runner: run %s: could not return to the fresh clone head %.12s after a failed fast-forward: %v", msg.RunID, current, rerr)
		}
		return refuse("fast_forward_failed", err.Error())
	}
	data["restored"] = true
	data["base"] = base
	data["from"] = current
	data["base_moved"] = base != current
	r.cfg.Logger.Info("runner: run %s: restored the %s chain %s @ %.12s on its base %.12s (target branch %s at %.12s)", msg.RunID, chain.Source, chain.Ref, chain.Head, base, branch, current)
	r.recordBankRestore(ctx, msg, data)
	return base
}

// recordBankRestore puts the restore (or its refusal) on the run's timeline.
// Observational: a store that refuses the append must not sink the run, but
// the fact still reaches the pod log.
func (r *Runner) recordBankRestore(ctx context.Context, msg *queue.RunMessage, data map[string]any) {
	if _, err := r.cfg.Store.AppendEvent(ctx, msg.RunID, store.Event{
		Type: store.EventRunWorkspaceBankRestored,
		Data: data,
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit %s: %v (data: %v)", msg.RunID, store.EventRunWorkspaceBankRestored, err, data)
	}
}
