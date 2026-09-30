package plugin

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// EffectiveConfig returns the named plugin's config as it is actually used:
// the manifest field defaults overlaid with the operator's stored values. This
// is the map fed to {{config.<key>}} expansion (it includes secret values, so
// the MCP/rewriter subprocess gets the real credential).
func (r *Registry) EffectiveConfig(name string) map[string]string {
	out := map[string]string{}
	defaults := map[string]string{}
	if p, ok := r.Get(name); ok {
		for _, f := range p.Manifest.Config {
			defaults[f.Key] = f.Default
		}
	}
	// Precedence, lowest to highest: the manifest's default, the operator's
	// stored value, the env override ITERION_PLUGIN_<NAME>_<KEY> — the
	// cloud/headless path, where the operator (or the Helm chart) sets plugin
	// config via immutable env instead of the per-pod-ephemeral plugins.yaml.
	for _, key := range r.configKeys(name) {
		if def := defaults[key]; def != "" {
			out[key] = def
		}
		if stored, ok := r.config[name][key]; ok {
			out[key] = stored
		}
		if v, ok := pluginConfigEnv(name, key); ok {
			out[key] = v
		}
	}
	return out
}

// configKeys is every key this plugin's configuration can carry: the ones its
// manifest declares, plus the ones the operator's plugins.yaml stores. The
// union, not the manifest alone, because a stored key the CURRENT manifest no
// longer declares is still read — a builtin's config schema changing between
// versions leaves exactly that.
//
// One owner for the key set, because two readers walking it differently is
// how the trust check came to ask about a smaller set than the reader used:
// an undeclared stored key's env override reached an operator-TRUSTED
// server's environment without the check ever looking at it.
func (r *Registry) configKeys(name string) []string {
	seen := map[string]bool{}
	if p, ok := r.Get(name); ok {
		for _, f := range p.Manifest.Config {
			seen[f.Key] = true
		}
	}
	for k := range r.config[name] {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pluginConfigEnv reads the env override for one plugin config key:
// ITERION_PLUGIN_<NAME>_<KEY>, upper-cased with '-' → '_'. Returns the raw
// value and whether the env var is set (empty-but-set is honoured, e.g. to
// blank a defaulted URL). E.g. plugin "firecrawl" key "api_url" →
// ITERION_PLUGIN_FIRECRAWL_API_URL.
func pluginConfigEnv(name, key string) (string, bool) {
	return os.LookupEnv(pluginConfigEnvName(name, key))
}

// pluginConfigEnvName is the per-key override variable. One place, because
// the provenance check has to ask about exactly the names the reader reads.
func pluginConfigEnvName(name, key string) string {
	return "ITERION_PLUGIN_" + envToken(name) + "_" + envToken(key)
}

// envToken upper-cases and replaces '-' with '_' so kebab plugin/config names
// map to a legal env-var segment.
func envToken(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
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
// invoke env/argv and run env — rewriters run per shell command and carry
// resolved env, unlike the mcp/lifecycle surfaces which expand via
// ExpandContext at run time.
func (r *Registry) EnabledRewriterSpecs() []RewriterSpec {
	contribs := r.EnabledRewriters()
	out := make([]RewriterSpec, 0, len(contribs))
	for _, c := range contribs {
		out = append(out, expandSpecConfig(c.Spec, r.EffectiveConfig(c.Plugin)))
	}
	return out
}

// expandSpecConfig returns a copy of spec with {{config.<key>}} substituted in
// its invoke argv + env and its run env. The registry's manifest is never
// mutated.
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
	if len(spec.RunEnv) > 0 {
		env := make(map[string]string, len(spec.RunEnv))
		for k, v := range spec.RunEnv {
			env[k] = rep.Replace(v)
		}
		out.RunEnv = env
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

// configIsOperators reports whether every configuration value this plugin
// will actually run with came from a source the operator controls.
//
// Two sources can carry a repository's answer: <home>/plugins.yaml, when a
// project `.env` selected that home, and the per-key override
// ITERION_PLUGIN_<NAME>_<KEY>, when a project `.env` planted it. Manifest
// defaults are the plugin's own and always count as the operator's.
//
// A plugin with no configuration at all is unaffected — there is nothing for
// a repository to have said.
func (r *Registry) configIsOperators(name string) bool {
	if len(r.config[name]) > 0 && !r.homeOperatorChosen {
		return false
	}
	for _, key := range r.configKeys(name) {
		if envtrust.Planted(pluginConfigEnvName(name, key)) {
			return false
		}
	}
	return true
}
