package bundle

import (
	"fmt"
	"reflect"
	"slices"
)

// ForgeIssueDefaultArgsVar is the workflow var a labelled issue's text lands
// in when the bot names none: the implementer contract every label-started
// bot follows.
const ForgeIssueDefaultArgsVar = "feature_prompt"

// legacyForgeKeyPrefix prefixes the key of a routing invocation derived
// from a bot's `forge:` block rather than from an invocation it declares.
const legacyForgeKeyPrefix = "legacy:"

// ForgeRoutingInvocation is one way a labelled issue starts a bot, as the
// forge routing spine reads it from the bot's manifest.
type ForgeRoutingInvocation struct {
	// Inv is the invocation the manifest declares, or the synthetic one
	// standing for a `forge:` block that claims issue_labeled on its own.
	Inv Invocation
	// Key is InvocationKey for a declared invocation, "legacy:issue_labeled"
	// for a synthetic one.
	Key string
	// Labels are the labels that start the bot, as the manifest spells
	// them; empty declares none of its own.
	Labels []string
	// Aliased reports that Labels come from a key other than forge.labels
	// (Invocation.ForgeTriggerLabels).
	Aliased bool
	// Synthetic marks the invocation derived from the `forge:` block.
	Synthetic bool
	// ArgsVar is the workflow var the issue's text lands in.
	ArgsVar string
	// OpensMR reports that the bot opens a merge request the issue is
	// back-linked from.
	OpensMR bool
}

// ForgeIssueInvocations returns the ways a labelled issue starts the bot
// whose `forge:` block is fr and whose invocations are invs, with the
// warnings an author is owed about the ones that derive nothing.
//
// A bot's own declarations come first, as they do when its webhook rule is
// built: each kind=forge invocation on event issue_labeled whose mode
// resolves derives one entry, in manifest order. A mode no lane honours
// derives nothing and is warned; an invocation repeating the key of an
// earlier one is warned and the earlier one answers for both. An entry's
// args var is its invocation's, else the bot's first command's, else
// ForgeIssueDefaultArgsVar; it opens a merge request as that first command
// does, or by default when the bot declares no command.
//
// When no declared invocation derives and fr claims issue_labeled, the block
// alone speaks: one synthetic entry carrying the label route such a bot is
// started by — board mode, the first command's args var or
// ForgeIssueDefaultArgsVar, that command's opens_mr or true, the block's
// labels, and no context vars: the label route never carries a command's.
//
// invs may be the declared invocations (Manifest.Invocations) or the
// effective ones (EffectiveInvocations), which every catalogue entry
// carries. For a bundle with no `invocations:` block the effective set is
// SyntheticInvocations' migration shim — one direct-mode kind=forge
// invocation per block event, issue_labeled included — and read as declared
// it would derive a direct route with no label, where the bot is started on
// the block's label route. So a set equal, element for element, to the shim
// of fr declares nothing, and the block speaks. The one consequence: a
// manifest that declares by hand exactly the shim's set is read like the
// shim, which is the label route such a bot is started by today.
func ForgeIssueInvocations(fr *ForgeRequirements, invs []Invocation) ([]ForgeRoutingInvocation, []string) {
	if len(invs) > 0 && reflect.DeepEqual(invs, SyntheticInvocations(&Manifest{Forge: fr})) {
		invs = nil
	}
	argsVar, opensMR := ForgeIssueDefaultArgsVar, true
	if cmd, ok := firstCommandInvocation(invs); ok {
		if cmd.ArgsVar != "" {
			argsVar = cmd.ArgsVar
		}
		opensMR = cmd.Command.OpensMR
	}

	var (
		out      []ForgeRoutingInvocation
		warnings []string
	)
	firstByKey := forgeKeyOwners(invs)
	for idx, inv := range invs {
		if inv.Kind != InvocationKindForge || inv.Forge == nil || inv.Forge.Event != ForgeEventIssueLabeled {
			continue
		}
		if _, err := inv.EffectiveMode(); err != nil {
			warnings = append(warnings, fmt.Sprintf("invocations[%d]: derives no %s route: %v", idx, ForgeEventIssueLabeled, err))
			continue
		}
		key := InvocationKey(invs, idx)
		if first := firstByKey[key]; first != idx {
			warnings = append(warnings, forgeDuplicateKeyWarning(idx, first, inv))
			continue
		}
		labels, aliased := inv.ForgeTriggerLabels()
		entry := ForgeRoutingInvocation{
			Inv:     inv,
			Key:     key,
			Labels:  labels,
			Aliased: aliased,
			ArgsVar: argsVar,
			OpensMR: opensMR,
		}
		if inv.ArgsVar != "" {
			entry.ArgsVar = inv.ArgsVar
		}
		out = append(out, entry)
	}
	if len(out) > 0 || fr == nil || !slices.Contains(fr.Events, ForgeEventIssueLabeled) {
		return out, warnings
	}
	labels := append([]string(nil), fr.Labels...)
	return []ForgeRoutingInvocation{{
		Inv: Invocation{
			Kind:    InvocationKindForge,
			Mode:    ExecutionBoard,
			ArgsVar: argsVar,
			Forge:   &InvocationForge{Event: ForgeEventIssueLabeled, Labels: labels},
		},
		Key:       legacyForgeKeyPrefix + ForgeEventIssueLabeled,
		Labels:    labels,
		Synthetic: true,
		ArgsVar:   argsVar,
		OpensMR:   opensMR,
	}}, warnings
}

// ForgeDuplicateKeyWarnings names each kind=forge invocation that repeats
// the key of an earlier one — the same event, actions and labels — in
// manifest order. The manifest decoder accepts such a pair; the later
// invocation derives nothing, and this is where its author is told.
func ForgeDuplicateKeyWarnings(invs []Invocation) []string {
	firstByKey := forgeKeyOwners(invs)
	var warnings []string
	for idx, inv := range invs {
		if !derivesForgeKey(inv) {
			continue
		}
		if first := firstByKey[InvocationKey(invs, idx)]; first != idx {
			warnings = append(warnings, forgeDuplicateKeyWarning(idx, first, inv))
		}
	}
	return warnings
}

// forgeKeyOwners maps each forge key to the index of the first invocation
// that derives it. An invocation whose mode no lane honours derives nothing
// and owns no key, so it never makes a later one a duplicate.
func forgeKeyOwners(invs []Invocation) map[string]int {
	owners := map[string]int{}
	for idx, inv := range invs {
		if !derivesForgeKey(inv) {
			continue
		}
		key := InvocationKey(invs, idx)
		if _, taken := owners[key]; !taken {
			owners[key] = idx
		}
	}
	return owners
}

// derivesForgeKey reports whether inv is a kind=forge invocation a route
// can be derived from: it carries its forge block and a mode a lane honours.
func derivesForgeKey(inv Invocation) bool {
	if inv.Kind != InvocationKindForge || inv.Forge == nil {
		return false
	}
	_, err := inv.EffectiveMode()
	return err == nil
}

func forgeDuplicateKeyWarning(idx, first int, inv Invocation) string {
	return fmt.Sprintf("invocations[%d]: two forge invocations derive the same key: the second is ignored (invocations[%d] already declares event %q with the same actions and labels)", idx, first, inv.Forge.Event)
}

// firstCommandInvocation is the bot's first kind=command invocation, the one
// a labelled issue borrows its args var and its opens_mr from.
func firstCommandInvocation(invs []Invocation) (Invocation, bool) {
	for _, inv := range invs {
		if inv.Kind == InvocationKindCommand && inv.Command != nil {
			return inv, true
		}
	}
	return Invocation{}, false
}
