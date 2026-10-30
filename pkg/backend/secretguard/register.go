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
// refresher that re-reads an unchanged value costs nothing.
//
// A registration whose NAME already exists carries that registration's
// attributes over: a seam that swaps a value must not widen or drop what
// the value it replaces carried. The egress scope (`Hosts`), the sink
// grants, the env-only and ambient classes, the placeholder and the file
// mount belong to the name's declaration, not to any one generation of
// its value — without this a rotated `hosts:`-scoped credential would
// come back allow-any at the Layer-2 egress gate while the launch-time
// value of the same name stayed blocked. When the replaced registration
// was MATERIALISABLE, the fresh value takes its slot over (the declared
// placeholder keeps resolving — to the live value from now on, instead of
// spending a dead token) and the value it replaces stays registered as a
// redaction-only remnant: still scrubbed from every sink, its placeholder
// resolving nowhere. Rotation sites pass one value per name per call;
// two same-name additions in one call both inherit, and the first
// takeover wins the remnant slot.
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

// carryReplaced transports the replaced same-name registration's
// attributes onto s, in place. Returns the redaction-only remnant of a
// replaced MATERIALISABLE registration, its inputs index, and whether a
// takeover happened (no takeover when no same-name match, or when the
// match was already redaction-only — its old value stays registered as it
// is).
func carryReplaced(inputs []Secret, s *Secret) (remnant Secret, idx int, took bool) {
	for i := range inputs {
		prev := inputs[i]
		if prev.Name != s.Name {
			continue
		}
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
		if prev.RedactOnly {
			return Secret{}, i, false
		}
		// The fresh value takes the materialisable slot over; the value it
		// replaces stays registered for redaction only. Its placeholder
		// becomes the _redacted__ form (the name's own now resolves), and
		// its encodings keep matching — a value that was live at launch is
		// scrubbed from every sink for the rest of the run.
		s.RedactOnly = false
		return Secret{Name: prev.Name, Value: prev.Value, RedactOnly: true}, i, true
	}
	return Secret{}, -1, false
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
