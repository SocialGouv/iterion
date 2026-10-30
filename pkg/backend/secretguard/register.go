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
// refresher that re-reads an unchanged value costs nothing. So is one
// whose value is shorter than the build's MinLen: the build would drop it
// anyway, and letting it take a materialisable slot over would kill the
// declared placeholder outright (the live generation degraded to a
// remnant, the fresh entry filtered away — nothing left to resolve).
//
// A registration whose NAME already exists carries that registration's
// attributes over, through the WHOLE chain of the name's rotations: the
// egress scope (`Hosts`), the sink grants, the env-only and ambient
// classes, the placeholder and the file mount belong to the name's
// declaration, not to any one generation of its value — without this a
// rotated `hosts:`-scoped credential would come back allow-any at the
// Layer-2 egress gate while the launch-time value of the same name stayed
// blocked. When the replaced registration was MATERIALISABLE, the fresh
// value takes its slot over (the declared placeholder keeps resolving —
// to the live value from now on, instead of spending a dead token) and
// the value it replaces stays registered as a redaction-only remnant that
// itself carries the attributes: after the first rotation the name's
// first registration IS such a remnant, so a scan that stopped there
// would re-lose the scope at the second rotation — the normal case of a
// long run (a five-minute ticker under a one-hour token). Rotation sites
// pass one value per name per call; two same-name additions in one call
// both inherit, and the first takeover wins the remnant slot.
//
// Values are registered exactly as handed otherwise — a rotation site
// passes RedactOnly, so the fresh value is scrubbed everywhere but its
// placeholder resolves nowhere. Register is nil-safe and a no-op on it (a
// nil Guard means redaction is disabled).
func (g *Guard) Register(add ...Secret) {
	if g == nil || len(add) == 0 {
		return
	}
	for {
		cur := g.state.Load()
		var fresh []Secret
		takeover := make(map[int]Secret) // inputs index → the replaced value, kept for redaction
		for _, s := range add {
			if s.Value == "" {
				continue
			}
			// Below MinLen the build registers nothing — and the takeover
			// must not fire either, or the live generation would be
			// degraded to a remnant while the fresh entry was filtered
			// away, and the declared placeholder would resolve nowhere.
			if len([]rune(s.Value)) < g.cfg.MinLen {
				continue
			}
			// The raw form is one of the value's encodings: already
			// present means this value is registered (any generation).
			if _, known := cur.literalPlaceholder[s.Value]; known {
				continue
			}
			if rem, idx, took := carryReplaced(cur.inputs, &s); took {
				if _, dup := takeover[idx]; !dup {
					takeover[idx] = rem
				}
			}
			fresh = append(fresh, s)
		}
		if len(fresh) == 0 {
			return
		}
		inputs := make([]Secret, 0, len(cur.inputs)+len(fresh))
		for i, prev := range cur.inputs {
			if rem, ok := takeover[i]; ok {
				inputs = append(inputs, rem)
				continue
			}
			inputs = append(inputs, prev)
		}
		inputs = append(inputs, fresh...)
		next := buildGuardState(inputs, g.cfg)
		// The host materialisation is a runtime RESOLUTION, not a
		// registration: MaterializeHostFiles rewrote the file hints to the
		// paths it wrote on this host, and a rebuild from the declared
		// inputs alone would revert them — the system prompt would announce
		// a path that does not exist here. Carry the resolution forward.
		carryHostPaths(next, cur)
		if g.state.CompareAndSwap(cur, next) {
			return
		}
	}
}

// carryReplaced chains the replaced same-name registration's attributes
// onto s, in place, across the name's WHOLE rotation chain. Returns the
// redaction-only remnant of a replaced MATERIALISABLE registration, its
// inputs index, and whether a takeover happened.
//
// Two passes: the name's live (materialisable) generation first — an
// earlier rotation left its remnant BEFORE the live entry in registration
// order, so a single first-match scan would stop on the attribute-less
// remnant and re-lose the scope at the second rotation. No live
// generation: the name's first registration (itself an
// attribute-carrying remnant after the first takeover) lends its
// attributes, with no takeover — its value stays registered as it is.
func carryReplaced(inputs []Secret, s *Secret) (remnant Secret, idx int, took bool) {
	for i := range inputs {
		prev := inputs[i]
		if prev.Name != s.Name || prev.RedactOnly {
			continue
		}
		carryAttributes(prev, s)
		// The fresh value takes the materialisable slot over; the value it
		// replaces stays registered for redaction only. Its placeholder
		// becomes the _redacted__ form (the name's own now resolves), and
		// its encodings keep matching — a value that was live at launch is
		// scrubbed from every sink for the rest of the run.
		s.RedactOnly = false
		return remnantOf(prev), i, true
	}
	for i := range inputs {
		prev := inputs[i]
		if prev.Name != s.Name {
			continue
		}
		carryAttributes(prev, s)
		return Secret{}, i, false
	}
	return Secret{}, -1, false
}

// carryAttributes transports the name's declaration-level attributes from
// the replaced registration onto s, in place: the egress scope, the sink
// grants, the env-only and ambient classes, the placeholder and the file
// mount belong to the NAME, not to any one generation of its value.
func carryAttributes(prev Secret, s *Secret) {
	s.Hosts = prev.Hosts
	s.Sinks = prev.Sinks
	s.EnvOnly = prev.EnvOnly
	s.Ambient = prev.Ambient
	s.Placeholder = prev.Placeholder
	if prev.FilePath != "" {
		s.FilePath = prev.FilePath
	}
	if prev.Env != "" {
		s.Env = prev.Env
	}
}

// remnantOf builds the redaction-only remnant a replaced MATERIALISABLE
// registration leaves behind: the replaced value, still scrubbed from
// every sink, carrying the name's attributes — after this takeover the
// name's earliest registration is THIS remnant, and the next rotation
// must find the scope on it.
func remnantOf(prev Secret) Secret {
	rem := prev
	rem.RedactOnly = true
	return rem
}

// carryHostPaths re-applies the outgoing generation's host materialisation
// onto the freshly built one: for every name whose file path
// MaterializeHostFiles resolved to a host path, the new generation keeps
// that path (and its file hint), instead of reverting to the declared
// mount path — which on this host may not exist.
func carryHostPaths(next, cur *guardState) {
	if len(cur.filePathByName) == 0 || len(next.filePathByName) == 0 {
		return
	}
	for name, hostPath := range cur.filePathByName {
		declared, ok := next.filePathByName[name]
		if !ok || declared == hostPath {
			continue
		}
		next.filePathByName[name] = hostPath
		for i := range next.fileHints {
			if next.fileHints[i].Name == name && next.fileHints[i].Path == declared {
				next.fileHints[i].Path = hostPath
			}
		}
	}
}
