package cloudpublisher

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// audienceWithholdings remembers the keys a workload audience kept out of a
// run's walk (secrets.ApiKey.Bots — see docs/byok.md).
//
// It exists because the refusal leaves no other trace. Resolve simply yields
// the next key, or none, and a caller reading an empty slot cannot tell "this
// team has no key for that provider" from "this team has one and I refused
// it". The consequences of that silence are not hypothetical: the wire is then
// filled by the org tier, the pool, the platform key, or the runner's ambient
// env, so narrowing an audience does not stop a run — it REDIRECTS which
// account pays for it, and possibly which vendor answers.
//
// A recorder, never a decision: Resolve has already refused by the time this
// is called.
type audienceWithholdings struct {
	botID string
	seen  map[string]bool
	rows  []secrets.ApiKey
}

func newAudienceWithholdings(botID string) *audienceWithholdings {
	return &audienceWithholdings{botID: botID, seen: map[string]bool{}}
}

// note records one withheld key. Deduplicated by id: every tier re-resolves
// the same providers and each restore walks them again, so the same key is
// offered and refused several times per launch — four log lines would read as
// four different keys.
func (a *audienceWithholdings) note(k secrets.ApiKey) {
	if a == nil || k.ID == "" || a.seen[k.ID] {
		return
	}
	a.seen[k.ID] = true
	a.rows = append(a.rows, k)
}

// holds reports whether this key was withheld for its audience — the question
// the pin diagnostic asks, since a pin that Resolve never returned is
// otherwise indistinguishable from a pin that named a deleted key.
func (a *audienceWithholdings) holds(keyID string) bool {
	return a != nil && keyID != "" && a.seen[keyID]
}

func (a *audienceWithholdings) any() bool { return a != nil && len(a.rows) > 0 }

// warn states each withholding once, naming the run, the key, its audience and
// the bot that was refused — everything needed to answer "why did this run not
// use our key" without reading the database.
func (a *audienceWithholdings) warn(p *Publisher, runID string) {
	if !a.any() || p == nil || p.logger == nil {
		return
	}
	bot := a.botID
	if bot == "" {
		bot = "(none — this launch named no bot)"
	}
	for _, k := range a.rows {
		p.logger.Warn(
			"cloudpublisher: api key %s (%s, provider=%s) WITHHELD from run %s — its workload audience [%s] does not name bot %s; the walk falls through to the next key or tier, which may be another account or another vendor",
			k.ID, k.Name, k.Provider, runID, strings.Join(k.Bots, " "), bot)
	}
}
