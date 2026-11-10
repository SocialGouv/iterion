package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume's funding is judged on the run's FROZEN content boundary, the one
// SubmitResume's walk replays (boundaryFrozen): a launch that sealed no
// gateway key — an own_keys stamp naming none, or a run stamped before the
// producer — seals none on a resume. A fork run launched on a vendor route
// whose bot was edited since to ride the gateway, on a team holding its own
// gateway key, is therefore refused at publish; the judge the gate asks
// before metering refuses it too, with the walk's own sentence, which names
// the frozen boundary as the cause — never a key to provision, the team
// holds one. A stamp whose launch sealed the team's key admits on both (the
// control). A trusted run held to its team's gateway whose key is gone since
// is refused by both on the boundary's strict arm.
//
// Every row drives the two public paths on one run document: the judge
// (ResumeFundingRefusal) first, then the operator's forced resume
// (SubmitResume), and asserts they agree — both admit, or both refuse with
// the same sentence.
//
// Mutations, each seen red and restored byte-identical:
//   - ResumeFundingRefusal judging without the frozen boundary (the
//     preview's probe re-deriving it, as a launch does) → the own_keys and
//     unstamped rows admit on the judge while the walk refuses, and the
//     trusted row admits on the judge;
//   - the preview's team_gateway reading derived instead of replayed → the
//     trusted row admits on the judge;
//   - the untrusted refusal's gateway remedy ignoring the replay → both
//     surfaces tell the operator to provision the gateway key the team holds
//     (red on the cause).
func TestResumeFundingRefusal_JudgesTheFrozenContentBoundaryTheWalkReplays(t *testing.T) {
	const provisionRemedy = "provision the team's gateway key"
	subject := string(orgusage.ForkAuthorSubject("team1", "github", "4242"))
	for _, row := range []struct {
		name string
		// trusted runs the row on the default runner, without the fork
		// lane's pool and author.
		trusted bool
		// boundary is the run's frozen stamp; keyID names the team's gateway
		// key ("" when the team holds none).
		boundary func(keyID string) *store.RunContentBoundary
		holdsKey bool
		// cause is what the shared refusal names; "" when both admit.
		cause string
	}{
		{"a fork run launched own_keys on a vendor route, its bot edited to the gateway", false,
			func(string) *store.RunContentBoundary { return ownKeys() }, true,
			"content boundary own_keys"},
		{"a fork run stamped before the producer, its bot edited to the gateway", false,
			func(string) *store.RunContentBoundary { return nil }, true,
			"content boundary none"},
		{"a fork run whose launch sealed the team's gateway key (the control)", false,
			func(id string) *store.RunContentBoundary {
				return &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: id, GatewayHost: gwHost}
			}, true, ""},
		{"a trusted run held to its team's gateway, its key gone since", true,
			func(string) *store.RunContentBoundary {
				return &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "key-gone", GatewayHost: gwHost}
			}, false, "content boundary team_gateway"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			p.store = rs
			published := 0
			p.publishRun = func(context.Context, *queue.RunMessage) error { published++; return nil }
			// The launch's funding: the team's own anthropic key.
			seedTeamAnthropic(t, p)
			keyID := ""
			if row.holdsKey {
				seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { keyID = k.ID })
			}
			run := &store.Run{
				ID: "run-frozen", TenantID: "team1", OwnerID: "owner1", BotID: gwBot,
				Status: store.RunStatusFailedResumable, ProjectPath: gwRepo,
				PinnedProviders: []string{"anthropic"}, ContentBoundary: row.boundary(keyID),
			}
			spec := runview.ResumeSpec{RunID: run.ID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n", Force: true}
			if !row.trusted {
				run.Trust, run.RunnerPool, run.ForkAuthorSubject = store.RunTrustFork, "forks", subject
				spec.Admitted = &runview.GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: subject}
			}
			ctx := context.Background()
			if err := rs.SaveRun(ctx, run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}
			prior, err := rs.LoadRun(ctx, run.ID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}

			judged := p.ResumeFundingRefusal(ctx, prior, gatewayOnlyWorkflow())
			walked := p.SubmitResume(ctx, spec, gatewayOnlyWorkflow(), &runview.CompiledSource{Hash: "hash"})

			if row.cause == "" {
				if judged != nil || walked != nil {
					t.Fatalf("judge = %v, walk = %v — want both to admit: the launch sealed the team's key", judged, walked)
				}
				if published != 1 {
					t.Fatalf("the admitted resume published %d message(s), want 1", published)
				}
				return
			}
			var judge, walk *UnfundedProvidersError
			if !errors.As(walked, &walk) {
				t.Fatalf("walk = %v, want the typed unfunded refusal — the fixture must be refused at publish", walked)
			}
			if !errors.As(judged, &judge) {
				t.Fatalf("judge = %v while the walk refuses: %v — the gate would meter a resume the publisher refuses", judged, walk)
			}
			if judge.Error() != walk.Error() {
				t.Fatalf("the judge and the walk refuse with different sentences:\njudge: %s\nwalk:  %s", judge, walk)
			}
			if !strings.Contains(walk.Error(), row.cause) {
				t.Fatalf("refusal %q does not name the frozen boundary (%q) as its cause", walk, row.cause)
			}
			if !row.trusted && strings.Contains(walk.Error(), provisionRemedy) {
				t.Fatalf("refusal %q tells the operator to provision a gateway key the team holds — the frozen boundary is the cause", walk)
			}
			if published != 0 {
				t.Fatalf("a refused resume published %d message(s)", published)
			}
		})
	}
}
