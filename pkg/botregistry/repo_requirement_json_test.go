package botregistry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestRepoRequirementJSONWireShape locks the /api/v1/bots wire shape of
// the repo: block — the studio reads snake_case keys; a missing json tag
// silently hides the create/none launch options (caught live 2026-07-17).
func TestRepoRequirementJSONWireShape(t *testing.T) {
	e := Entry{Repo: &bundle.RepoRequirement{Mode: "optional", AllowCreate: true, Purpose: "p", Visibility: "private"}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"repo"`, `"mode":"optional"`, `"allow_create":true`, `"purpose":"p"`, `"visibility":"private"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("wire JSON missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"AllowCreate"`) || strings.Contains(s, `"Mode"`) {
		t.Fatalf("wire JSON leaked Go field names: %s", s)
	}
}
