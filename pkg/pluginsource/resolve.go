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

// Resolve returns the contribution files for every enabled source of a tenant.
//
// Failure policy is deliberate and differs from the local plugin registry's
// "best-effort, skip quietly": a source the operator explicitly bound and
// enabled, which then fails to fetch, is reported as an ERROR. Silently
// contributing nothing is precisely the failure mode that lets a deploy run
// without its platform playbook and still report success.
func (r *Resolver) Resolve(ctx context.Context, tenantID string) ([]File, error) {
	if r.Store == nil || r.Fetcher == nil || tenantID == "" {
		return nil, nil
	}
	sources, err := r.Store.ListEnabledByTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("pluginsource: list sources for team %s: %w", tenantID, err)
	}
	if len(sources) == 0 {
		return nil, nil
	}
	var out []File
	for _, s := range sources {
		if r.Warnf != nil && !s.PinnedRef() {
			r.Warnf("plugin source %q tracks the moving ref %q — pin a tag or sha so the skill cannot change under a run", s.Name, s.Ref)
		}
		dir, err := r.Fetcher.Fetch(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("pluginsource: fetch %q: %w", s.Name, err)
		}
		p, err := plugin.LoadDir(s.Name, dir)
		if err != nil {
			return nil, fmt.Errorf("pluginsource: load %q: %w", s.Name, err)
		}
		for _, kind := range plugin.MirrorKinds {
			files, ferr := p.MirrorFiles(kind)
			if ferr != nil {
				return nil, fmt.Errorf("pluginsource: read %s from %q: %w", kind.Name, s.Name, ferr)
			}
			for _, f := range files {
				out = append(out, File{Kind: kind.Dir, Name: f.Name, Content: f.Content})
			}
		}
	}
	return out, nil
}
