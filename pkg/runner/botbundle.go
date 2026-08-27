package runner

import (
	"context"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// materializeBotBundle rebuilds a STORED bot bundle (team-authored bot or
// platform override) from the botsource store into the pod's ephemeral
// scratch (os.MkdirTemp → the emptyDir every other runner temp write uses)
// and opens it as the run's bundle. The dir is a re-derivable cache: a
// redelivery re-materializes from the same authority.
//
// The version check is the anti-drift guard: the publisher resolved and
// compiled THIS row version at launch, so a row that moved (or vanished)
// under the queued message must fail the attempt explicitly — pairing the
// message's IR with newer skills/prompts is the silent-wrong-result façade.
// The caller routes the error through nak/redelivery; a resume re-resolves
// the current version end-to-end and self-heals.
func (r *Runner) materializeBotBundle(ctx context.Context, ref *queue.BotBundleRef) (*bundle.Bundle, func(), error) {
	if r.cfg.BotSources == nil {
		return nil, nil, fmt.Errorf("no bot-source store wired (the message names a stored bundle this runner cannot fetch)")
	}
	tctx := store.WithTenant(ctx, ref.TenantID)
	bs, err := r.cfg.BotSources.GetBySlug(tctx, ref.TenantID, ref.Slug)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch: %w (deleted since launch? resume re-resolves the current bot)", err)
	}
	if bs.Version != ref.Version {
		return nil, nil, fmt.Errorf("version drift: store has v%d, launch resolved v%d — a push landed after this launch; resume to pick up the current version", bs.Version, ref.Version)
	}
	dir, err := os.MkdirTemp("", "iterion-bot-bundle-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := botsource.Materialize(dir, bs.Files); err != nil {
		cleanup()
		return nil, nil, err
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open materialized bundle: %w", err)
	}
	return b, cleanup, nil
}
