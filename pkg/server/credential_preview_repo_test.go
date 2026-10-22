package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// repoCapturePublisher records the repository the HTTP preview stamps on
// the walk's context — what the live launch would stamp from the delivery.
type repoCapturePublisher struct {
	credentialPreviewPublisher
	repo string
}

func (p *repoCapturePublisher) PreviewCredentials(ctx context.Context, s runview.CredentialPreviewSpec, wf *ir.Workflow) (runview.CredentialPreview, error) {
	p.repo = secrets.LaunchRepoFromContext(ctx)
	return p.credentialPreviewPublisher.PreviewCredentials(ctx, s, wf)
}

// The preview names a repository only when the webhook names exactly one,
// LITERALLY. The allowlist is a pattern list (webhooks.MatchProject: "*",
// "group/*"); a pattern stamped as the repository would preview a key
// restricted to "acme/api" as withheld while every delivery spends it,
// and a webhook serving several repositories cannot say which one the
// next delivery is for. The personal source names none.
func TestCredentialPreview_StampsOnlyALiteralWebhookRepository(t *testing.T) {
	for _, tc := range []struct {
		allow []string
		want  string
	}{
		{[]string{"acme/api"}, "acme/api"},
		// Trimmed as the delivery matcher reads it.
		{[]string{" acme/api "}, "acme/api"},
		{[]string{"acme/*"}, ""},
		{[]string{"*"}, ""},
		{[]string{"acme/api", "acme/web"}, ""},
		{nil, ""},
	} {
		s, rs := newTeamForkServer(t, &tierPublisher{})
		pub := &repoCapturePublisher{}
		s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
		configs := webhooks.NewMemoryConfigStore()
		s.webhookConfigs = configs
		cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe", ProjectAllowlist: tc.allow}
		if err := configs.Create(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
		w := callCredentialPreview(s, `{"source":{"kind":"webhook","id":"hook"}}`, auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember})
		if w.Code != http.StatusOK {
			t.Fatalf("allowlist %v: preview=%d %s", tc.allow, w.Code, w.Body.String())
		}
		if pub.repo != tc.want {
			t.Fatalf("allowlist %v: the preview stamped repository %q, want %q", tc.allow, pub.repo, tc.want)
		}
	}
	s, rs := newTeamForkServer(t, &tierPublisher{})
	pub := &repoCapturePublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	w := callCredentialPreview(s, `{"source":{"kind":"personal"},"bot_id":"probe"}`, auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember})
	if w.Code != http.StatusOK || pub.repo != "" {
		t.Fatalf("personal source: %d %s, repo %q, want 200 and no repository", w.Code, w.Body.String(), pub.repo)
	}
}
