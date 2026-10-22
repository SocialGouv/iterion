package cloudpublisher

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// audienceWithholdings remembers the keys an audience kept out of a run's
// walk (secrets.ApiKey.Bots and .Repos — see docs/byok.md).
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
// is called. The CAUSE is recorded beside the key, as Resolve reported it: a
// line that blamed the bot for a repository mismatch would send the operator
// to edit the wrong list.
type audienceWithholdings struct {
	botID string
	repo  string
	seen  map[string]bool
	rows  []withheldKey
}

type withheldKey struct {
	key secrets.ApiKey
	why secrets.AudienceRefusal
}

func newAudienceWithholdings(botID, repo string) *audienceWithholdings {
	return &audienceWithholdings{botID: botID, repo: repo, seen: map[string]bool{}}
}

// note records one withheld key with the audience that refused it. It is
// the onWithheld of every Resolve call in the walk. Deduplicated by id:
// every tier re-resolves the same providers and each restore walks them
// again, so the same key is offered and refused several times per launch —
// four log lines would read as four different keys.
func (a *audienceWithholdings) note(k secrets.ApiKey, why secrets.AudienceRefusal) {
	if a == nil || k.ID == "" || a.seen[k.ID] {
		return
	}
	a.seen[k.ID] = true
	a.rows = append(a.rows, withheldKey{key: k, why: why})
}

// holds reports whether this key was withheld for its audience — the question
// the pin diagnostic asks, since a pin that Resolve never returned is
// otherwise indistinguishable from a pin that named a deleted key.
func (a *audienceWithholdings) holds(keyID string) bool {
	return a != nil && keyID != "" && a.seen[keyID]
}

// cause spells out which audience refused the key, for a line that names
// it: "its workload audience [a b] does not name bot x", "its repository
// audience [o/r] does not name repository o/s" — both, joined, when both
// refused. "" for a key this recorder never saw.
func (a *audienceWithholdings) cause(keyID string) string {
	if a == nil {
		return ""
	}
	for _, row := range a.rows {
		if row.key.ID == keyID {
			return a.describe(row)
		}
	}
	return ""
}

func (a *audienceWithholdings) describe(row withheldKey) string {
	var parts []string
	if row.why.Bot {
		bot := a.botID
		if bot == "" {
			bot = "(none — this launch named no bot)"
		}
		parts = append(parts, "its workload audience ["+strings.Join(row.key.Bots, " ")+"] does not name bot "+bot)
	}
	if row.why.Repo {
		parts = append(parts, "its repository audience ["+strings.Join(row.key.Repos, " ")+"] does not name "+describeLaunchRepo(a.repo))
	}
	return strings.Join(parts, " and ")
}

// describeLaunchRepo names the repository a launch serves, in the words a
// withholding is explained with; a launch that names none says so, since
// that is the cause a restricted key is withheld for on such a lane.
func describeLaunchRepo(repo string) string {
	if repo == "" {
		return "a repository (this launch names none)"
	}
	return "repository " + repo
}

func (a *audienceWithholdings) any() bool { return a != nil && len(a.rows) > 0 }

// warn states each withholding once, naming the run, the key, the audience
// that refused and what it refused — everything needed to answer "why did
// this run not use our key" without reading the database.
func (a *audienceWithholdings) warn(p *Publisher, runID string) {
	if !a.any() || p == nil || p.logger == nil {
		return
	}
	for _, row := range a.rows {
		p.logger.Warn(
			"cloudpublisher: api key %s (%s, provider=%s) WITHHELD from run %s — %s; the walk falls through to the next key or tier, which may be another account or another vendor",
			row.key.ID, row.key.Name, row.key.Provider, runID, a.describe(row))
	}
}
