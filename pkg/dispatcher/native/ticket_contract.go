package native

import (
	"strings"
)

// Well-known BotArgs keys for cross-bot multi-pipeline tickets. Iterion does
// not interpret Town/game-specific semantics beyond what admission and the
// /pipelines UI need; bots own the request JSON on disk pointed by InputPathKey.
//
// See docs/native-tracker.md § Ticket contract (bot_args) and ADR-076.
const (
	// BotArgInputPath is the primary immutable request file path (relative to
	// the workspace). Upsert key with Bot: (bot, input_path).
	BotArgInputPath = "input_path"
	// BotArgRevisionID / BotArgRequestHash — immutability / cache identity.
	BotArgRevisionID  = "revision_id"
	BotArgRequestHash = "request_hash"
	// Correlation ids.
	BotArgAssetID   = "asset_id"
	BotArgFeatureID = "feature_id"
	BotArgFamilyID  = "family_id"
	// BotArgPipelineKind — mesh | humanoid | feature | custom (filter surface).
	BotArgPipelineKind = "pipeline_kind"
	// Serialized artefact / dependency lists (JSON strings in bot_args).
	BotArgProduces = "produces"
	BotArgConsumes = "consumes"
	BotArgDocRefs  = "doc_refs"
	// BotArgAutoReady — when truthy, waiting_deps → ready on unblock (else backlog).
	BotArgAutoReady = "auto_ready"
	// BotArgRequireBlockerLabels — comma-separated labels every hard blocker
	// must also carry once done (e.g. "accepted"). Empty = state-only gate.
	BotArgRequireBlockerLabels = "require_blocker_labels"
	// BotArgSpawnedFrom — planner ticket id that published this card. Kept in
	// bot_args for contract visibility; Issue.ParentID is the store-canonical
	// pointer (kept in sync on create). Distinct from blockers.
	BotArgSpawnedFrom = "spawned_from"
	// BotArgRole — optional planner|producer hint for UI grouping. Prefer
	// stamping on planner-published tickets (role=producer) and planner roots
	// (role=planner); empty = infer from parent/children.
	BotArgRole = "role"
)

// ContractDisplayKeys are bot_args keys the /pipelines drawer surfaces in a
// dedicated “Contract” strip (not buried in a generic key dump).
var ContractDisplayKeys = []string{
	BotArgInputPath,
	BotArgAssetID,
	BotArgFeatureID,
	BotArgFamilyID,
	BotArgPipelineKind,
	BotArgProduces,
	BotArgConsumes,
	BotArgRevisionID,
	BotArgRequestHash,
	BotArgDocRefs,
	BotArgAutoReady,
	BotArgRequireBlockerLabels,
	BotArgSpawnedFrom,
	BotArgRole,
}

// UpsertKey returns the (bot, input_path) pair used for ticket reconcile, or
// ("","") when upsert is not possible.
func UpsertKey(bot string, args map[string]string) (botName, inputPath string, ok bool) {
	botName = strings.TrimSpace(bot)
	if botName == "" || args == nil {
		return "", "", false
	}
	inputPath = strings.TrimSpace(args[BotArgInputPath])
	if inputPath == "" {
		return "", "", false
	}
	return botName, inputPath, true
}

// RequireBlockerLabels parses bot_args.require_blocker_labels into a clean list.
func RequireBlockerLabels(args map[string]string) []string {
	if args == nil {
		return nil
	}
	raw := strings.TrimSpace(args[BotArgRequireBlockerLabels])
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	})
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// IssueHasAllLabels reports whether iss carries every required label.
func IssueHasAllLabels(iss *Issue, labels []string) bool {
	if len(labels) == 0 {
		return true
	}
	if iss == nil {
		return false
	}
	have := make(map[string]struct{}, len(iss.Labels))
	for _, l := range iss.Labels {
		have[l] = struct{}{}
	}
	for _, want := range labels {
		if _, ok := have[want]; !ok {
			return false
		}
	}
	return true
}

// FindByBotInputPath returns the first issue matching bot + bot_args.input_path.
// Used by pipeline-board upsert. Match is exact on both strings.
func FindByBotInputPath(store BoardStore, bot, inputPath string) (*Issue, error) {
	bot = strings.TrimSpace(bot)
	inputPath = strings.TrimSpace(inputPath)
	if bot == "" || inputPath == "" {
		return nil, nil
	}
	all, err := store.List(ListFilter{})
	if err != nil {
		return nil, err
	}
	for _, iss := range all {
		if iss == nil || iss.Bot != bot {
			continue
		}
		if iss.BotArgs == nil {
			continue
		}
		if strings.TrimSpace(iss.BotArgs[BotArgInputPath]) == inputPath {
			return iss, nil
		}
	}
	return nil, nil
}
