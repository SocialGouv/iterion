package plugin

import (
	"fmt"
	"strings"
)

// EffectiveConfig returns the named plugin's config as it is actually used:
// the manifest field defaults overlaid with the operator's stored values. This
// is the map fed to {{config.<key>}} expansion (it includes secret values, so
// the MCP/rewriter subprocess gets the real credential).
func (r *Registry) EffectiveConfig(name string) map[string]string {
	out := map[string]string{}
	if p, ok := r.Get(name); ok {
		for _, f := range p.Manifest.Config {
			if f.Default != "" {
				out[f.Key] = f.Default
			}
		}
	}
	for k, v := range r.config[name] {
		out[k] = v
	}
	return out
}

// StoredConfig returns a copy of the operator-set values for a plugin (no
// defaults), for the config handler's merge logic.
func (r *Registry) StoredConfig(name string) map[string]string {
	out := map[string]string{}
	for k, v := range r.config[name] {
		out[k] = v
	}
	return out
}

// ApplyConfig merges submitted values over a plugin's stored config and
// persists. Only declared fields are accepted; a secret submitted blank keeps
// its prior value ("leave blank to keep"). Shared by the HTTP config handler
// and the `iterion plugin config` CLI so both behave identically.
func (r *Registry) ApplyConfig(name string, submitted map[string]string) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	merged := r.StoredConfig(name)
	for _, f := range p.Manifest.Config {
		v, sent := submitted[f.Key]
		if !sent {
			continue
		}
		if f.Type == "secret" && strings.TrimSpace(v) == "" {
			continue
		}
		merged[f.Key] = v
	}
	return r.SetConfig(name, merged)
}

// EnabledRewriterSpecs returns the enabled plugins' rewriter specs with their
// {{config.<key>}} placeholders resolved from each plugin's effective config.
// {{command}} and {{workspace}}/{{plugin.*}} are left untouched for the
// rewrite/sandbox layers. This is how operator config reaches a rewriter's
// invoke env/argv — rewriters run per shell command and carry resolved env,
// unlike the mcp/lifecycle surfaces which expand via ExpandContext at run time.
func (r *Registry) EnabledRewriterSpecs() []RewriterSpec {
	contribs := r.EnabledRewriters()
	out := make([]RewriterSpec, 0, len(contribs))
	for _, c := range contribs {
		out = append(out, expandSpecConfig(c.Spec, r.EffectiveConfig(c.Plugin)))
	}
	return out
}

// expandSpecConfig returns a copy of spec with {{config.<key>}} substituted in
// its invoke argv + env. The registry's manifest is never mutated.
func expandSpecConfig(spec RewriterSpec, cfg map[string]string) RewriterSpec {
	if len(cfg) == 0 {
		return spec
	}
	pairs := make([]string, 0, len(cfg)*2)
	for k, v := range cfg {
		pairs = append(pairs, "{{config."+k+"}}", v)
	}
	rep := strings.NewReplacer(pairs...)
	out := spec
	if len(spec.Invoke.Argv) > 0 {
		argv := make([]string, len(spec.Invoke.Argv))
		for i, a := range spec.Invoke.Argv {
			argv[i] = rep.Replace(a)
		}
		out.Invoke.Argv = argv
	}
	if len(spec.Invoke.Env) > 0 {
		env := make(map[string]string, len(spec.Invoke.Env))
		for k, v := range spec.Invoke.Env {
			env[k] = rep.Replace(v)
		}
		out.Invoke.Env = env
	}
	return out
}

// SetConfig persists the operator config values for a plugin (replacing any
// prior values) and updates the in-memory registry. Empty values are dropped so
// the field falls back to its manifest default.
func (r *Registry) SetConfig(name string, values map[string]string) error {
	if _, ok := r.Get(name); !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	clean := map[string]string{}
	for k, v := range values {
		if v != "" {
			clean[k] = v
		}
	}
	if r.config == nil {
		r.config = map[string]map[string]string{}
	}
	r.config[name] = clean
	return r.saveState()
}

// ViewFor returns the full listing view (incl. config schema + masked values)
// for one plugin. Used by the install/config handlers to echo fresh state.
func (r *Registry) ViewFor(name string) (View, bool) {
	p, ok := r.Get(name)
	if !ok {
		return View{}, false
	}
	return r.fillConfigView(p.View(), p), true
}

// fillConfigView populates a view's config VALUES from registry state. Non-secret
// fields carry their effective value; secret fields never leave the server — we
// only report which ones currently have a value (so the studio can show
// "set — leave blank to keep").
func (r *Registry) fillConfigView(v View, p *Plugin) View {
	if len(p.Manifest.Config) == 0 {
		return v
	}
	eff := r.EffectiveConfig(p.Name())
	values := map[string]string{}
	var secretSet []string
	for _, f := range p.Manifest.Config {
		if f.Type == "secret" {
			if eff[f.Key] != "" {
				secretSet = append(secretSet, f.Key)
			}
			continue
		}
		values[f.Key] = eff[f.Key]
	}
	v.ConfigValues = values
	v.ConfigSecretSet = secretSet
	return v
}
