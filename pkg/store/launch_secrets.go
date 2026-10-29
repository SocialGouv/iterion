package store

import (
	"os"
	"strings"
)

// The launch vars a server mints for one run's forge publish grant: its bearer
// and the three endpoints the bearer is presented to.
const (
	ForgePublishURLVar           = "forge_publish_url"
	ForgePublishTokenVar         = "forge_publish_token"
	ForgePRStateURLVar           = "forge_pr_state_url"
	ForgeDeliveryPreflightURLVar = "forge_delivery_preflight_url"
)

// ServerMintedLaunchVars are the launch vars a server mints for one run: the
// forge publish grant's bearer and the endpoints it is presented to. The
// runner, a resume, a fork and the grant's own lifecycle read them from the run
// record. Every write path a client drives drops all of them
// (DropServerMintedVars): the server binds the bearer to its endpoints, and a
// client able to set an endpoint while the run keeps its bearer would point a
// live grant at a host of its choosing.
var ServerMintedLaunchVars = [...]string{ForgePublishURLVar, ForgePublishTokenVar, ForgePRStateURLVar, ForgeDeliveryPreflightURLVar}

// ServerMintedSecretVars are the ServerMintedLaunchVars that are credentials.
// Every read surface that hands a run's inputs out masks them
// (RedactLaunchVars), and the run's secret guard redacts their values from its
// sinks.
var ServerMintedSecretVars = []string{ForgePublishTokenVar}

// RedactedLaunchVar is what a read surface shows in place of a server-minted
// secret var: it says a grant was minted without handing it out.
const RedactedLaunchVar = "[redacted]"

// mintedSecretEnvPrefix prefixes the environment name a server-minted secret
// var travels under to a child process. It reads as a credential
// (IsSecretEnvName: SECRET anywhere), so the engine redacts the name's value
// from its sinks and the platform refuses it as a settable bot var.
const mintedSecretEnvPrefix = "ITERION_LAUNCH_SECRET_"

// ServerMintedSecretEnvName is the environment name under which a
// server-minted secret var reaches a child process iterion spawns, instead of
// its argv: a command line is readable by any co-resident process through
// /proc/<pid>/cmdline for the whole life of the process, while an environment
// is readable by the owning user and root only.
func ServerMintedSecretEnvName(name string) string {
	return mintedSecretEnvPrefix + strings.ToUpper(name)
}

// ServerMintedSecretEnv splits vars for a child process iterion spawns: the
// vars that ride its command line, and the "NAME=value" environment entries
// carrying the server-minted secrets that must not. ONE decision answers both
// halves, so no var can end up on neither channel or on both.
//
// A var with no value is not a credential — it is a caller withdrawing one, as
// DropServerMintedVars reads it — so it stays on the argv the child reads its
// var set from, where it says exactly that.
func ServerMintedSecretEnv(vars map[string]string) (argv map[string]string, env []string) {
	for name, v := range vars {
		if IsServerMintedSecretVar(name) && mintedSecretValue(v) {
			env = append(env, ServerMintedSecretEnvName(name)+"="+v)
			continue
		}
		if argv == nil {
			argv = make(map[string]string, len(vars))
		}
		argv[name] = v
	}
	return argv, env
}

// TakeServerMintedSecretVarsFromEnv returns the server-minted secret vars the
// spawning process passed through the environment and REMOVES them from it:
// the launch is their only reader, and a value left in the environment is
// inherited by every tool process the run goes on to spawn. Call it once, at
// the launch boundary, before the first node runs.
//
// The unset is about the CHILDREN: this process's own /proc/<pid>/environ
// keeps the initial environment for its whole life, readable by the owning
// user and by root. That residue is the exposure this carriage accepts in
// exchange for keeping the grant out of a command line, which any co-resident
// process reads.
func TakeServerMintedSecretVarsFromEnv() map[string]string {
	var out map[string]string
	for _, name := range ServerMintedSecretVars {
		envName := ServerMintedSecretEnvName(name)
		v, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}
		// Unset whatever was passed, value or not: a name the parent set is
		// a name no tool process may inherit.
		_ = os.Unsetenv(envName)
		if !mintedSecretValue(v) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(ServerMintedSecretVars))
		}
		out[name] = v
	}
	return out
}

