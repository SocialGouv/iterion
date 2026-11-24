package cloudpublisher

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// forfaitAuditGrep is the `grep -E` pattern the operator docs hand out as
// the audit evidence of which runs a consumer subscription paid for. It
// keys on the one line every sealing run gets (logGrantedCredentials), so
// a forfait from any tier — own, org, pool, platform — matches it.
const forfaitAuditGrep = `credentials GRANTED for run=.*\(oauth:(claude_code|codex) `

// The docs that publish the grep carry it verbatim: an operator copies it
// from there, so a drift between the page and this pattern is a blind audit.
func TestForfaitAuditGrep_publishedVerbatim(t *testing.T) {
	want := "grep -E '" + forfaitAuditGrep + "'"
	for _, doc := range []string{"cloud-admin.md", "cloud-public-exposure-checklist.md", "oauth-forfait.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", doc))
		if err != nil {
			t.Fatalf("read docs/%s: %v", doc, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("docs/%s does not publish the forfait audit grep %q", doc, want)
		}
	}
}

// The grep matches the GRANTED line of a run a forfait funded, whichever tier
// filled the slot, and never a run funded by API keys alone.
func TestForfaitAuditGrep_matchesEveryForfaitTier(t *testing.T) {
	re := regexp.MustCompile(forfaitAuditGrep)
	claude := string(secrets.OAuthKindClaudeCode)
	codex := string(secrets.OAuthKindCodex)
	oauth := func(kind string) secrets.RunBundle {
		return secrets.RunBundle{
			OAuthCredentials:  map[string][]byte{kind: []byte("{}")},
			OAuthFingerprints: map[string]string{kind: "fp-" + kind},
		}
	}
	withKey := func(b secrets.RunBundle) secrets.RunBundle {
		b.APIKeys = map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-x"}
		return b
	}
	orgForfait := oauth(codex)
	orgForfait.OrgSourced = map[string]bool{codex: true}
	platformForfait := oauth(claude)
	platformForfait.PlatformSourced = map[string]bool{claude: true}
	poolGrant := &credpool.Grant{Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: codex}}
	orgKey := withKey(secrets.RunBundle{})
	orgKey.OrgSourced = map[string]bool{string(secrets.ProviderAnthropic): true}
	apiKeyFPs := map[secrets.Provider]string{secrets.ProviderAnthropic: "fp-key"}

	cases := []struct {
		name   string
		bundle secrets.RunBundle
		grant  *credpool.Grant
		tier   string
		match  bool
	}{
		{"own forfait", oauth(claude), nil, "oauth-forfait(oauth:", true},
		{"org forfait", orgForfait, nil, "org(oauth:", true},
		{"pool forfait", oauth(codex), poolGrant, "pool(oauth:", true},
		{"platform forfait", platformForfait, nil, "platform(oauth:", true},
		{"forfait beside an api key", withKey(oauth(claude)), nil, "oauth-forfait(oauth:", true},
		{"tenant api key only", withKey(secrets.RunBundle{}), nil, "byok(api_key:", false},
		{"org api key only", orgKey, nil, "org(api_key:", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logGrantedCredentials(iterlog.New(iterlog.LevelInfo, &buf), "run-audit", tc.bundle, apiKeyFPs, tc.grant)
			line := buf.String()
			if !strings.Contains(line, tc.tier) {
				t.Fatalf("GRANTED line does not name %q:\n%s", tc.tier, line)
			}
			if got := re.MatchString(line); got != tc.match {
				t.Fatalf("forfait audit grep matched=%v, want %v on:\n%s", got, tc.match, line)
			}
		})
	}
}
