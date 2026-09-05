package dispatcher

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
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
	project, err := buildGitHubProjectOptions(cfg)
	if err != nil {
		return nil, err
	}
	return tracker.NewGitHub(tracker.GitHubOptions{
		Repo:            cfg.Repo,
		Token:           cfg.Token,
		IncludeLabels:   cfg.IncludeLabels,
		ExcludeLabels:   cfg.ExcludeLabels,
		AuthorAllowlist: cfg.AuthorAllowlist,
		ClaimedLabel:    cfg.ClaimedLabel,
		StateMapping:    mapping,
		Project:         project,
	})
}

// buildGitHubProjectOptions turns a `project:` block into the tracker's board
// mode, building the forge board client from the config's own token. Returns
// nil when no project is configured.
func buildGitHubProjectOptions(cfg *GitHubTrackerConfig) (*tracker.GitHubProjectOptions, error) {
	p := cfg.Project
	if p == nil {
		return nil, nil
	}
	// Unlike the label path, board mode does NOT ride `gh`: Projects v2 is
	// GraphQL, reached with a real API credential, and `gh` authenticates
	// itself from its own config — which a cloud pod does not have.
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("dispatcher: tracker.github.project requires tracker.github.token (the board client is an API client and cannot borrow the gh login)")
	}
	baseURL := strings.TrimSpace(p.BaseURL)
	if baseURL == "" {
		baseURL = forge.DefaultBaseURL(forge.ProviderGitHub)
	}
	admin := forgegithub.New(http.DefaultClient, baseURL, cfg.Token)
	board, ok := forge.AsBoardClient(admin)
	if !ok {
		return nil, errors.New("dispatcher: the github client exposes no project board")
	}
	opts := &tracker.GitHubProjectOptions{
		Owner:             strings.TrimSpace(p.Owner),
		Number:            p.Number,
		OwnerKind:         forge.ProjectOwnerKind(strings.TrimSpace(p.OwnerKind)),
		Board:             board,
		CandidateStatuses: p.CandidateStatuses,
	}
	if len(p.StatusMap) > 0 {
		mapping, err := forge.StatusMappingFromMap(p.StatusMap)
		if err != nil {
			return nil, fmt.Errorf("dispatcher: tracker.github.project.status_map: %w", err)
		}
		opts.StatusMapping = mapping
	}
	return opts, nil
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
