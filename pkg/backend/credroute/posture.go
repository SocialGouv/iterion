package credroute

import (
	"context"
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

// Capture returns the posture ctx carries — pinned for the attempt by
// WithPosture, or the environment a caller carries (WithEnv) — else a
// snapshot of the process environment, each name's provenance read from
// envtrust at the capture.
func Capture(ctx context.Context) Posture {
	if p, ok := carried(ctx); ok {
		return p
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
func Getenv(ctx context.Context, name string) string {
	if p, ok := carried(ctx); ok {
		return p.env[name]
	}
	return os.Getenv(name)
}

// LookupEnv is Getenv with the set-vs-unset distinction.
func LookupEnv(ctx context.Context, name string) (string, bool) {
	if p, ok := carried(ctx); ok {
		v, set := p.env[name]
		return v, set
	}
	return os.LookupEnv(name)
}

// Environ enumerates the environment ctx carries in os.Environ's shape —
// sorted, so a consumer's output is deterministic — else the process's.
func Environ(ctx context.Context) []string {
	if p, ok := carried(ctx); ok {
		return p.Environ()
	}
	return os.Environ()
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
