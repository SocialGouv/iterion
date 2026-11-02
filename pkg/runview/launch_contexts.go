package runview

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// BotContextResolver is the launch-time materialiser of operator-bound
// context documents (ADR-147, #1908): the binding store the operator's
// policy lives in, and the fetch seam the server wires onto
// pluginsource.Fetcher. Nil on a service that cannot materialise (no
// fetcher, no binding store) — launches then behave exactly as before, and
// the binding route keeps refusing the context kind.
type BotContextResolver struct {
	Bindings secrets.BotSecretBindingStore
	Fetch    secrets.ContextFetchFunc
	// MaxBytes bounds one materialised document; zero reads
	// secrets.DefaultMaxContextBytes.
	MaxBytes int64
}

// WithBotContextResolver wires the materialiser of operator-bound context
// documents into every launch this service drives. Optional: a service
// without it launches without contexts, which is what every launch did
// before the seam existed.
func WithBotContextResolver(r *BotContextResolver) ServiceOption {
	return func(s *Service) { s.botContexts = r }
}

// composeBoundContexts resolves the bot's operator-bound context documents
// and folds their delivery INTO the launch's attachment-promote chain, so
// the one seam every hand-off already honours — in-process
// (runtime.WithAttachmentPromote), detached and cloud
// (PromoteForHandoff) — carries them with no new wire: in cloud the write
// lands in the shared blob store and the runner reads it back through
// RunStore.OpenAttachment, the blob reference of ADR-075 that D7 picked.
//
// The composition order IS the precedence of ADR-147 D4: the engine's
// bundle-default promotion runs first (author), this composite overwrites
// under the same name (operator), and the previous promote — the launch's
// own uploads — runs last (launch). WriteAttachment overwrites
// unconditionally on both store twins, so the order decides, never a
// "held" check that could not tell an author's default from an operator's
// upload.
//
// Resolution is EAGER, here on the launching process: the fetch and its
// failures belong to the launch, where they are said (a skipped binding is
// logged against the run id and flagged degraded on its record), and the
// closure the hand-off invokes is then a pure store write.
func (s *Service) composeBoundContexts(ctx context.Context, runID string, spec *LaunchSpec, wf *ir.Workflow) {
	r := s.botContexts
	if r == nil || r.Bindings == nil || r.Fetch == nil || spec.BotID == "" || wf == nil || len(wf.Attachments) == 0 {
		return
	}
	tenantID, _ := store.TenantFromContext(ctx)
	if tenantID == "" {
		// A tenant-less launch (a local run of a file, a tenant-less
		// fixture) binds nothing: bindings are org policy, stamped per team.
		s.logger.Debug("runview: launch of bot %q carries no tenant — context bindings are not consulted", spec.BotID)
		return
	}
	declared := make(map[string]bool, len(wf.Attachments))
	for name := range wf.Attachments {
		declared[name] = true
	}
	resolved, skipped, err := secrets.ResolveContextBindings(ctx, r.Bindings, r.Fetch, tenantID, spec.BotID, declared, r.MaxBytes, s.logger)
	if err != nil {
		// The one fatal case the resolver keeps: the LIST failing means
		// nothing can tell a healthy bot from one whose contexts are all
		// lost. A launch that cannot say what it binds refuses to guess —
		// but as a warn-and-proceed, not a failed launch: the run without
		// its context is the pluginsource skip discipline, and the binding
		// records show nothing because the store is the thing that failed.
		s.logger.Warn("runview: could not list the context bindings of bot %q for run %s — the run proceeds with NO bound context: %v", spec.BotID, runID, err)
		return
	}
	for _, sk := range skipped {
		s.logger.Warn("runview: run %s proceeds WITHOUT the bound context %q of bot %s (binding flagged degraded): %v", runID, sk.Binding.DeclaredName(), spec.BotID, sk.Err)
	}
	if len(resolved) == 0 {
		return
	}
	// Deterministic write order: two bindings never share a name (the
	// store's unique key), so any order is correct — a sorted one is also
	// reproducible.
	slices.SortFunc(resolved, func(a, b secrets.ResolvedContext) int {
		return strings.Compare(a.Name, b.Name)
	})
	prev := spec.AttachmentPromote
	names := make([]string, 0, len(resolved))
	for _, rc := range resolved {
		names = append(names, rc.Name)
	}
	s.logger.Info("runview: run %s carries %d bound context document(s) of bot %s (%s)", runID, len(resolved), spec.BotID, strings.Join(names, ", "))
	spec.AttachmentPromote = func(promoteCtx context.Context, promoteRunID string) error {
		for _, rc := range resolved {
			rec := store.AttachmentRecord{
				Name:             rc.Name,
				OriginalFilename: rc.Filename,
				MIME:             rc.MIME,
			}
			if err := s.store.WriteAttachment(promoteCtx, promoteRunID, rec, bytes.NewReader(rc.Content)); err != nil {
				return fmt.Errorf("runview: write bound context %q: %w", rc.Name, err)
			}
		}
		if prev != nil {
			return prev(promoteCtx, promoteRunID)
		}
		return nil
	}
}
