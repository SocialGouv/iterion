package cloudpublisher

import (
	"context"
	"fmt"
	"slices"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// GatewayCapacitySubjectOf is the capacity subject of the gateway a key with
// this base URL dials (ADR-167 §1): the host the gateway client dials
// (compatgw.Config.Host), folded by store.GatewayCapacitySubject. "" for a
// URL that names no dialable host. The fold the run stamp and the gateway
// status route share, so a host stamped on a run and the host its key's row
// names are compared in one spelling.
func GatewayCapacitySubjectOf(baseURL string) string {
	return store.GatewayCapacitySubject(compatgw.Config{BaseURL: baseURL}.Host())
}

// capacitySubjectsOf reads the capacity subjects a sealed bundle draws on: the
// gateway subject of every openai_compatible key the bundle seals, in either
// channel — the default (APIKeys with APIBaseURLs) and the pinned one
// (PinnedAPIKeys with PinnedAPIBaseURLs) — deduplicated and sorted. A vendor
// key never contributes: a vendor's URL is compiled, not the bundle's. Never
// the content boundary's host: it is frozen at launch and replayed, while the
// subjects are re-read from the bundle every resolution seals (ADR-167 §2).
//
// A gateway key whose URL names no dialable host contributes no subject, and
// no run carries such a key: the seal of the same resolution
// (secrets.SealRunBundle) refuses a gateway URL that is not the canonical
// https form with a host (ErrGatewayKeyWithoutURL), so the resolution fails
// before any stamp is written.
func capacitySubjectsOf(b secrets.RunBundle) []string {
	gw := secrets.ProviderOpenAICompatible
	var subjects []string
	for _, ch := range []struct{ key, url string }{
		{b.APIKeys[gw], b.APIBaseURLs[gw]},
		{b.PinnedAPIKeys[gw], b.PinnedAPIBaseURLs[gw]},
	} {
		if ch.key == "" {
			continue
		}
		if subject := GatewayCapacitySubjectOf(ch.url); subject != "" && !slices.Contains(subjects, subject) {
			subjects = append(subjects, subject)
		}
	}
	slices.Sort(subjects)
	return subjects
}

// lostCapacityStampRefusal is the fail-closed half of a resume whose re-stamp
// was lost (ADR-167 §2: refused as soon as the old or the new stamp carries a
// subject). It answers the refusal, or nil when neither stamp names a subject.
//
// The new stamp is resealed. The old one is the stamp the lost write left on
// the run document, read again here, after the claim: the document the resume
// read before its claim can predate a concurrent resume that flipped the run,
// stamped a subject and was reverted — a revert puts the status back, never
// the stamp. A document that cannot be read again refuses as well: nothing
// then proves it names no subject.
func (p *Publisher) lostCapacityStampRefusal(ctx context.Context, runID string, resealed []string, writeErr error) error {
	if len(resealed) > 0 {
		return fmt.Errorf("cloudpublisher: resume of run %s refused: re-stamp its capacity subjects (re-sealed %v): %w — a run whose document does not name the gateway it dials is counted on the wrong host", runID, resealed, writeErr)
	}
	current, err := p.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("cloudpublisher: resume of run %s refused: re-stamp its capacity subjects: %w — and the run document could not be read again to prove it names none: %w", runID, writeErr, err)
	}
	if len(current.CapacitySubjects) > 0 {
		return fmt.Errorf("cloudpublisher: resume of run %s refused: re-stamp its capacity subjects (stamped %v, re-sealed none): %w — a run whose document names a gateway it no longer dials holds a place every team shares", runID, current.CapacitySubjects, writeErr)
	}
	return nil
}
