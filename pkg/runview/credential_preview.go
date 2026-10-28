package runview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// Permission is the run-level tool-permission-gate mode the caller is
	// about to launch with ("off"|"ask"|"deny") — the same knob the launch
	// request carries — so the preview's permission screen answers for
	// THAT launch. Empty inherits the workflow/node permission: DSL then
	// ITERION_PERMISSION, exactly as the launch would. See
	// docs/permissions.md.
	Permission string `json:"permission,omitempty"`
	// LLMRouting is the RUN level of the adaptive-routing policy the
	// caller is about to launch with (ADR-121) — folded as the HEAD
	// Launcher layer so the preview's whitelist answers the launch the
	// operator shapes, not the launch the defaults would make. Optional.
	LLMRouting *llmroute.Policy `json:"llm_routing,omitempty"`
	// Vars is the launch's own --var-style map the caller is about to
	// launch with — the personal lane's operator-owned values, read the
	// way fill, restore and the gate read them (ir.LaunchVarsView under
	// ADR-129 §4). The webhook lane resolves its vars from the config
	// layers and the delivery payload, so it takes no caller values.
	Vars map[string]string `json:"vars,omitempty"`
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
	ID        string `json:"id"`
	Tier      string `json:"tier"`
	Source    string `json:"source"`
	Provider  string `json:"provider"`
	Wire      string `json:"wire"`
	Rank      int    `json:"rank"`
	Label     string `json:"label"`
	Pinned    bool   `json:"pinned"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Selection string `json:"selection"`
	Selected  bool   `json:"selected"`
	// RouteOnly marks a selected key sealed for the routes that NAME its
	// provider and nothing else (secrets.RunBundle.PinnedAPIKeys): it does
	// not fill its wire for the run's other nodes.
	RouteOnly    bool                       `json:"route_only,omitempty"`
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
	// DoorWants/DoorReason are the routing fallback door's state
	// (ADR-121 § Delivery 2, slice 5): the credential kinds the run's
	// own set lacks that its policy ladder would dispatch, and what the
	// donor-consented pool answers for them — computed OUTSIDE the tier
	// walk (the pool block above stays bypassed for these runs by the
	// plan), by the same derivation the live consult runs. Empty when
	// the door never arms: no policy, no held credential of its own, or
	// no missing dispatchable kind.
	DoorWants  []string `json:"door_wants,omitempty"`
	DoorReason string   `json:"door_reason,omitempty"`
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
	// LLMRoutePolicy is the launch's RESOLVED adaptive-routing policy
	// (ADR-121 §1) — resolved by the server exactly as the launch site
	// would, so the preview's whitelist answers what the launch will seal
	// (the parity doctrine: fill, restore and preview apply the same
	// answer). Nil is back-filled from the launch shape's own
	// LLMRoutePolicy before the fold (the top level wins when both are
	// set), so a producer that resolved the policy onto
	// Launch.LLMRoutePolicy keeps its fold AND its whitelist and fallback
	// door — this field and the launch shape leave PreviewCredentials
	// equal, and every consumer reads one policy. Nil on both = the launch
	// is unwired; everything is sealable.
	LLMRoutePolicy *store.RunLLMRoutePolicy
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
	wf, _, _, err := compileForLaunch(spec.Launch.FilePath, spec.Launch.Source, spec.Launch.BundleDir, s.effectiveWorkDir(spec.Launch.WorkDir))
	if err != nil {
		return CredentialPreview{}, err
	}
	// The SAME unknown-input refusal the launch funnel applies (#1757): a
	// preview over a var no var of the workflow declares would answer for a
	// launch that refuses itself. The operator's opt-out rides with a
	// visible warning instead of silently reading the defaults.
	var previewWarnings []string
	if unknown := ir.UnknownInputNames(wf, spec.Launch.Vars); len(unknown) > 0 {
		if !spec.Launch.AllowUnknownInputs {
			return CredentialPreview{}, fmt.Errorf("credential preview input %s names no var of the workflow (declared: %s)",
				strings.Join(unknown, ", "), strings.Join(ir.DeclaredVarNames(wf), ", "))
		}
		previewWarnings = append(previewWarnings, fmt.Sprintf("Input %s name(s) no var of the workflow — riding on the operator's opt-out (declared: %s).", strings.Join(unknown, ", "), strings.Join(ir.DeclaredVarNames(wf), ", ")))
	}
	// The resolved policy rides the spec's TOP-LEVEL field — the server
	// resolves it exactly as the launch site would — while the fold reads
	// the launch shape's own. The two carriers are normalized HERE, before
	// any consumer: the top level wins when both are set, and a nil top
	// level is back-filled from the launch shape, so a producer that
	// resolved onto the embedded field keeps its fold AND hands the
	// publisher's slot whitelist and fallback door the same policy — they
	// read the top level, the launch publisher reads the launch shape. Nil
	// on both leaves the fold a no-op, an unwired launch.
	if spec.LLMRoutePolicy != nil {
		spec.Launch.LLMRoutePolicy = spec.LLMRoutePolicy
	} else {
		spec.LLMRoutePolicy = spec.Launch.LLMRoutePolicy
	}
	// The SAME fold the launch paths run, before the same permission
	// screen (ADR-129 §6): PreviewCredentials does not pass through
	// Service.Launch, so a fold in Launch alone would leave the preview
	// deriving candidates, pool wants and warnings from the unfolded set —
	// the operator shown credentials the launch never acquires. One
	// shared body with the two launch entries (foldAndScreen), so the
	// order exists in exactly one place.
	if err := foldAndScreen(wf, &spec.Launch); err != nil {
		return CredentialPreview{}, err
	}
	out, err := p.PreviewCredentials(ctx, spec, wf)
	if err != nil {
		return CredentialPreview{}, err
	}
	out.Warnings = append(out.Warnings, previewWarnings...)
	return out, nil
}
