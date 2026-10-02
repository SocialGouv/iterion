package orgsweep

import (
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/pkg/orgusage"
)

// PurgeOrg deletes org_usage rows by an _id regex, and that collection now
// holds TWO id kinds. The filter and the key builders are kept in step by a
// comment; this is the witness. It needs no Mongo: the ids come from the real
// constructors, so a change to either side reddens here.
func TestOrgUsagePurgeFilter_MatchesBothKindsAndNothingElse(t *testing.T) {
	const orgID = "org-abc"
	re := regexp.MustCompile("^(org|forkauthor)\\|" + regexp.QuoteMeta(orgID) + "\\|")

	month := "2026-09"
	must := []string{
		string(orgusage.OrgSubject(orgID)) + "|" + month,
		string(orgusage.ForkAuthorSubject(orgID, "github", "1234")) + "|" + month,
	}
	for _, id := range must {
		if !re.MatchString(id) {
			t.Fatalf("%q survives an org purge — a kind left behind outlives the org it belonged to", id)
		}
	}

	// Adversarial neighbours: an id that merely STARTS with the purged one,
	// another org carrying it as an author segment, and a different kind.
	mustNot := []string{
		string(orgusage.OrgSubject(orgID+"-suffix")) + "|" + month,
		string(orgusage.OrgSubject("team-of-"+orgID)) + "|" + month,
		string(orgusage.ForkAuthorSubject("other-org", "github", orgID)) + "|" + month,
		"wh|" + orgID + "|w1|" + month,
	}
	for _, id := range mustNot {
		if re.MatchString(id) {
			t.Fatalf("%q is deleted by a purge of %q — the filter reaches a document that is not this org's", id, orgID)
		}
	}
}
