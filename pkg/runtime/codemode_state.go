package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bindCodemodeState freezes the authority for an engine activation. The
// bank follows the durable run ID, not a session, workspace or branch.
func (e *Engine) bindCodemodeState(ctx context.Context, run *store.Run) model.CodemodeState {
	bank := store.AsCodemodeScratchStore(e.store)
	if bank == nil || run == nil || run.ID == "" {
		return nil
	}
	tenant, _ := store.TenantFromContext(ctx)
	if (run.TenantID != "" && tenant != run.TenantID) || (tenant == "" && store.IsWithoutTenantFilter(ctx)) {
		return nil
	}
	observation, err := store.CaptureRunObservation(store.RunDataHeader{RunID: run.ID, Format: store.ValueFormat(run.FormatVersion), Protocol: run.DataProtocol, BirthID: run.BirthID}, run.TenantID, true)
	if err != nil {
		return nil
	}
	entries, bytes := ir.ResolveCodemodeStoreLimits(run.CodemodeBoundsOverride, e.workflow.CodemodeBounds)
	return runCodemodeState{bank: bank, runID: run.ID, tenant: tenant, observation: observation, limits: store.CodemodeScratchLimits{Entries: entries, Bytes: bytes}}
}

type runCodemodeState struct {
	bank        store.CodemodeScratchStore
	runID       string
	tenant      string
	observation store.RunObservation
	limits      store.CodemodeScratchLimits
}

func (s runCodemodeState) StoreState(ctx context.Context, key string, value json.RawMessage) error {
	if tenant, _ := store.TenantFromContext(ctx); tenant != s.tenant || (tenant == "" && store.IsWithoutTenantFilter(ctx)) {
		return scripthost.ErrStateUnavailable
	}
	bound, err := store.BindRunObservation(ctx, s.observation)
	if err != nil {
		return codemodeStateError(err)
	}
	return codemodeStateError(s.bank.PutCodemodeScratch(bound, s.runID, key, value, s.limits))
}

func (s runCodemodeState) LoadState(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if tenant, _ := store.TenantFromContext(ctx); tenant != s.tenant || (tenant == "" && store.IsWithoutTenantFilter(ctx)) {
		return nil, false, scripthost.ErrStateUnavailable
	}
	bound, err := store.BindRunObservation(ctx, s.observation)
	if err != nil {
		return nil, false, codemodeStateError(err)
	}
	value, found, err := s.bank.LoadCodemodeScratch(bound, s.runID, key)
	if err != nil {
		return nil, false, codemodeStateError(err)
	}
	return value, found, nil
}

func codemodeStateError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrCodemodeScratchInvalid):
		return scripthost.ErrStateInvalid
	case errors.Is(err, store.ErrCodemodeScratchQuota):
		return scripthost.ErrStateQuota
	case errors.Is(err, store.ErrRunNotFound), errors.Is(err, store.ErrRunDeleted), errors.Is(err, store.ErrRunDataIdentityChanged):
		return scripthost.ErrStateUnavailable
	default:
		// An unexpected backend failure is fatal to the script, not a miss.
		return errors.New("runtime: codemode state storage operation failed")
	}
}

// claimWithCodemodeQuota persists the override before the pause can be
// consumed or the runner notified that it owns the attempt.
func (e *Engine) claimWithCodemodeQuota(ctx context.Context, run *store.Run, claim store.CodemodeQuotaResumeClaim, quota store.CodemodeScratchLimits) (bool, error) {
	claimer := store.AsCodemodeQuotaResumeClaimer(e.store)
	if claimer == nil {
		return false, errors.New("this store cannot restate codemode quotas atomically with a resume claim")
	}
	bounds, err := claimer.ClaimResumeWithCodemodeQuota(ctx, run.ID, claim, quota)
	if err != nil || bounds == nil {
		return false, err
	}
	run.CodemodeBoundsOverride = bounds
	return true, nil
}
