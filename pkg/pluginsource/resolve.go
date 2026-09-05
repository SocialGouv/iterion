package pluginsource

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// File is one markdown contribution read out of a git-hosted plugin, in the
// shape the run-contribution payload expects.
type File struct {
	// Kind is the .claude/ leaf dir: "skills" | "commands" | "agents".
	Kind    string
	Name    string
	Content []byte
}

// Skipped names a source a launch proceeded WITHOUT, and why. The resolver
// returns one per source it could not materialise, so the caller can say so
// against the run it is launching; the durable readout is the source's own
// degraded flag.
type Skipped struct {
	Source PluginSource
	Err    error
}

// Materialize fetches a source and reads every contribution file out of it —
// the whole of what a launch needs from the source, nothing less. It is the
// ONE check both the launch-time resolver and the registration endpoint run,
// so a source that passes registration is a source a launch can use, and a
// source a launch skips is one registration would have refused.
func Materialize(ctx context.Context, f *Fetcher, s PluginSource) ([]File, error) {
	if f == nil {
		return nil, fmt.Errorf("pluginsource: no fetcher to materialise %q with", s.Name)
	}
	dir, err := f.Fetch(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("pluginsource: fetch %q: %w", s.Name, err)
	}
	p, err := plugin.LoadDir(s.Name, dir)
	if err != nil {
		return nil, fmt.Errorf("pluginsource: load %q: %w", s.Name, err)
	}
	var out []File
	for _, kind := range plugin.MirrorKinds {
		files, ferr := p.MirrorFiles(kind)
		if ferr != nil {
			return nil, fmt.Errorf("pluginsource: read %s from %q: %w", kind.Name, s.Name, ferr)
		}
		for _, f := range files {
			out = append(out, File{Kind: kind.Dir, Name: f.Name, Content: f.Content})
		}
	}
	return out, nil
}

// Resolver turns a team's enabled PluginSources into contribution files.
//
// This is what makes an org-private plugin DURABLE and TEAM-SCOPED: the
// authority is the store record (Mongo), not a pod's filesystem, so a restart
// re-derives instead of silently losing the plugin — and each team resolves
// only its own sources.
type Resolver struct {
	Store   Store
	Fetcher *Fetcher
	// Warnf reports a source that could not be resolved. Optional.
	Warnf func(format string, args ...any)
}

// Resolve returns the contribution files for every enabled source of a tenant,
// plus the sources it skipped.
//
// Failure policy. A source that cannot be materialised — a fetch the remote
// refuses, a plugin.yaml that does not parse, a contribution file that cannot
// be read — is SKIPPED for this launch and flagged degraded on its record; the
// launch proceeds with the sources that work. One team's broken manifest used
// to fail every launch of the team (2h22 of webhook launches, 2026-08-26),
// which is a worse outcome than one run missing one skill: the skip is not
// quiet (the caller logs it against the run, the record shows it to the
// studio and the API, and registration refuses the same failure up front),
// while a launch path down for everyone is silent about everything else.
//
// The only fatal error left is the LIST failing: then nothing can tell a
// healthy team from one whose sources are all lost.
func (r *Resolver) Resolve(ctx context.Context, tenantID string) ([]File, []Skipped, error) {
	if r.Store == nil || r.Fetcher == nil || tenantID == "" {
		return nil, nil, nil
	}
	sources, err := r.Store.ListEnabledByTenant(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginsource: list sources for team %s: %w", tenantID, err)
	}
	if len(sources) == 0 {
		return nil, nil, nil
	}
	var out []File
	var skipped []Skipped
	for _, s := range sources {
		if r.Warnf != nil && !s.PinnedRef() {
			r.Warnf("plugin source %q tracks the moving ref %q — pin a tag or sha so the skill cannot change under a run", s.Name, s.Ref)
		}
		files, err := Materialize(ctx, r.Fetcher, s)
		if err != nil {
			skipped = append(skipped, Skipped{Source: s, Err: err})
			// The readout is best-effort next to the skip itself: a store
			// that cannot take the stamp still gets the launch, and the
			// caller's log line names the source either way.
			if merr := r.Store.MarkDegraded(ctx, s.TenantID, s.ID, err.Error()); merr != nil && r.Warnf != nil {
				r.Warnf("plugin source %q could not be flagged degraded after a failed resolution: %v", s.Name, merr)
			}
			continue
		}
		if s.Degraded() {
			if cerr := r.Store.ClearDegraded(ctx, s.TenantID, s.ID); cerr != nil && r.Warnf != nil {
				r.Warnf("plugin source %q resolved again but its degraded flag could not be cleared: %v", s.Name, cerr)
			}
		}
		out = append(out, files...)
	}
	return out, skipped, nil
}
