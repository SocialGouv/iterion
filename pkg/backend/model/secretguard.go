package model

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// BuildSecretGuard assembles the per-run secret guard (Layer 0/1/2)
// from every value iterion can already see as sensitive:
//
//   - the resolved per-run credentials carried in ctx (cloud BYOK keys),
//   - host env vars whose name looks secret (the local-run path — same
//     definition store.CaptureLaunchEnv uses), and
//   - the workflow's declared `secrets:` values (Layer 1; the agent only
//     ever sees their placeholder).
//
// Returns nil when redaction is disabled via ITERION_SECRETS_REDACT=off,
// in which case every guard method is a no-op. Otherwise returns a
// non-nil guard even with zero known values, so the heuristic detector
// pass still scrubs unknown token shapes from the run's sinks.
func BuildSecretGuard(ctx context.Context, wf *ir.Workflow, vars map[string]string) *secretguard.Guard {
	cfg := secretGuardConfigFromEnv()

	var known []secretguard.Secret

	// 1. Resolved credentials (cloud / runner-injected).
	var genericSecrets map[string]string
	var genericHosts map[string][]string
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		for prov, val := range creds.APIKeys {
			if val == "" {
				continue
			}
			known = append(known, secretguard.Secret{
				Name:  "provider_" + string(prov),
				Value: val,
			})
		}
		genericSecrets = creds.Generic
		genericHosts = creds.GenericHosts
	}

	// 2. Sensitive host env vars (local runs, where keys come from the
	//    environment rather than an injected bundle).
	for _, raw := range os.Environ() {
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			continue
		}
		name, val := raw[:eq], raw[eq+1:]
		if val == "" || !store.IsSecretEnvName(name) {
			continue
		}
		// Ambient variables are not necessarily credentials merely because
		// their NAME contains PASSWORD/KEY/AUTH. Registering a short,
		// dictionary-like value as a global substring taint can erase an
		// unrelated project vocabulary everywhere (for example a local DB
		// password named after the project). Such values remain protected at
		// source by the credential-file and permission boundaries; only values
		// distinctive enough for sink-wide literal matching enter this matcher.
		if !ambientSecretGloballySafe(val) {
			continue
		}
		known = append(known, secretguard.Secret{
			Name:  "env_" + name,
			Value: val,
		})
	}

	// 3. Declared workflow secrets (Layer 1). These carry an explicit
	//    placeholder the agent sees in place of the value, plus optional
	//    egress host scoping consumed by Layer 2.
	known = append(known, declaredWorkflowSecrets(wf, vars, genericSecrets, genericHosts)...)

	g := secretguard.New(known, cfg)
	// Return nil only when the guard would do nothing at all: no known
	// values to redact/materialise and the heuristic pass is disabled.
	// This keeps the common no-secrets path allocation-free downstream.
	if !g.HasKnownSecrets() && len(g.SecretFileHints()) == 0 && !cfg.Heuristic {
		return nil
	}
	return g
}

// ambientSecretGloballySafe reports whether an ambient value is distinctive
// enough to replace as a substring in arbitrary source, logs and artifacts.
// Twelve mixed characters covers ordinary generated passwords; long
// passphrases are accepted with two character classes. Short/simple values
// are intentionally left to source controls instead of becoming toxic global
// redaction patterns.
func ambientSecretGloballySafe(value string) bool {
	runes := []rune(value)
	if len(runes) < 12 {
		return false
	}
	classes := 0
	var lower, upper, digit, other bool
	for _, r := range runes {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			other = true
		}
	}
	for _, present := range []bool{lower, upper, digit, other} {
		if present {
			classes++
		}
	}
	return classes >= 3 || (len(runes) >= 20 && classes >= 2)
}

