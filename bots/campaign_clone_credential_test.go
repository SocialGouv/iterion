package bots

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// TestCampaignDeclaresTheCloneCredentialInItsChildsShape holds campaign's
// forge_token to the shape the reasons above require of it: no inline value,
// or the launch's forge_token override has nothing to fill; the runner's
// clone-credential gate (refuseUngrantedCloneCredential) refuses a run whose
// clone secret withholds the `file` sink; `optional` keeps a local run — host-
// authenticated git, no stored token — admitted, as modernize's own
// declaration does; and campaign declares it on its child's behalf, so it
// declares it as the child does, field for field but the description. Where
// both runs materialise it they put it in the same place. And the child runs
// in campaign's sandbox, whose egress proxy rewrites and screens through
// campaign's guard (docs/secrets.md: a child in its parent's sandbox, whose
// rewriter is the parent's): the token leaves under campaign's `hosts:`, so a
// host scope declared on one side only is enforced for the wrong run.
func TestCampaignDeclaresTheCloneCredentialInItsChildsShape(t *testing.T) {
	t.Parallel()
	campaign := compileBot(t, "campaign")
	modernize := compileBot(t, "modernize")
	if campaign == nil || modernize == nil {
		t.Fatal("campaign or modernize did not compile")
	}
	parent, child := campaign.Secrets["forge_token"], modernize.Secrets["forge_token"]
	if child == nil {
		t.Fatal("modernize declares no forge_token — this test's premise (the child's own clone/push credential) is gone; revisit campaign's declaration with it")
	}
	if parent == nil {
		t.Fatal("campaign declares no forge_token: a repo-targeted launch's forge_token override has nothing to fill, the runner clones a private repository credential-less, and run_lot's child never receives the credential it declares")
	}
	if parent.HasInlineValue() {
		t.Errorf("campaign's forge_token carries an inline value (%q): the launch resolves no stored secret, binding or override for it, so a repo-targeted launch's forge_token never reaches the clone and the private repository is cloned credential-less", parent.Value)
	}
	if !parent.Grants(secretsink.File) {
		t.Errorf("campaign's forge_token grants %v and withholds %q — the runner's clone-credential gate refuses the run before the clone", parent.Sinks, secretsink.File)
	}
	if !parent.Optional {
		t.Error("campaign's forge_token is required — every launch without a stored token is refused, local runs on host-authenticated git included, where modernize runs without one")
	}
	if p, c := secretShape(parent), secretShape(child); !reflect.DeepEqual(p, c) {
		t.Errorf("campaign's forge_token %+v is not modernize's %+v — the child reads its credential where its own declaration puts it, and sends it out through campaign's guard, which applies campaign's declaration",
			p, c)
	}
}
