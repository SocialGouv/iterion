package secretguard

// Register adds values to a LIVE guard — the mid-run rotation seam
// (#2756): the runner's refresh loops re-read a rotated forge token or
// `as: file` secret minutes after launch, and the guard built at launch
// never saw the new value, so a bot that prints the credential file lands
// the rotated token verbatim in the run's log and events. Register closes
// that: the fresh value is redacted from every sink from this call on.
//
// The swap is copy-on-write: a new state generation is built from the
// current registrations plus the additions and exchanged atomically, so a
// concurrent Redact keeps reading a complete, consistent matcher (an
// in-place mutation under a lock is not an option — Redact re-enters
// itself through heuristicRedact → recurseDecode, and a writer waiting
// between two read-locks would deadlock).
//
// A registration whose raw value is already known is skipped, so a
// refresher that re-reads an unchanged value costs nothing. Values are
// registered exactly as handed — a rotation site passes RedactOnly, so
// the fresh value is scrubbed everywhere but its placeholder resolves
// nowhere. Register is nil-safe and a no-op on it (a nil Guard means
// redaction is disabled).
func (g *Guard) Register(add ...Secret) {
	if g == nil || len(add) == 0 {
		return
	}
	for {
		cur := g.state.Load()
		var fresh []Secret
		for _, s := range add {
			if s.Value == "" {
				continue
			}
			// The raw form is one of the value's encodings: already
			// present means this value is registered (any generation).
			if _, known := cur.literalPlaceholder[s.Value]; known {
				continue
			}
			fresh = append(fresh, s)
		}
		if len(fresh) == 0 {
			return
		}
		inputs := make([]Secret, 0, len(cur.inputs)+len(fresh))
		inputs = append(inputs, cur.inputs...)
		inputs = append(inputs, fresh...)
		next := buildGuardState(inputs, g.cfg)
		if g.state.CompareAndSwap(cur, next) {
			return
		}
	}
}
