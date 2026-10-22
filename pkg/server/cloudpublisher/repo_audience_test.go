package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// seedRepoKey seals an anthropic key for team1 restricted to repos (and to
// bots, when given), with a known fingerprint.
func seedRepoKey(t *testing.T, keys secrets.ApiKeyStore, sealer secrets.Sealer, name string, bots, repos []string) secrets.ApiKey {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("sk-ant-"+name))
	if err != nil {
		t.Fatal(err)
	}
	k := secrets.ApiKey{ID: id, ScopeTeamID: "team1", Provider: secrets.ProviderAnthropic, Name: name, Fingerprint: "fp-" + name,
		SealedSecret: sealed, Bots: bots, Repos: repos, CreatedAt: time.Now().UTC()}
	if err := keys.Create(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return k
}

// A tenant anthropic key the REPOSITORY audience withholds from this launch
// is not a native credential the run holds, any more than one the bot
// audience withholds: facade_default=auto (the default) must then let the
// shared tiers' facade key take the anthropic wire's default, or the run
// seals nothing and falls back to the runner pod's environment.
func TestFacadeAutoIgnoresAKeyTheRepoAudienceWithholds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bots       []string
		repos      []string
		launchRepo string
		wantAnth   bool
		wantZAIDef bool
	}{
		{"bot audience withholds the anthropic key", []string{"other-bot"}, nil, "acme/y", false, true},
		{"repo audience withholds the anthropic key", nil, []string{"acme/x"}, "acme/y", false, true},
		{"repo audience admits the anthropic key", nil, []string{"acme/x"}, "acme/x", true, false},
		{"repo-restricted key, launch names no repo", nil, []string{"acme/x"}, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
			seedRepoKey(t, keys, sealer, "scoped", tc.bots, tc.repos)
			fd := string(platformcfg.FacadeAuto)
			p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
				logger: testLogger(), usageCaps: usagecap.NewMemStore(),
				platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil)}
			ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), tc.launchRepo)
			res, err := p.resolveAndSealCredentials(ctx, "run-native", "", "team1", "webhook:cfg-1", "review-pr", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			var b secrets.RunBundle
			if res.secretsRef != "" {
				rec, err := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, res.secretsRef)
				if err != nil {
					t.Fatal(err)
				}
				b, err = secrets.OpenRunBundle(mustDEKSealer(t, res.dek), rec.TenantID, "", "run-native", rec.KeyID, rec.SealedBundle)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := b.APIKeys[secrets.ProviderAnthropic] != ""; got != tc.wantAnth {
				t.Errorf("anthropic sealed = %v, want %v", got, tc.wantAnth)
			}
			if got := b.APIKeys[secrets.ProviderZAI] != ""; got != tc.wantZAIDef {
				t.Errorf("platform z.ai key as the wire default = %v, want %v — the run is left with NO anthropic-wire default while its only native key is withheld", got, tc.wantZAIDef)
			}
		})
	}
}

// A key withheld for its repository is reported as such — in the
// withholding line and in the pin diagnostic — never blamed on the bot:
// a confident line pointing at the wrong list sends the operator to edit
// the wrong one. A launch that names no repository says so.
func TestWithheldDiagnosticsNameTheRepository(t *testing.T) {
	for _, tc := range []struct {
		launchRepo string
		want       string
	}{
		{"acme/y", "its repository audience [acme/x] does not name repository acme/y"},
		{"", "its repository audience [acme/x] does not name a repository (this launch names none)"},
	} {
		sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
		keys := secrets.NewMemoryApiKeyStore()
		k := seedRepoKey(t, keys, sealer, "repo-x-only", nil, []string{"acme/x"})
		var buf bytes.Buffer
		p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: iterlog.New(iterlog.LevelInfo, &buf)}
		ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), tc.launchRepo)
		if _, err := p.resolveAndSealCredentials(ctx, "run-pin", "", "team1", "webhook:cfg-1", "review-pr", "", nil, map[string]string{"anthropic": k.ID}, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil); err != nil {
			t.Fatal(err)
		}
		log := buf.String()
		if n := strings.Count(log, tc.want); n != 2 {
			t.Fatalf("launch repo %q: the repository cause appears %d times, want 2 (the withholding line and the pin line):\n%s", tc.launchRepo, n, log)
		}
		if strings.Contains(log, "does not name bot") {
			t.Fatalf("launch repo %q: a repository withholding was blamed on the bot:\n%s", tc.launchRepo, log)
		}
	}
	// Both audiences refusing are both named.
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	keys := secrets.NewMemoryApiKeyStore()
	seedRepoKey(t, keys, sealer, "both", []string{"other-bot"}, []string{"acme/x"})
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: iterlog.New(iterlog.LevelInfo, &buf)}
	ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), "acme/y")
	if _, err := p.resolveAndSealCredentials(ctx, "run-both", "", "team1", "webhook:cfg-1", "review-pr", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil); err != nil {
		t.Fatal(err)
	}
	if log := buf.String(); !strings.Contains(log, "its workload audience [other-bot] does not name bot review-pr and its repository audience [acme/x] does not name repository acme/y") {
		t.Fatalf("both causes must be named:\n%s", log)
	}
}

