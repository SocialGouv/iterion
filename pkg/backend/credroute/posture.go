package credroute

import (
	"context"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Posture is the pod environment a call is judged against: the whole
// environment, no name list to drift, each name with its envtrust
// provenance. A planted value — one a project `.env` filled in — funds in
// process (the local lane's `.env`), never across the sandbox (D20,
// ADR-132): Planted and Inherited tell the arms which.
//
// A posture is hermetic: a name it does not hold reads as unset, never as
// the process value.
type Posture struct {
	env     map[string]string
	planted map[string]bool
}

// postureKey keys the posture a context carries; unexported, so the only
// ways in are WithPosture and WithEnv.
type postureKey struct{}

// envOverlayKey keys a context's env overlay: names a caller pinned on
// the context WITHOUT replacing the environment beneath them — the
// gateway price feed's channel (ADR-163). A posture or carried env
// still answers every other name; an overlay name answers FIRST, for
// the process fallback included, so a reader holding the context and a
// reader falling back to the process see one table.
type envOverlayKey struct{}

// WithEnvOverlay pins one name's value on ctx as an overlay: Getenv and
// LookupEnv answer it before any carried posture and before the process
// environment, while every other name reads exactly as before. Unlike
// WithEnv it never blanks what it does not name — the overlay ADDS a
// host-written word to the environment the run already carries. An
// empty value unpins the name (nothing writes an empty feed table; an
// explicit empty would read as "the host says no table", which is a
// different fact from "this run has none" only the host can state).
//
// The overlay is not marked planted: it is host-written, the same
// standing the sandbox crossing's explicitly forwarded names hold. A
// project `.env` planting the SAME name is still refused downstream by
// the reader that cares (the gateway catalog's planted filter reads the
// process-level mark, which the overlay does not touch).
func WithEnvOverlay(ctx context.Context, name, value string) context.Context {
	if name == "" || value == "" {
		return ctx
	}
	prev, _ := ctx.Value(envOverlayKey{}).(map[string]string)
	next := make(map[string]string, len(prev)+1)
	for k, v := range prev {
		next[k] = v
	}
	next[name] = value
	return context.WithValue(ctx, envOverlayKey{}, next)
}

func overlay(ctx context.Context, name string) (string, bool) {
	if ctx == nil {
		return "", false
	}
	m, ok := ctx.Value(envOverlayKey{}).(map[string]string)
	if !ok {
		return "", false
	}
	v, ok := m[name]
	return v, ok
}

// Capture returns the posture ctx carries — pinned for the attempt by
// WithPosture, or the environment a caller carries (WithEnv) — else a
// snapshot of the process environment, each name's provenance read from
// envtrust at the capture. Overlay names (WithEnvOverlay) ride INTO the
// captured posture: they answer first on reads, so a posture that
// dropped them would hand a whole-env reader a different environment
// than the named readers answer from.
func Capture(ctx context.Context) Posture {
	if p, ok := carried(ctx); ok {
		return withOverlay(ctx, p)
	}
	environ := os.Environ()
	p := Posture{env: make(map[string]string, len(environ)), planted: map[string]bool{}}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		if name == "" {
			// Windows' per-drive "=C:=C:\…" entries name no variable.
			continue
		}
		p.env[name] = value
		if envtrust.Planted(name) {
			p.planted[name] = true
		}
	}
	return withOverlay(ctx, p)
}

// withOverlay folds ctx's overlay names into a posture — the overlay
// wins, matching Getenv's precedence, and no overlay name is planted
// (it is host-written; the planted mark stays whatever the process said
// for names the overlay does not carry).
func withOverlay(ctx context.Context, p Posture) Posture {
	m, ok := ctx.Value(envOverlayKey{}).(map[string]string)
	if !ok || len(m) == 0 {
		return p
	}
	env := make(map[string]string, len(p.env)+len(m))
	for k, v := range p.env {
		env[k] = v
	}
	// The planted map is cloned before the deletes: a PINNED posture is
	// shared by every reader of the ctx that pinned it, and mutating its
	// map here races (round 2 caught it under -race) besides flipping
	// verdicts other readers still consult.
	planted := maps.Clone(p.planted)
	for k, v := range m {
		env[k] = v
		delete(planted, k)
	}
	p.env = env
	p.planted = planted
	return p
}

