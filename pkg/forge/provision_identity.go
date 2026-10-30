package forge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/google/uuid"
)

var ErrProvisionBusy = errors.New("forge: repo provisioning is already running; retry")

// ErrMintNotApproved is the derivation chokepoint's refusal as the
// orchestrator reports it: this provisioning reached the point where it
// reserves a NEW hook identity — a team-scoped token that outlives the
// credential asking for it — and ApproveTeamTokenMint refused. It wraps the
// chokepoint's own error, so the route answers the credential's status (401
// for one that no longer stands, 403 for one that may never mint).
var ErrMintNotApproved = errors.New("forge: the hook token this provisioning mints was refused by the derivation chokepoint")

var provisionLeaseMu sync.Mutex

// provisionLease reduces overlapping work. It is not a remote-write fence:
// a timed-out HTTP request can still apply after its lease has expired.
func (o *Orchestrator) provisionLease(ctx context.Context, tenant, conn, repo string) (context.Context, func(), error) {
	provisionLeaseMu.Lock()
	if o.ProvisionLeases == nil {
		o.ProvisionLeases = lease.NewMemoryStore()
	}
	ls := o.ProvisionLeases
	provisionLeaseMu.Unlock()
	key, _ := json.Marshal([]string{tenant, conn, repo})
	name := fmt.Sprintf("forge-provision:%x", sha256.Sum256(key))
	owner := uuid.NewString()
	ttl := o.ProvisionLeaseTTL
	if ttl == 0 {
		ttl = time.Minute
	}
	ok, err := ls.Acquire(ctx, name, owner, o.clock(), ttl)
	if err != nil {
		return ctx, nil, fmt.Errorf("forge: acquire provisioning lease: %w", err)
	}
	if !ok {
		return ctx, nil, ErrProvisionBusy
	}
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	errtrack.Go("forge.provisionLease", func() {
		defer close(done)
		ticker := time.NewTicker(max(ttl/3, time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := ls.Renew(ctx, name, owner, o.clock(), ttl)
				if err != nil {
					cancel(fmt.Errorf("forge: renew provisioning lease: %w", err))
					return
				}
				if !ok {
					cancel(lease.ErrLost)
					return
				}
			}
		}
	})
	return ctx, func() {
		cancel(nil)
		<-done
		rctx, releaseCancel := repairContext(ctx)
		defer releaseCancel()
		if err := ls.Release(rctx, name, owner); err != nil && !errors.Is(err, lease.ErrLost) && o.LogWarn != nil {
			o.LogWarn("forge: release provisioning lease: %v", err)
		}
	}, nil
}

// reserveHookIdentity durably reserves an immutable secret BEFORE either side
// is written. Concurrent/ambiguous retries share it. Legacy hash-only hooks
// migrate through a distinct URL; their active secret is never overwritten.
func (o *Orchestrator) reserveHookIdentity(ctx context.Context, req ProvisionRequest, conn Connection, ri RepoIntegration, prev *webhooks.Config, managedSecret string, bot *ProjectBot) (RepoIntegration, string, string, error) {
	if o.Sealer == nil {
		return ri, "", "", errors.New("forge: provisioning requires a secret sealer")
	}
	create := ri.ID == ""
	if create || ri.Retired {
		id, version := ri.ID, ri.Version
		if create {
			id = o.id()
		}
		ri = RepoIntegration{ID: id, Version: version, TenantID: req.TenantID, ConnectionID: conn.ID, Provider: conn.Provider, RepoFullName: req.RepoFullName, CreatedAt: o.clock(), CreatedBy: req.ActorID}
	}
	webhookID, sealed := ri.WebhookID, ri.WebhookSecretSealed
	if len(sealed) == 0 {
		webhookID, sealed = ri.CandidateWebhookID, ri.CandidateSecretSealed
		if webhookID == "" {
			// THE mint of this package, and the only one: ask the
			// chokepoint HERE, where the decision is taken, inside the
			// provisioning lease and after the sealed secret has been
			// found missing. A re-provision that reuses the sealed
			// secret never reaches this line, so it is never refused —
			// and a repo deleted between a caller's read and this
			// reservation cannot slip a mint past the credential check,
			// which a gate at the route's door would have let through.
			// The context the seam returns carries the verdict the ask
			// granted, and it is THE context the mint runs under: the
			// primitive itself refuses one without the verdict, so the
			// class is closed twice — at the ask and at the mint.
			mctx, err := o.approveTeamTokenMint(ctx)
			if err != nil {
				return ri, "", "", err
			}
			webhookID = o.id()
			plaintext, _, _, _, err := webhooks.MintToken(mctx)
			if err != nil {
				return ri, "", "", fmt.Errorf("forge: mint webhook identity: %w", err)
			}
			sealed, err = o.Sealer.Seal([]byte(plaintext), provisionSecretAAD(ri.ID, webhookID))
			if err != nil {
				return ri, "", "", fmt.Errorf("forge: seal webhook identity: %w", err)
			}
			ri.CandidateWebhookID, ri.CandidateSecretSealed = webhookID, sealed
		}
	}
	plaintext, err := o.Sealer.Open(sealed, provisionSecretAAD(ri.ID, webhookID))
	if err != nil {
		return ri, "", "", fmt.Errorf("forge: open webhook identity: %w", err)
	}
	if len(plaintext) == 0 {
		return ri, "", "", errors.New("forge: empty sealed webhook identity")
	}
	// Freeze the source policy through its snapshot CAS before migration. All
	// ordinary writers conflict until the operator switches to the new endpoint.
	if prev != nil && prev.ID != webhookID && !prev.ProvisionFrozen {
		prev.ProvisionIntegrationID = ri.ID
		prev.ProvisionFrozen = true
		if err := o.Webhooks.Update(ctx, *prev); err != nil {
			return ri, "", "", err
		}
		prev.Version++
	}
	ri.ProvisionPending = true
	ri.ManagedSecretID, ri.ProjectBot = managedSecret, bot
	ri.UpdatedAt = o.clock()
	if create {
		err = o.Integrations.Create(ctx, ri)
	} else {
		err = o.Integrations.Update(ctx, ri)
		ri.Version++
	}
	if err != nil {
		return ri, "", "", fmt.Errorf("forge: reserve webhook identity: %w", err)
	}
	return ri, webhookID, string(plaintext), nil
}

func provisionSecretAAD(integrationID, webhookID string) []byte {
	return []byte("forge_hook_identity:" + integrationID + ":" + webhookID)
}

// approveTeamTokenMint asks the wired chokepoint whether the credential
// behind ctx may mint the team token this reservation is about to fabricate,
// and hands back the context carrying its verdict — the one the mint runs
// under, which the primitive itself requires. A nil seam refuses: this
// package mints a team-scoped token, and minting one without the derivation
// chokepoint's approval is exactly what #2687 closes — a server wires the
// seam (pkg/server passes auth.MintTeamToken through
// auth.Service.CheckDerivation and stamps the verdict), a test stamps the
// same verdict through the same constructor the gate uses.
func (o *Orchestrator) approveTeamTokenMint(ctx context.Context) (context.Context, error) {
	if o.ApproveTeamTokenMint == nil {
		return ctx, fmt.Errorf("%w: no ApproveTeamTokenMint seam is wired, and a team-token mint refuses without the derivation chokepoint's approval", ErrMintNotApproved)
	}
	mctx, err := o.ApproveTeamTokenMint(ctx)
	if err != nil {
		return ctx, fmt.Errorf("%w: %w", ErrMintNotApproved, err)
	}
	return mctx, nil
}
