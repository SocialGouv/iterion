package worktreepool

// admission decides whether a classification admits a deletion.
//
// The two callers ask different questions of the SAME facts, which is why
// the predicate travels with the sweep instead of being baked into the
// classifier:
//
//   - `iterion clean` asks "is this inside the level the operator chose",
//     where the level is a statement about what they are willing to lose.
//   - the pool bound asks "can this be taken with nothing lost at all",
//     because nobody asked for it and nobody is watching.
//
// Neither may invent its own notion of what a worktree IS — landing,
// dirty and durablyHeld are produced once, by inspectGit, for both.
type admission func(landing string, dirty, durablyHeld bool) (skipReason string, ok bool)

// admission resolves the predicate a scan or sweep judges with. Nil means
// the level ladder, so a caller that only sets Level gets today's
// behaviour and nothing else has to know the bound exists.
func (o ScanOptions) admission() admission {
	if o.Admit != nil {
		return o.Admit
	}
	return levelAdmission(o.Level)
}

// levelAdmission is `iterion clean`'s rule: take what the chosen level
// admits, and name the level as the reason for everything else.
func levelAdmission(level Level) admission {
	return func(landing string, dirty, _ bool) (string, bool) {
		if levelRank[level] < requiredLevel(landing, dirty) {
			return SkipLevel, false
		}
		return "", true
	}
}

// evictionAdmission is the pool bound's rule: take only what can be taken
// with NOTHING lost.
//
// It is deliberately not one of the levels, and it is neither a subset nor
// a superset of any of them:
//
//   - It takes an `own-branch` worktree that a durable ref holds, which
//     `conservative` spares. Nothing is lost there — the ref lives in the
//     parent repository and outlives the directory — and this is the case
//     the bound exists for: a run that failed before committing anything
//     leaves a full checkout sitting at a commit its own branch already
//     points at. Sparing those is how a pool reaches 32 entries and 12 GB.
//   - It refuses anything DIRTY, which `moderate` takes. Uncommitted files
//     are the run's own output, and "preserved for inspection" is a
//     feature: an eviction nobody asked for must never be the thing that
//     destroys them.
//   - It refuses `orphan`, which `aggressive` takes. git cannot say what
//     is in there, and "we could not tell" is not a licence to delete.
//
// A worktree held ONLY by iterion's own per-run checkpoint refs is refused
// too: those are reaped with the run, so they are not a durable holder.
func evictionAdmission() admission {
	return func(landing string, dirty, durablyHeld bool) (string, bool) {
		// inspectGit reports an orphan as dirty because git cannot account
		// for any of its contents. Name the stronger fact first: moderate
		// does not take orphans, while aggressive does.
		if landing == LandingOrphan {
			return SkipOrphan, false
		}
		if landing == LandingOwnBranch && !durablyHeld {
			return SkipIterionHeldOnly, false
		}
		if dirty {
			// Reported as `needs-higher-level` rather than a reason of its
			// own: what the operator does next IS to pick a level, and
			// `iterion clean --level moderate` is exactly the answer.
			return SkipLevel, false
		}
		switch landing {
		case LandingMerged:
			return "", true
		case LandingOwnBranch:
			return "", true // the non-durable case returned above
		}
		return SkipLevel, false
	}
}