// WithPosture pins p on ctx: every read of the environment through ctx
// (Getenv, LookupEnv, Environ, Capture) answers from it.
func WithPosture(ctx context.Context, p Posture) context.Context {
	if p.env == nil {
		p.env = map[string]string{}
	}
	return context.WithValue(ctx, postureKey{}, p)
}

// WithEnv carries env as ctx's environment — the #2808 seam: a run's
// envelope, a test's isolation — each name's provenance read from envtrust.
// A nil env is a no-op (the process stays the source); an empty non-nil map
// is a deliberately empty environment. Treat the map as read-only once
// carried.
func WithEnv(ctx context.Context, env map[string]string) context.Context {
	if env == nil {
		return ctx
	}
	p := Posture{env: env, planted: map[string]bool{}}
	for name := range env {
		if envtrust.Planted(name) {
			p.planted[name] = true
		}
	}
	return context.WithValue(ctx, postureKey{}, p)
}

func carried(ctx context.Context) (Posture, bool) {
	if ctx == nil {
		return Posture{}, false
	}
	p, ok := ctx.Value(postureKey{}).(Posture)
	return p, ok
}

// Carries reports whether ctx carries an environment of its own (a pinned
// posture or a carried env) rather than reading the process.
func Carries(ctx context.Context) bool {
	_, ok := carried(ctx)
	return ok
}

// Getenv reads name from the environment ctx carries, else the process.
// An overlay name (WithEnvOverlay) answers before both.
func Getenv(ctx context.Context, name string) string {
	if v, ok := overlay(ctx, name); ok {
		return v
	}
	if p, ok := carried(ctx); ok {
		return p.env[name]
	}
	return os.Getenv(name)
}

// LookupEnv is Getenv with the set-vs-unset distinction.
func LookupEnv(ctx context.Context, name string) (string, bool) {
	if v, ok := overlay(ctx, name); ok {
		return v, true
	}
	if p, ok := carried(ctx); ok {
		v, set := p.env[name]
		return v, set
	}
	return os.LookupEnv(name)
}

// Inherited is name's value as the process inherited it, read through
// ctx: an overlay answers as itself (a host-written value IS inherited
// by the run it was pinned on); a carried posture answers its Inherited
// form ("" where a project `.env` planted the name); nothing carried,
// the process's own envtrust verdict. The ctx-aware twin of
// envtrust.Inherited, so a reader with a context never falls back to a
// process-scoped answer the dispatch did not use.
func Inherited(ctx context.Context, name string) string {
	if v, ok := overlay(ctx, name); ok {
		return v
	}
	if p, ok := carried(ctx); ok {
		return p.Inherited(name)
	}
	return envtrust.Inherited(name)
}

// Environ enumerates the environment ctx carries in os.Environ's shape —
// sorted, so a consumer's output is deterministic — else the process's.
func Environ(ctx context.Context) []string {
	// Through Capture: the overlay rides INTO the enumeration (a
	// whole-env reader building a child's environment must not silently
	// drop the host-written names the named readers answer from), and
	// the carried-posture branch of Capture folds the same way.
	return Capture(ctx).Environ()
}

// Getenv reads name from the posture.
func (p Posture) Getenv(name string) string { return p.env[name] }

// LookupEnv reads name with the set-vs-unset distinction.
func (p Posture) LookupEnv(name string) (string, bool) {
	v, ok := p.env[name]
	return v, ok
}

// Planted reports whether a project `.env` filled name in.
func (p Posture) Planted(name string) bool { return p.planted[name] }

// Inherited is name's value as the process inherited it: "" where a project
// `.env` planted it — the operator said nothing.
func (p Posture) Inherited(name string) string {
	if p.planted[name] {
		return ""
	}
	return p.env[name]
}

// Environ enumerates the posture, sorted.
func (p Posture) Environ() []string {
	out := make([]string, 0, len(p.env))
	for name, value := range p.env {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}
