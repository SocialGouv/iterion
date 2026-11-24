package server

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The forge delivery flattening's table tests (ADR-172, plan §2
// forge_spine_event.go): the provider-normalized forgeIssueDelivery, the
// trigger.Event the router matches, and the legacy strings a spine target
// shares its chains with. They drive the stubs' types and assert the REAL
// expected shapes, so they are red on the stubs' zero values and the router
// lot turns them green.

// spineGitHubCfg is the config the GitHub event tests flatten against: the
// tenant, the webhook id and the allowlist the delivery's Added filters on.
func spineGitHubCfg() webhooks.Config {
	return webhooks.Config{
		ID:                     "ghw",
		TenantID:               "t1",
		Provider:               webhooks.ProviderGitHub,
		ProvisionIntegrationID: spineIntegrationID,
		LabelAllowlist:         []string{"nestor", "bug"},
	}
}

// spineGitLabCfg is the GitLab twin.
func spineGitLabCfg() webhooks.Config {
	return webhooks.Config{
		ID:                     "w1",
		TenantID:               "t1",
		Provider:               webhooks.ProviderGitLab,
		ProvisionIntegrationID: spineIntegrationID,
		LabelAllowlist:         []string{"nestor", "bug"},
	}
}

// gitlabIssuePayload is the GitLab issue hook adding labels in the given
// payload order, with the given current label set.
func gitlabIssuePayload(t *testing.T, current, added []string) gitlab.ParsedIssue {
	t.Helper()
	p, err := gitlab.ParseIssue([]byte(glIssueAddedLabelsBody(current...)))
	if err != nil {
		t.Fatal(err)
	}
	p.AddedLabels = added
	return p
}

func TestForgeIssueDelivery(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		parsed, err := prforge.ParseIssues([]byte(ghIssueLabelBody("nestor")))
		if err != nil {
			t.Fatal(err)
		}
		want := forgeIssueDelivery{
			Provider:    webhooks.ProviderGitHub,
			ProjectPath: spineRepo,
			Number:      42,
			Title:       "Add a CSV export endpoint",
			Body:        "Users need their data as CSV.",
			URL:         "https://github.com/acme/widgets/issues/42",
			State:       "open",
			Labels:      nil, // the payload lists no current labels
			Added:       []string{"nestor"},
			CloneURL:    "https://github.com/acme/widgets.git",
			RepoRef:     "", // GitHub carries no branch: the runner clones the default
			Sender:      "maintainer-bob",
		}
		if got := githubIssueDelivery(spineGitHubCfg(), parsed); !reflect.DeepEqual(got, want) {
			t.Fatalf("githubIssueDelivery = %+v, want %+v", got, want)
		}
	})
	t.Run("github, a label outside the allowlist adds nothing", func(t *testing.T) {
		parsed, err := prforge.ParseIssues([]byte(ghIssueLabelBody("question")))
		if err != nil {
			t.Fatal(err)
		}
		got := githubIssueDelivery(spineGitHubCfg(), parsed)
		if len(got.Added) != 0 {
			t.Fatalf("Added = %v, want empty — the repository pre-gate narrows the added labels", got.Added)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		parsed, err := gitlab.ParseIssue([]byte(glIssueAddedLabelsBody("nestor")))
		if err != nil {
			t.Fatal(err)
		}
		want := forgeIssueDelivery{
			Provider:    webhooks.ProviderGitLab,
			ProjectPath: spineRepo,
			ProjectID:   42,
			Number:      42,
			Title:       "Add CSV export",
			Body:        "as CSV",
			URL:         "https://gitlab.com/acme/widgets/-/issues/42",
			State:       "opened",
			Labels:      []string{"nestor"},
			Added:       []string{"nestor"},
			CloneURL:    "https://gitlab.com/acme/widgets.git",
			RepoRef:     "main", // GitLab's default branch, the base the bot cuts its MR from
			Sender:      "maintainer-bob",
		}
		if got := gitlabIssueDelivery(spineGitLabCfg(), parsed); !reflect.DeepEqual(got, want) {
			t.Fatalf("gitlabIssueDelivery = %+v, want %+v", got, want)
		}
	})
	// One GitLab event can add several labels: Added keeps the payload's own
	// order whatever it is — the router reads the smallest matching label
	// through trigger.MatchedAddedLabel, never the payload order.
	for name, added := range map[string][]string{
		"nestor first": {"nestor", "bug"},
		"bug first":    {"bug", "nestor"},
	} {
		t.Run("gitlab two added labels, "+name, func(t *testing.T) {
			got := gitlabIssueDelivery(spineGitLabCfg(), gitlabIssuePayload(t, added, added))
			if !reflect.DeepEqual(got.Added, added) {
				t.Fatalf("Added = %v, want %v — the payload order survives the flattening", got.Added, added)
			}
		})
	}
}