// declaredWorkflowSecrets resolves the workflow's `secrets:` block into
// guard entries. Each declared value (typically "${ENV}" or a
// "{{vars.X}}" reference) is resolved to its real plaintext here; the
// agent only ever sees the placeholder (PlaceholderForName), which the
// guard materialises at tool/shell exec.
func declaredWorkflowSecrets(wf *ir.Workflow, vars map[string]string, generic map[string]string, genericHosts map[string][]string) []secretguard.Secret {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	out := make([]secretguard.Secret, 0, len(wf.Secrets))
	names := make([]string, 0, len(wf.Secrets))
	for name := range wf.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := wf.Secrets[name]
		val := ""
		if strings.TrimSpace(s.Value) != "" {
			val = resolveSecretValue(s.Value, vars)
		} else if generic != nil {
			val = generic[name]
		}
		filePath := ""
		if s.IsFile() {
			filePath = secrets.ResolveFileMountPath(name, s.MountPath)
		}
		if val == "" && filePath == "" {
			continue // unset/unresolved — nothing to taint or materialise
		}
		out = append(out, secretguard.Secret{
			Name:        name,
			Value:       val,
			Placeholder: secretguard.PlaceholderForName(name),
			FilePath:    filePath,
			Env:         s.Env,
			// A bot-secret binding may NARROW egress for this secret.
			// effectiveSecretHosts intersects the workflow's declared hosts
			// with the binding's allowed hosts; a binding can only restrict,
			// never broaden.
			Hosts: effectiveSecretHosts(s.Hosts, genericHosts[name]),
		})
	}
	return out
}

// effectiveSecretHosts computes the egress host allowlist for a secret
// from the workflow's declared hosts and a bot-secret binding's allowed
// hosts. Empty on either side means "that side imposes no restriction".
// A binding can only NARROW, never broaden:
//   - binding empty            -> the workflow's hosts (unchanged)
//   - workflow empty, binding set -> the binding's hosts (narrows from any)
//   - both set, overlapping     -> their intersection (narrowest)
//   - both set, disjoint        -> deny-all ([""], an unmatchable host —
//     secretguard.hostMatch returns false for the empty pattern, so the
//     secret is never materialised toward any host; this is distinct from
//     a nil list, which means allow-any).
func effectiveSecretHosts(workflow, binding []string) []string {
	if len(binding) == 0 {
		return workflow
	}
	if len(workflow) == 0 {
		return binding
	}
	isect := secrets.IntersectHosts(workflow, binding)
	if len(isect) == 0 {
		return []string{""}
	}
	return isect
}

// resolveSecretValue resolves a declared secret's value expression:
// ${ENV} / ${ENV:-default} via the shared IR expander, or a single
// {{vars.NAME}} reference against the run's vars. A plain literal is
// returned unchanged.
func resolveSecretValue(expr string, vars map[string]string) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "{{") && strings.HasSuffix(expr, "}}") {
		inner := strings.TrimSpace(expr[2 : len(expr)-2])
		if rest, ok := strings.CutPrefix(inner, "vars."); ok {
			return vars[strings.TrimSpace(rest)]
		}
	}
	return ir.ExpandEnvWithDefault(expr)
}

// secretGuardConfigFromEnv resolves the guard's tunables from the
// environment, falling back to the production defaults.
func secretGuardConfigFromEnv() secretguard.Config {
	cfg := secretguard.DefaultConfig()
	// Master kill-switch: disable all sink redaction (known + heuristic)
	// while leaving placeholder materialisation intact.
	if !envFlagEnabled("ITERION_SECRETS_REDACT", true) {
		cfg.RedactKnown = false
		cfg.Heuristic = false
	}
	if !envFlagEnabled("ITERION_SECRETS_REDACT_HEURISTIC", true) {
		cfg.Heuristic = false
	}
	if !envFlagEnabled("ITERION_SECRETS_REDACT_DECODE", true) {
		cfg.RecurseDecode = false
	}
	if v := strings.TrimSpace(os.Getenv("ITERION_SECRETS_REDACT_MIN_SCORE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.MinScore = f
		}
	}
	if !envFlagEnabled("ITERION_SECRETS_PLACEHOLDERS", true) {
		cfg.Placeholders = false
	}
	return cfg
}

// envFlagEnabled reads a boolean-ish env var. Recognised falsey values
// are "0", "false", "off", "no" (case-insensitive); anything else
// (including unset) returns def.
func envFlagEnabled(name string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return def
	}
	switch v {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
	default:
		return def
	}
}