// IsServerMintedSecretVar reports whether name is one of ServerMintedSecretVars.
func IsServerMintedSecretVar(name string) bool {
	for _, n := range ServerMintedSecretVars {
		if n == name {
			return true
		}
	}
	return false
}

func isServerMintedLaunchVar(name string) bool {
	for _, n := range ServerMintedLaunchVars {
		if n == name {
			return true
		}
	}
	return false
}

// mintedSecretValue reports whether v is a value a mask stands for: a var
// declared with an empty default carries no grant, and masking it would say
// one was minted.
func mintedSecretValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	default:
		return true
	}
}

// HasServerMintedSecret reports whether vars holds a server-minted secret var
// with a value.
func HasServerMintedSecret[V any](vars map[string]V) bool {
	for k, v := range vars {
		if IsServerMintedSecretVar(k) && mintedSecretValue(v) {
			return true
		}
	}
	return false
}

// RedactLaunchVars returns vars with every server-minted secret var that holds
// a value masked: a copy when one is present, vars itself otherwise. vars is
// never mutated — the caller's map may be the run record's own.
func RedactLaunchVars(vars map[string]any) map[string]any {
	if !HasServerMintedSecret(vars) {
		return vars
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if IsServerMintedSecretVar(k) && mintedSecretValue(v) {
			v = RedactedLaunchVar
		}
		out[k] = v
	}
	return out
}

// RedactLaunchStringVars is RedactLaunchVars for a STRING-valued var map — a
// board card's bot args, a tracker's vars: a copy with every server-minted
// secret var that holds a value masked, or vars itself when it holds none.
// vars is never mutated — the caller's map may be the stored record's own.
//
// Separate from RedactLaunchVars rather than generic over the value type: the
// mask IS a string, so a map[string]V could not receive it.
func RedactLaunchStringVars(vars map[string]string) map[string]string {
	if !HasServerMintedSecret(vars) {
		return vars
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		if IsServerMintedSecretVar(k) && mintedSecretValue(v) {
			v = RedactedLaunchVar
		}
		out[k] = v
	}
	return out
}

// DropServerMintedVars returns vars without any VALUE sent under one of
// ServerMintedLaunchVars — a copy when one was present, vars itself
// otherwise. For the write paths a client drives (fork inputs, board-card bot
// args): the server mints these, and whatever a client sends under their
// names — the mask it was shown, a stale token, an endpoint of its own — is
// not them.
//
// An EMPTY value is kept: it grants nothing and names nowhere, so the only
// thing it can do is withdraw. A fork launched with forge_publish_token: ""
// is the operator asking for a child that does not publish, and dropping it
// would hand that child its parent's live grant instead — overriding an
// explicit choice with the opposite one.
func DropServerMintedVars[V any](vars map[string]V) map[string]V {
	drop := func(k string, v V) bool { return isServerMintedLaunchVar(k) && mintedSecretValue(any(v)) }
	present := false
	for k, v := range vars {
		if drop(k, v) {
			present = true
			break
		}
	}
	if !present {
		return vars
	}
	out := make(map[string]V, len(vars))
	for k, v := range vars {
		if !drop(k, v) {
			out[k] = v
		}
	}
	return out
}

// RedactCheckpoint is cp for a read surface: a shallow copy whose vars mask
// the server-minted secret vars, or cp itself when it holds none. cp is never
// mutated — the engine resumes from it.
func RedactCheckpoint(cp *Checkpoint) *Checkpoint {
	if cp == nil || !HasServerMintedSecret(cp.Vars) {
		return cp
	}
	out := *cp
	out.Vars = RedactLaunchVars(cp.Vars)
	return &out
}

// RedactRunForOutput is r for a surface that prints the whole record (a CLI
// dump): a shallow copy with its inputs and checkpoint vars masked, or r
// itself when neither holds a server-minted secret var. r is never mutated.
func RedactRunForOutput(r *Run) *Run {
	if r == nil || !HasServerMintedSecret(r.Inputs) && (r.Checkpoint == nil || !HasServerMintedSecret(r.Checkpoint.Vars)) {
		return r
	}
	out := *r
	out.Inputs = RedactLaunchVars(r.Inputs)
	out.Checkpoint = RedactCheckpoint(r.Checkpoint)
	return &out
}
