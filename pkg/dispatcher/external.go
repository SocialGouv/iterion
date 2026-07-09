package dispatcher

import (
	"errors"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// External tracker factories. These translate the dispatcher.Config
// shapes into the corresponding tracker package adapter options and
// instantiate the adapter. Used by both the Manager (studio-driven
// flow) and the standalone `iterion dispatch` CLI.

// buildLabelSelectorMapping converts the dispatcher config's per-state label
// selectors into the tracker package's equivalent type.
func buildLabelSelectorMapping(stateMapping map[string]LabelSelector) map[string]tracker.LabelSelector {
	mapping := make(map[string]tracker.LabelSelector, len(stateMapping))
	for state, sel := range stateMapping {
		mapping[state] = tracker.LabelSelector{
			LabelsInclude: sel.LabelsInclude,
			LabelsExclude: sel.LabelsExclude,
		}
	}
	return mapping
}

func buildGitHubTrackerFromConfig(cfg *GitHubTrackerConfig) (tracker.Tracker, error) {
	if cfg == nil {
		return nil, errors.New("dispatcher: tracker.kind=github requires tracker.github block")
	}
	mapping := buildLabelSelectorMapping(cfg.StateMapping)
	return tracker.NewGitHub(tracker.GitHubOptions{
		Repo:            cfg.Repo,
		Token:           cfg.Token,
		IncludeLabels:   cfg.IncludeLabels,
		ExcludeLabels:   cfg.ExcludeLabels,
		AuthorAllowlist: cfg.AuthorAllowlist,
		ClaimedLabel:    cfg.ClaimedLabel,
		StateMapping:    mapping,
	})
}

func buildForgejoTrackerFromConfig(cfg *ForgejoTrackerConfig) (tracker.Tracker, error) {
	if cfg == nil {
		return nil, errors.New("dispatcher: tracker.kind=forgejo requires tracker.forgejo block")
	}
	mapping := buildLabelSelectorMapping(cfg.StateMapping)
	return tracker.NewForgejo(tracker.ForgejoOptions{
		Host:          cfg.Host,
		Repo:          cfg.Repo,
		Token:         cfg.Token,
		IncludeLabels: cfg.IncludeLabels,
		ExcludeLabels: cfg.ExcludeLabels,
		ClaimedLabel:  cfg.ClaimedLabel,
		StateMapping:  mapping,
	})
}

func buildGitLabTrackerFromConfig(cfg *GitLabTrackerConfig) (tracker.Tracker, error) {
	if cfg == nil {
		return nil, errors.New("dispatcher: tracker.kind=gitlab requires tracker.gitlab block")
	}
	mapping := buildLabelSelectorMapping(cfg.StateMapping)
	return tracker.NewGitLab(tracker.GitLabOptions{
		Host:          cfg.Host,
		Repo:          cfg.Repo,
		Token:         cfg.Token,
		IncludeLabels: cfg.IncludeLabels,
		ExcludeLabels: cfg.ExcludeLabels,
		ClaimedLabel:  cfg.ClaimedLabel,
		StateMapping:  mapping,
	})
}