func TestForgeIssueDelivery_Event(t *testing.T) {
	// The lane identity the handler hands the router: the forge delivery id
	// is set (both handlers set meta.DeliveryID), so the event's ID carries
	// laneEvent.key()'s delivery form.
	meta := webhookEventMeta{
		Kind:        "issues",
		Action:      "labeled",
		ProjectPath: spineRepo,
		SubjectID:   "issue:42",
		SubjectURL:  "https://github.com/acme/widgets/issues/42",
		DeliveryID:  "d-1",
	}
	t.Run("github", func(t *testing.T) {
		parsed, err := prforge.ParseIssues([]byte(ghIssueLabelBody("nestor")))
		if err != nil {
			t.Fatal(err)
		}
		d := githubIssueDelivery(spineGitHubCfg(), parsed)
		// Field by field, on what the ADR fixes: the router may stamp more
		// (an occurred-at, the tenant) — the shape this test pins is the
		// fields the match and the answers read.
		ev := d.event(spineGitHubCfg(), meta)
		if want := "forge:github:integration:" + spineIntegrationID + ":delivery:d-1"; ev.ID != want {
			t.Fatalf("event id = %q, want %q", ev.ID, want)
		}
		if ev.Source != trigger.SourceForge {
			t.Fatalf("source = %q, want %q", ev.Source, trigger.SourceForge)
		}
		if ev.Kind != bundle.ForgeEventIssueLabeled || ev.Action != trigger.ForgeIssueAction {
			t.Fatalf("kind/action = %q/%q, want %q/%q", ev.Kind, ev.Action, bundle.ForgeEventIssueLabeled, trigger.ForgeIssueAction)
		}
		if ev.Repo != spineRepo || ev.Actor != "maintainer-bob" {
			t.Fatalf("repo/actor = %q/%q, want %q/%q", ev.Repo, ev.Actor, spineRepo, "maintainer-bob")
		}
		wantSubject := trigger.Subject{
			Type:  "issue",
			ID:    "issue:42",
			URL:   "https://github.com/acme/widgets/issues/42",
			Title: "Add a CSV export endpoint",
			Body:  "Users need their data as CSV.",
			State: "open",
		}
		if ev.Subject != wantSubject {
			t.Fatalf("subject = %+v, want %+v", ev.Subject, wantSubject)
		}
		if !reflect.DeepEqual(ev.AddedLabels, []string{"nestor"}) {
			t.Fatalf("added labels = %v, want [nestor]", ev.AddedLabels)
		}
		if v, ok := ev.Payload["forge_kind"].(string); !ok || v != "issues" {
			t.Fatalf("payload forge_kind = %v, want %q", ev.Payload["forge_kind"], "issues")
		}
	})
	t.Run("gitlab, both payload orders share the event identity", func(t *testing.T) {
		meta := meta
		meta.SubjectURL = "https://gitlab.com/acme/widgets/-/issues/42"
		for _, added := range [][]string{{"nestor", "bug"}, {"bug", "nestor"}} {
			d := gitlabIssueDelivery(spineGitLabCfg(), gitlabIssuePayload(t, added, added))
			ev := d.event(spineGitLabCfg(), meta)
			// The event's id is the forge delivery's, never the labels': two
			// payload orders of one delivery are ONE event.
			if wantID := "forge:gitlab:integration:" + spineIntegrationID + ":delivery:d-1"; ev.ID != wantID {
				t.Fatalf("event id = %q, want %q", ev.ID, wantID)
			}
			if !reflect.DeepEqual(ev.AddedLabels, added) {
				t.Fatalf("AddedLabels = %v, want %v", ev.AddedLabels, added)
			}
		}
	})
}

