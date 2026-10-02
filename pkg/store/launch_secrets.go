package store

import "strings"

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
