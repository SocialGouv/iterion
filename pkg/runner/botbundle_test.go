package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// materializeBotBundle rebuilds a stored bot bundle from the DB with the
// anti-drift version check: the publisher compiled a specific row version,
// so a row that moved (or vanished) under the queued message must fail the
// attempt EXPLICITLY — pairing the message's IR with newer resources is the
// silent-wrong-result façade the check exists to prevent.
func TestMaterializeBotBundle(t *testing.T) {
	mem := botsource.NewMemoryStore()
	ctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	created, err := mem.Create(ctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "probe",
		Files: map[string]string{
			botsource.MainBotFile: "workflow main:\n  entry: done\n",
			"manifest.yaml":       "name: probe\n",
			"skills/x.md":         "# x",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := &Runner{cfg: Config{BotSources: mem}}

	// Happy path: fetch, materialize, open as a bundle.
	b, cleanup, err := r.materializeBotBundle(context.Background(),
		&queue.BotBundleRef{TenantID: botsource.PlatformTenantID, Slug: "probe", Version: created.Version})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()
	if b == nil || b.SkillsDir == "" {
		t.Fatalf("bundle = %+v, want skills dir wired", b)
	}

	// Version drift (a push landed after the launch) fails loudly.
	_, _, err = r.materializeBotBundle(context.Background(),
		&queue.BotBundleRef{TenantID: botsource.PlatformTenantID, Slug: "probe", Version: created.Version + 1})
	if err == nil || !strings.Contains(err.Error(), "version drift") {
		t.Fatalf("drift must fail naming the drift, got %v", err)
	}

	// A vanished row fails loudly too — never a silent baked fallback.
	_, _, err = r.materializeBotBundle(context.Background(),
		&queue.BotBundleRef{TenantID: botsource.PlatformTenantID, Slug: "gone", Version: 1})
	if err == nil {
		t.Fatal("missing row must fail")
	}

	// No store wired: a ref-carrying message must not silently degrade.
	bare := &Runner{}
	if _, _, err := bare.materializeBotBundle(context.Background(),
		&queue.BotBundleRef{TenantID: "t1", Slug: "probe", Version: 1}); err == nil {
		t.Fatal("nil store must fail explicitly")
	}
}
