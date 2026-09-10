package forge

import (
	"context"
	"strings"
	"testing"
)

// hookURLFor returns the URL the fake forge recorded for repo.
func hookURLFor(t *testing.T, fa *fakeAdmin, repo string) string {
	t.Helper()
	h, ok := fa.hooks[repo]
	if !ok {
		t.Fatalf("no hook registered for %s", repo)
	}
	return h.URL
}

// TestHookURLFollowsThePublicURLByDefault: the ordinary connection has no
// opinion about its hook address and must move with the deployment.
func TestHookURLFollowsThePublicURLByDefault(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)

	if _, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	got := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(got, "https://iterion.example.com/api/webhooks/") {
		t.Fatalf("hook URL = %q, want it built from the deployment's public URL", got)
	}
}

// TestPinnedConnectionKeepsItsOwnHookBase: a forge that cannot reach the
// deployment's public URL pins its own. GitLab refuses any webhook URL
// outside its outbound allowlist with "Invalid url given" (HTTP 422), and
// listing a host is an administrative act on the forge's side — so without
// this, one such forge pins the public URL of the WHOLE deployment.
func TestPinnedConnectionKeepsItsOwnHookBase(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()

	conn.WebhookBaseURL = "https://iterion.allowlisted.example"
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	got := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(got, "https://iterion.allowlisted.example/api/webhooks/") {
		t.Fatalf("hook URL = %q, want the connection's pinned base — the deployment's "+
			"public URL is one the forge is not allowed to call", got)
	}
}

// TestPinSurvivesAReprovision is the assertion the feature exists for.
// Leaving an old hook URL in place "because nobody re-provisions it" is not
// a state, it is a delay: enabling one more bot, splitting a team, or any
// bots-enable call rewrites the hook from the CURRENT public URL. The pin
// is what makes the exception durable rather than merely un-disturbed.
func TestPinSurvivesAReprovision(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()

	conn.WebhookBaseURL = "https://iterion.allowlisted.example"
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	// The deployment moves to its new domain while this connection cannot.
	o.PublicURL = "https://iterion.cloud"

	// One more bot on the same repo — the full re-provision path.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	got := hookURLFor(t, fa, "group/api")
	if strings.Contains(got, "iterion.cloud") {
		t.Fatalf("the re-provision rewrote the hook to the new public URL (%q). The forge "+
			"refuses that host, so deliveries stop — and the write SUCCEEDS, so nothing "+
			"reports it.", got)
	}
	if !strings.HasPrefix(got, "https://iterion.allowlisted.example/api/webhooks/") {
		t.Fatalf("hook URL = %q, want the pinned base preserved across the re-provision", got)
	}
}

// TestClearingThePinHandsTheConnectionBack: the override is not a one-way
// door — once the forge lists the canonical host, clearing the pin must put
// the connection back on the deployment's public URL at the next provision.
func TestClearingThePinHandsTheConnectionBack(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()

	conn.WebhookBaseURL = "https://iterion.allowlisted.example"
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	conn.WebhookBaseURL = ""
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	got := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(got, "https://iterion.example.com/api/webhooks/") {
		t.Fatalf("hook URL = %q, want the public URL again once the pin is cleared", got)
	}
}
