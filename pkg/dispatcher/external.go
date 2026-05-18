package dispatcher

import (
	"errors"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// External tracker factories. These translate the dispatcher.Config
// shapes into the corresponding tracker package adapter options and
// instantiate the adapter. Used by both the Manager (studio-driven
// flow) and the standalone `iterion dispatch` CLI.

func buildGitHubTrackerFromConfig(cfg *GitHubTrackerConfig) (tracker.Tracker, error) {
	if cfg == nil {
		return nil, errors.New("dispatcher: tracker.kind=github requires tracker.github block")
	}
	mapping := make(map[string]tracker.LabelSelector, len(cfg.StateMapping))
	for state, sel := range cfg.StateMapping {
		mapping[state] = tracker.LabelSelector{
			LabelsInclude: sel.LabelsInclude,
			LabelsExclude: sel.LabelsExclude,
		}
	}
	return tracker.NewGitHub(tracker.GitHubOptions{
		Repo:          cfg.Repo,
		Token:         cfg.Token,
		IncludeLabels: cfg.IncludeLabels,
		ExcludeLabels: cfg.ExcludeLabels,
		ClaimedLabel:  cfg.ClaimedLabel,
		StateMapping:  mapping,
	})
}

func buildForgejoTrackerFromConfig(cfg *ForgejoTrackerConfig) (tracker.Tracker, error) {
	if cfg == nil {
		return nil, errors.New("dispatcher: tracker.kind=forgejo requires tracker.forgejo block")
	}
	mapping := make(map[string]tracker.LabelSelector, len(cfg.StateMapping))
	for state, sel := range cfg.StateMapping {
		mapping[state] = tracker.LabelSelector{
			LabelsInclude: sel.LabelsInclude,
			LabelsExclude: sel.LabelsExclude,
		}
	}
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

func init() {
	// Wire the production factories at package init so Manager.Start
	// can build any tracker kind without consumer-side registration.
	buildGitHubTracker = buildGitHubTrackerFromConfig
	buildForgejoTracker = buildForgejoTrackerFromConfig
}
