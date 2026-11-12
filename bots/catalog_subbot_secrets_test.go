package bots

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// A subbot child runs on its parent's credentials: the cloud runner builds the
// child under the parent's message (pkg/runner/subbot.go, `child := *msg`) and
// the in-process path keeps the credentials already stamped on the context
// (runview.StampLocalCredentials). Those credentials are resolved at launch
// from the PARENT's declarations only — the publisher asks the store for the
// names the launched workflow declares (cloudpublisher
// genericSecretNamesForWorkflow), and a launch override fills a declared name
// or nothing (secrets.ResolveGenericWithBindings). So a secret the child
// declares reaches it only when its parent declares it too; otherwise the
// child's declaration resolves to nothing on every run of the parent.
//
// For the forge credential the parent's declaration matters twice: the runner
// clones a repo-targeted run's repository for the ROOT run, before any node,
// with the token resolved for it — a parent that declares none clones a
// private repository credential-less and never starts.

// subbotSecretGaps are the child secrets a parent of this tree does not
// declare, each with the reason it stays undeclared. Being listed is the only
// way a gap passes, and the table is read both ways: an entry whose parent
// now declares the secret, or that names no pair of this tree, fails as loudly
// as an unlisted gap. Keyed "<parent file> <subbot node> <secret>".
var subbotSecretGaps = map[string]string{
	"campaign/main.bot run_lot deploy_credential": "a lot's gate may judge a DEPLOYED application through $DEPLOY_CREDENTIAL, and under campaign that gate finds no credential and refuses; campaign's finalize re-runs the blocked lots' gates and exports none either. Declaring it hands every campaign run the team's platform credential — a credential-surface decision for the operator, kept visible here until it is taken",
}

// TestEverySubbotChildSecretIsDeclaredByItsParent is the class guard: for
// every subbot node of every shipped workflow, every secret the child declares
// is declared by the parent, or named in subbotSecretGaps with its reason.
func TestEverySubbotChildSecretIsDeclaredByItsParent(t *testing.T) {
	t.Parallel()
	pairs, declaring := 0, 0
	seen := map[string]bool{}
	for _, path := range catalogWorkflowFiles(t) {
		parent := compileBotFile(t, path)
		ids := make([]string, 0, len(parent.Nodes))
		for id := range parent.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			sb, ok := parent.Nodes[id].(*ir.SubbotNode)
			if !ok {
				continue
			}
			src := strings.TrimSpace(sb.Source)
			if src == "" || strings.Contains(src, "{{") || strings.Contains(src, "${") || strings.Contains(src, "://") || filepath.IsAbs(src) {
				t.Errorf("%s: subbot %q names %q, a source this guard cannot follow to a file — its child's secrets go unjudged; name the child relative to its parent", path, id, sb.Source)
				continue
			}
			child := compileBotFile(t, filepath.Join(filepath.Dir(path), src))
			pairs++
			names := make([]string, 0, len(child.Secrets))
			for name := range child.Secrets {
				names = append(names, name)
			}
			sort.Strings(names)
			if len(names) > 0 {
				declaring++
			}
			for _, name := range names {
				key := fmt.Sprintf("%s %s %s", filepath.ToSlash(path), id, name)
				why, listed := subbotSecretGaps[key]
				if listed {
					seen[key] = true
				}
				declared := parent.Secrets[name] != nil
				switch {
				case declared && listed:
					t.Errorf("%s: subbot %q's secret %q is declared by its parent and still listed in subbotSecretGaps (%q) — a stale entry tells the next reader a closed gap is open", path, id, name, why)
				case declared:
				case listed:
					if strings.TrimSpace(why) == "" {
						t.Errorf("subbotSecretGaps[%q] has no reason — a gap nobody explained is one nobody can close", key)
					}
				default:
					t.Errorf("%s: subbot %q runs %s, which declares secret %q, and the parent declares no secret of that name — the launch resolves only the parent's declarations and the child runs on them, so the child's %q is empty on every run. Declare it in the parent, in the child's shape, or name the gap in subbotSecretGaps with its reason",
						path, id, src, name, name)
				}
			}
		}
	}
	for key := range subbotSecretGaps {
		if !seen[key] {
			t.Errorf("subbotSecretGaps names %q, which is no subbot child secret of this tree — drop the entry, or fix the name it was meant to spell", key)
		}
	}
	// Floors: the tree ships parents whose children declare secrets
	// (campaign's run_lot runs modernize); a walk that found none judged
	// nothing.
	if pairs < 3 || declaring < 1 {
		t.Fatalf("followed %d subbot pair(s), %d of them with a child that declares secrets — want at least 3 and 1: the walk no longer reaches the tree's subbot nodes, and every verdict above is vacuous", pairs, declaring)
	}
}

// TestCampaignDeclaresTheCloneCredentialInItsChildsShape holds campaign's
// forge_token to the shape the reasons above require of it: the runner's
// clone-credential gate (refuseUngrantedCloneCredential) refuses a run whose
// clone secret withholds the `file` sink; `optional` keeps a local run — host-
// authenticated git, no stored token — admitted, as modernize's own
// declaration does; and campaign declares it on its child's behalf, so it
// declares it as the child does — a file with the same sinks, mount path and
// env — and where both runs materialise it, they put it in the same place.
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
	if !parent.Grants(secretsink.File) {
		t.Errorf("campaign's forge_token grants %v and withholds %q — the runner's clone-credential gate refuses the run before the clone", parent.Sinks, secretsink.File)
	}
	if !parent.Optional {
		t.Error("campaign's forge_token is required — every launch without a stored token is refused, local runs on host-authenticated git included, where modernize runs without one")
	}
	if parent.As != child.As || !slices.Equal(parent.Sinks, child.Sinks) || parent.MountPath != child.MountPath || parent.Env != child.Env {
		t.Errorf("campaign's forge_token (as=%q sinks=%v mount_path=%q env=%q) is not modernize's (as=%q sinks=%v mount_path=%q env=%q) — the child reads its credential where its own declaration puts it",
			parent.As, parent.Sinks, parent.MountPath, parent.Env, child.As, child.Sinks, child.MountPath, child.Env)
	}
}