// The wiring: a LAUNCH stamps the repository it serves (spec.ProjectPath)
// on the walk, so a key restricted to it is spent there and withheld
// elsewhere; a RESUME re-reads it from the run document, so the stamp
// survives a resume and a re-resolution reads the same repository.
func TestSubmitLaunchAndResume_StampTheRepositoryOnTheWalk(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	ctx := store.WithIdentity(context.Background(), "team1", "owner1")
	apiOnly := seedRepoKey(t, keys, sealer, "api-only", nil, []string{"acme/api"})
	p := &Publisher{
		apiKeys:    keys,
		usageCaps:  usagecap.NewMemStore(),
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	wf := &ir.Workflow{Name: "wf"}
	launch := func(runID, projectPath string) []string {
		t.Helper()
		if _, err := p.SubmitLaunch(ctx, runID, runview.LaunchSpec{
			FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", ProjectPath: projectPath,
		}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitLaunch %s: %v", runID, err)
		}
		run, err := rs.LoadRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return run.CredFingerprints
	}
	if fps := launch("run-api", "acme/api"); len(fps) != 1 || fps[0] != "fp-api-only" {
		t.Fatalf("a launch for acme/api carries %v, want the key restricted to it", fps)
	}
	if fps := launch("run-web", "acme/web"); len(fps) != 0 {
		t.Fatalf("a launch for acme/web carries %v — the repository audience was not read from the launch", fps)
	}
	if fps := launch("run-none", ""); len(fps) != 0 {
		t.Fatalf("a launch naming no repository carries %v", fps)
	}
	// The resume re-resolves from the run document's repository: the key
	// is still spent for acme/api, and once the key's audience moves on,
	// the resume withholds it and clears the stamp.
	run, err := rs.LoadRun(ctx, "run-api")
	if err != nil {
		t.Fatal(err)
	}
	resume := func() []string {
		t.Helper()
		run, err := rs.LoadRun(ctx, "run-api")
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusPausedOperator
		if err := rs.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: "run-api", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitResume: %v", err)
		}
		run, err = rs.LoadRun(ctx, "run-api")
		if err != nil {
			t.Fatal(err)
		}
		return run.CredFingerprints
	}
	if run.ProjectPath != "acme/api" {
		t.Fatalf("the run document carries project path %q, want acme/api — the resume has nothing to read", run.ProjectPath)
	}
	if fps := resume(); len(fps) != 1 || fps[0] != "fp-api-only" {
		t.Fatalf("a resume of the acme/api run carries %v, want the key restricted to it (the resume did not read the run's repository)", fps)
	}
	if _, err := keys.Patch(context.Background(), apiOnly.ID, secrets.ApiKeyPatch{Repos: &[]string{"acme/other"}}); err != nil {
		t.Fatal(err)
	}
	if fps := resume(); len(fps) != 0 {
		t.Fatalf("a resume after the audience moved on still carries %v", fps)
	}
}

// The preview names the cause of a repository withholding but never the
// key's own list: a shared-tier key belongs to another tenant, and the
// repositories it names are theirs to know. The bot reason has the same
// discipline.
func TestPreviewReasonNamesNoOtherTenantsRepositories(t *testing.T) {
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	keys := secrets.NewMemoryApiKeyStore()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("sk-ant-platform"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Create(context.Background(), secrets.ApiKey{ID: id, ScopeTeamID: secrets.PlatformTenantID, Provider: secrets.ProviderAnthropic, Name: "platform-ant",
		SealedSecret: sealed, Repos: []string{"customer-x/secret-acquisition"}, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	seedRepoKey(t, keys, sealer, "team-scoped", nil, []string{"team1/elsewhere"})
	fd := string(platformcfg.FacadeAuto)
	p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger(), usageCaps: usagecap.NewMemStore(),
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil)}
	preview, err := previewReadOnly(p).PreviewCredentials(secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), "team1/app"), previewSpec("team1", "owner1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, c := range preview.Candidates {
		if c.State != "repo_filtered" {
			continue
		}
		seen++
		if strings.Contains(c.Reason, "customer-x") || strings.Contains(c.Reason, "elsewhere") || strings.Contains(c.Reason, "[") {
			t.Fatalf("the preview reason names a key's repository list: %q (tier %s)", c.Reason, c.Tier)
		}
		if !strings.Contains(c.Reason, "does not name repository team1/app") {
			t.Fatalf("the preview reason does not name the launch's repository: %q", c.Reason)
		}
	}
	if seen != 2 {
		t.Fatalf("want the team key and the platform key withheld for their repository audience, saw %d: %+v", seen, preview.Candidates)
	}
}
