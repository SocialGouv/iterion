package runview

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// CredentialPreviewSource names a real launch surface. Identity and overrides
// are derived by the HTTP handler; callers cannot choose another owner.
type CredentialPreviewSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

type CredentialPreviewRequest struct {
	Source CredentialPreviewSource `json:"source"`
	BotID  string                  `json:"bot_id,omitempty"`
}

type CredentialPreviewContext struct {
	TeamID string                  `json:"team_id"`
	BotID  string                  `json:"bot_id"`
	Source CredentialPreviewSource `json:"source"`
}

type CredentialPreviewWindow struct {
	Name       string     `json:"name"`
	Percent    *float64   `json:"percent,omitempty"`
	Status     string     `json:"status,omitempty"`
	ObservedAt time.Time  `json:"observed_at"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
	Fresh      bool       `json:"fresh"`
}

type CredentialPreviewCapacity struct {
	LiveRuns          *int     `json:"live_runs,omitempty"`
	MaxConcurrentRuns int      `json:"max_concurrent_runs,omitempty"`
	RemainingUSD      *float64 `json:"remaining_usd,omitempty"`
}

// ID and AccountGroup are opaque identifiers valid only within this response.
// Selected means included by the credential chain, conditional on successful
// materialization and admission; model routing still decides which slot is spent.
// Selection (selected/shadowed/not_consulted) is independent of availability
// State: a pinned blocked key or a restored credential can still be selected.
type CredentialPreviewCandidate struct {
	ID           string                     `json:"id"`
	Tier         string                     `json:"tier"`
	Source       string                     `json:"source"`
	Provider     string                     `json:"provider"`
	Wire         string                     `json:"wire"`
	Rank         int                        `json:"rank"`
	Label        string                     `json:"label"`
	Pinned       bool                       `json:"pinned"`
	State        string                     `json:"state"`
	Reason       string                     `json:"reason,omitempty"`
	Selection    string                     `json:"selection"`
	Selected     bool                       `json:"selected"`
	Conditional  bool                       `json:"conditional"`
	AccountGroup string                     `json:"account_group,omitempty"`
	ReopensAt    *time.Time                 `json:"reopens_at,omitempty"`
	Windows      []CredentialPreviewWindow  `json:"windows"`
	Capacity     *CredentialPreviewCapacity `json:"capacity,omitempty"`
}

type CredentialPreviewWire struct {
	Wire         string   `json:"wire"`
	CandidateIDs []string `json:"candidate_ids"`
}

type CredentialPreviewPool struct {
	Considered bool     `json:"considered"`
	Reason     string   `json:"reason,omitempty"`
	Wants      []string `json:"wants"`
}

type CredentialPreview struct {
	ObservedAt time.Time                    `json:"observed_at"`
	Context    CredentialPreviewContext     `json:"context"`
	Wires      []CredentialPreviewWire      `json:"wires"`
	Candidates []CredentialPreviewCandidate `json:"candidates"`
	Pool       CredentialPreviewPool        `json:"pool"`
	Warnings   []string                     `json:"warnings"`
}

// CredentialPreviewSpec is internal and accepts only a server-authorized source.
// Launch supplies the same bot compilation and model/key override vocabulary.
type CredentialPreviewSpec struct {
	Context CredentialPreviewContext
	OwnerID string
	Launch  LaunchSpec
}

type CredentialPreviewer interface {
	PreviewCredentials(context.Context, CredentialPreviewSpec, *ir.Workflow) (CredentialPreview, error)
}

var ErrCredentialPreviewUnavailable = errors.New("credential preview is unavailable")

// PreviewCredentials compiles exactly as Launch does, but never creates a run,
// snapshot, reservation, probe, or secret bundle.
func (s *Service) PreviewCredentials(ctx context.Context, spec CredentialPreviewSpec) (CredentialPreview, error) {
	p, ok := s.publisher.(CredentialPreviewer)
	if !ok {
		return CredentialPreview{}, ErrCredentialPreviewUnavailable
	}
	wf, _, _, err := compileForLaunch(spec.Launch.FilePath, spec.Launch.Source, spec.Launch.BundleDir)
	if err != nil {
		return CredentialPreview{}, err
	}
	if err := ValidateModelOverridePermissions(wf, toModelOverrides(spec.Launch.ModelOverrides), spec.Launch.Permission); err != nil {
		return CredentialPreview{}, err
	}
	return p.PreviewCredentials(ctx, spec, wf)
}
