package native

import "github.com/SocialGouv/iterion/pkg/store"

// ReservedLaunchArgs are the reserved `__iterion_*` bot-arg keys that carry a
// webhook-launched card's launch context: the repo to clone and its ref, the
// forge slug the run is metered and bounded by, the webhook's BYOK key and
// secret overrides, and the id of the integration whose launch-policy layers
// the launch resolves. ensureBoardCard (pkg/server) stamps them from the
// webhook payload — the server's word about what the card's run targets —
// and the cloud dispatcher lifts them into the launch. They are launch
// context, never bot input.
//
// Every client-facing board write strips them (DropReservedLaunchArgs at
// create, KeepStoredLaunchContext across a wholesale patch): a principal
// able to write them would decide what its run clones, which credential it
// resolves, and which repository meters, bounds and debits it — the repo
// quota's subject and a BYOK key's repo audience both read off this class
// (#2371, D33). Symmetric with store.DropServerMintedVars, same doctrine:
// the route filters the class, the store twins stay writer-agnostic (the
// deeper funnel cannot tell a card's existing args from args this write
// supplies, and the server's own stamp — ensureBoardCard — writes through
// the same store.Create).
var ReservedLaunchArgs = []string{
	"__iterion_repo_url",
	"__iterion_repo_ref",
	"__iterion_project_path",
	"__iterion_key_overrides",
	"__iterion_secret_overrides",
	"__iterion_webhook_id",
}

// IsReservedLaunchArg reports whether k is one of the reserved launch-context
// keys.
func IsReservedLaunchArg(k string) bool {
	for _, res := range ReservedLaunchArgs {
		if k == res {
			return true
		}
	}
	return false
}

// DropReservedLaunchArgs is args without the reserved launch-context keys, or
// args itself when it carries none. Applied at the board write surfaces a
// client drives, so a card's launch context is always the server's stamp.
func DropReservedLaunchArgs(args map[string]string) map[string]string {
	dropped := false
	for _, res := range ReservedLaunchArgs {
		if _, ok := args[res]; ok {
			dropped = true
			break
		}
	}
	if !dropped {
		return args
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		if !IsReservedLaunchArg(k) {
			out[k] = v
		}
	}
	return out
}

// KeepStoredLaunchContext re-stamps the reserved launch context of the card a
// wholesale patch edits: the patch's (already-filtered) args land, the
// payload's identity — which the patching client neither wrote nor may
// rewrite — survives the swap untouched.
func KeepStoredLaunchContext(stored, patched map[string]string) map[string]string {
	out := make(map[string]string, len(patched)+len(ReservedLaunchArgs))
	for k, v := range patched {
		out[k] = v
	}
	for k, v := range stored {
		if IsReservedLaunchArg(k) {
			out[k] = v
		}
	}
	return out
}

// DropServerMintedLaunchVars keeps the two bot-arg filters one class: a write
// surface drops what the server mints (the publish grant) and what the server
// stamps (the launch context) in one gesture, and no surface can drop one and
// keep the other.
func DropServerMintedLaunchVars(args map[string]string) map[string]string {
	return DropReservedLaunchArgs(store.DropServerMintedVars(args))
}
