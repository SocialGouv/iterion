package plugin

import "fmt"

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
