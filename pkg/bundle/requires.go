package bundle

import (
	"fmt"
	"strconv"
	"strings"
)

// The engine contract a bundle may state about the build that runs it.
//
// A bot and the engine that evaluates it ship in two different halves of a
// deployment: the bundle can be pushed in a second (a platform override, a
// team bundle, a `.botz` from a git URL) while the runner image is pinned by
// digest and moves only on a deploy. Nothing used to hold the two together —
// a bot using a builtin the runner's evaluator did not have COMPILED (a
// function call parses generically) and died at its first evaluation.
//
// `requires:` is the declaration that closes it:
//
//	requires:
//	  iterion: ">= 3.112.14"
//
// It is deliberately a VERSION FLOOR rather than a feature list. The push
// guard's only channel to the fleet is a version string (the build each
// runner stamps on the runs it executes — store.Run.RunnerVersion), so a
// feature list would have to be translated back into a version before it
// could be checked there, which is the version comparison with an extra map
// in front of it. The cost is honest and named: a fork or a backport carrying
// the feature under a different version string reads as too old, and a build
// with no orderable version reads as EngineUnknown — reported, never silently
// passed.
//
// The manifest decoder is STRICT (yaml.UnmarshalStrict), which gives the
// contract a property worth relying on: a build too old to know `requires:`
// refuses the whole manifest rather than dropping the requirement, and so
// does a build that does not know a key some future release adds under it.
// A requirement iterion cannot read is never a requirement iterion ignores.

// Requires is the bundle's engine contract. Every key is a separate
// requirement and ALL of them must hold; an unknown key is refused by the
// strict manifest decoder.
type Requires struct {
	// Iterion is the minimum engine build, as an `>= X.Y.Z` floor (the
	// operator is optional — a bare version means the same thing).
	Iterion string `yaml:"iterion,omitempty"`
}

// atLeast is the one operator the floor accepts. Anything else is refused
// with this string in the message, so the accepted grammar is discoverable
// from the error alone.
const atLeast = ">="

// EngineConstraint is a parsed version floor.
type EngineConstraint struct {
	// Raw is the constraint as authored, for error messages.
	Raw string
	// Min is the floor's dotted numeric components.
	Min []int
}

// ParseEngineConstraint reads the `requires.iterion` grammar: an optional
// `>=`, then a dotted numeric version with an optional `v` prefix. Every
// other shape is an explicit error naming what is accepted — a constraint
// this build cannot enforce must not be mistaken for one it satisfies.
func ParseEngineConstraint(s string) (EngineConstraint, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return EngineConstraint{}, fmt.Errorf("engine requirement is empty (expected %q, e.g. %q)", atLeast+" X.Y.Z", atLeast+" 3.112.14")
	}
	rest := raw
	if strings.HasPrefix(rest, atLeast) {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, atLeast))
	}
	parts, ok := numericVersionParts(rest)
	if !ok {
		return EngineConstraint{}, fmt.Errorf(
			"engine requirement %q is not a version floor: expected %q or a bare version (dotted numbers, optional leading %q) — no other operator is enforced, so accepting one would promise a check that never runs",
			raw, atLeast+" X.Y.Z", "v")
	}
	return EngineConstraint{Raw: raw, Min: parts}, nil
}

// Validate parses every declared requirement, so a manifest carrying one
// iterion cannot read fails at decode instead of at launch.
func (r *Requires) Validate() error {
	if r == nil {
		return nil
	}
	if _, err := ParseEngineConstraint(r.Iterion); err != nil {
		return fmt.Errorf("requires.iterion: %w", err)
	}
	return nil
}

// EngineVerdict is the answer to "may this build run this bundle".
type EngineVerdict int

const (
	// EngineOK: no requirement, or the build satisfies it.
	EngineOK EngineVerdict = iota
	// EngineTooOld: the build is ORDERED below the floor. The only verdict
	// that refuses.
	EngineTooOld
	// EngineUnknown: a requirement exists but the build's own version is not
	// orderable (a `dev` build, a fork's naming scheme). The check could not
	// run — which is reported, never taken for a pass.
	EngineUnknown
)

func (v EngineVerdict) String() string {
	switch v {
	case EngineTooOld:
		return "too_old"
	case EngineUnknown:
		return "unknown"
	default:
		return "ok"
	}
}

// CheckEngine holds a build against the declared floor and returns the
// verdict plus the sentence an operator needs: what was required, what is
// running, and what to do about it. The reason is empty only for EngineOK.
func (r *Requires) CheckEngine(build string) (EngineVerdict, string) {
	if r == nil || strings.TrimSpace(r.Iterion) == "" {
		return EngineOK, ""
	}
	c, err := ParseEngineConstraint(r.Iterion)
	if err != nil {
		// Unreachable through decodeManifest, which validates. Reachable for
		// a hand-built Manifest value, and silence there would be the very
		// hole this type exists to close.
		return EngineTooOld, fmt.Sprintf("bot requires iterion %q, which this build cannot read: %v", r.Iterion, err)
	}
	got, ok := numericVersionParts(build)
	if !ok {
		return EngineUnknown, fmt.Sprintf(
			"bot requires iterion %s but the running build %q carries no orderable version — the requirement could not be checked",
			c.Raw, build)
	}
	if compareVersionParts(got, c.Min) < 0 {
		return EngineTooOld, fmt.Sprintf(
			"bot requires iterion %s but this build is %s — upgrade the engine (or the image this bot runs on), or relax the manifest's requires.iterion",
			c.Raw, build)
	}
	return EngineOK, ""
}

// CheckManifestEngine is the nil-safe form every admission site calls: a
// bundle with no manifest, or no `requires:`, passes.
func CheckManifestEngine(m *Manifest, build string) (EngineVerdict, string) {
	if m == nil {
		return EngineOK, ""
	}
	return m.Requires.CheckEngine(build)
}

// CompareVersions orders two free-form version strings by their dotted
// numeric components: -1, 0 or +1, with ok=false when either side cannot be
// ordered. A leading "v" is tolerated, a shorter version is zero-padded
// (1.2 == 1.2.0), and components compare numerically so 0.10.0 > 0.9.0
// (which a string compare gets backwards).
//
// KNOWN BLIND SPOT, shared by every caller: a suffixed version (0.8.0-rc1,
// a build metadata tail) is unorderable. Widening this to full semver
// precedence would mean deciding that -rc1 sorts BEFORE 0.8.0 for every
// operator, which neither the free-form Manifest.Version contract nor the
// engine's own build strings license.
func CompareVersions(a, b string) (int, bool) {
	pa, okA := numericVersionParts(a)
	pb, okB := numericVersionParts(b)
	if !okA || !okB {
		return 0, false
	}
	return compareVersionParts(pa, pb), true
}

// compareVersionParts orders two already-parsed component lists.
func compareVersionParts(pa, pb []int) int {
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var ca, cb int
		if i < len(pa) {
			ca = pa[i]
		}
		if i < len(pb) {
			cb = pb[i]
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// numericVersionParts splits a version into its dotted numeric components.
// A leading "v" is tolerated, and a `+commit` build tail is dropped (the
// engine's own FullVersion carries one); anything else non-numeric makes the
// whole version unorderable.
func numericVersionParts(v string) ([]int, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	fields := strings.Split(s, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