func TestForgeIssueDelivery_LegacyTrigger(t *testing.T) {
	t.Run("github, the applied label", func(t *testing.T) {
		parsed, err := prforge.ParseIssues([]byte(ghIssueLabelBody("nestor")))
		if err != nil {
			t.Fatal(err)
		}
		d := githubIssueDelivery(spineGitHubCfg(), parsed)
		if got := d.legacyTrigger(spineGitHubCfg()); got != "nestor" {
			t.Fatalf("legacyTrigger = %q, want %q — GitHub's legacy trigger is the label", got, "nestor")
		}
	})
	// GitLab's legacy trigger is FirstMatchingLabel in payload order: the
	// key legacy used for THIS event, read only for its identity.
	for _, tc := range []struct {
		name  string
		added []string
		want  string
	}{
		{name: "the first matching label in payload order", added: []string{"nestor", "bug"}, want: "nestor"},
		{name: "the other order picks the other label", added: []string{"bug", "nestor"}, want: "bug"},
		{name: "a label outside the allowlist is skipped", added: []string{"question", "bug"}, want: "bug"},
		{name: "nothing matches", added: []string{"question"}, want: ""},
	} {
		t.Run("gitlab "+tc.name, func(t *testing.T) {
			d := gitlabIssueDelivery(spineGitLabCfg(), gitlabIssuePayload(t, tc.added, tc.added))
			if got := d.legacyTrigger(spineGitLabCfg()); got != tc.want {
				t.Fatalf("legacyTrigger = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestForgeIssueDelivery_LegacyString(t *testing.T) {
	// The exact legacy idempotency bases, byte for byte: a spine target's
	// chains and a rollback's reads key on these strings.
	t.Run("github", func(t *testing.T) {
		parsed, err := prforge.ParseIssues([]byte(ghIssueLabelBody("nestor")))
		if err != nil {
			t.Fatal(err)
		}
		d := githubIssueDelivery(spineGitHubCfg(), parsed)
		want := knowledge.ChecksumHex([]byte("gh|issue|t1|ghw|acme/widgets|42|nestor"))
		if got := d.legacyString(spineGitHubCfg(), "nestor"); got != want {
			t.Fatalf("legacyString = %q, want %q", got, want)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		d := gitlabIssueDelivery(spineGitLabCfg(), gitlabIssuePayload(t, []string{"nestor"}, []string{"nestor"}))
		want := knowledge.ChecksumHex([]byte("gl|issue|t1|w1|42|42|nestor"))
		if got := d.legacyString(spineGitLabCfg(), "nestor"); got != want {
			t.Fatalf("legacyString = %q, want %q", got, want)
		}
	})
	t.Run("the checksummed base feeds the bot lane", func(t *testing.T) {
		// The spine base is botLaneBase(legacyString, bot): the legacy string
		// of the SAME trigger, extended — never replaced. The two functions
		// must compose on the exact bytes above.
		d := gitlabIssueDelivery(spineGitLabCfg(), gitlabIssuePayload(t, []string{"nestor"}, []string{"nestor"}))
		base := d.legacyString(spineGitLabCfg(), "nestor")
		if base == "" {
			t.Fatalf("legacyString is empty — the bot lane's base would be the checksum of an empty string")
		}
		if got, want := botLaneBase(base, "nestor"), knowledge.ChecksumHex([]byte(base+"|bot:nestor")); got != want {
			t.Fatalf("botLaneBase = %q, want %q", got, want)
		}
	})
}

// The stub route answers before anything routes: the witnesses never reach
// it while the handler branches are absent, and when they exist the router
// lot has replaced it.
func TestForgeRoutingDefaultReader(t *testing.T) {
	t.Setenv("ITERION_FORGE_ROUTING_DEFAULT", "issues=spine,pull_requests=legacy")
	if got := forgeRoutingDefaultFor(webhooks.LaneIssues); got != webhooks.RoutingSpine {
		t.Fatalf("issues default = %q, want %q", got, webhooks.RoutingSpine)
	}
	if got := forgeRoutingDefaultFor(webhooks.LanePullRequests); got != webhooks.RoutingLegacy {
		t.Fatalf("pull_requests default = %q, want %q", got, webhooks.RoutingLegacy)
	}
	if got := forgeRoutingDefaultFor(webhooks.LaneComments); got != "" {
		t.Fatalf("comments default = %q, want empty — the env names no mode for it", got)
	}
	t.Setenv("ITERION_FORGE_ROUTING_DEFAULT", "")
	if got := forgeRoutingDefaultFor(webhooks.LaneIssues); got != "" {
		t.Fatalf("unset env = %q, want empty", got)
	}
}
