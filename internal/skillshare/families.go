package skillshare

// Families declares every skill file name more than one bundle carries. The
// declaration records the tree as it is: a family's mirrors are the copies that
// already read alike and must keep doing so, and a variant is a copy whose
// divergence somebody chose, with the reason in the declaration rather than in
// a comment each copy repeats differently.
//
// Adding a shared skill means adding it here — Check reddens on a file name
// several bundles carry that no family covers, which is the defect this
// package closes.
//
// Two shapes of divergence recur and are worth naming once:
//
//   - A copy that differs from its source only by the `[[bundle]]` line that
//     cross-references its own owning playbook. The document is shared; the
//     self-reference cannot be. Those copies stay variants until the sharing
//     primitive can substitute a per-bundle token, which
//     docs/adr/144-shared-skills-are-bundle-dependencies.md leaves
//     deliberately undecided: `exports.skills` shares a document that is
//     shareable verbatim, and a templated skill is an authoring surface of
//     its own. Seven of the variants below are that one line and nothing
//     else.
//   - A copy whose generated region a per-bundle generator rebuilds
//     (`iterion bots regen-catalog`). It can never be a byte-mirror.
func Families() []Family {
	return []Family{
		{
			Name:   "verify-build.md",
			Source: "app-dev",
			Mirrors: []string{
				"adr-cartograph", "branch-improve-loop", "feature-dev",
				"feature-gap-fill", "instrument", "secured-renovacy",
				"whole-improve-loop",
			},
			Variants: []Variant{
				{"dep-update-guard", "adds §1c, mirror CI's exact strictness in BOTH directions: a bump must not be held for debt it did not create, and the verify sandbox is slower than CI so timeouts are raised, never lowered. No other campaign bounds a verdict that way."},
				{"e2e-coverage", "the campaign's deterministic gate replaces the improve-loop's commit gate, and the suite the run's new e2e tests live in is NOT optional — a campaign whose new tests never ran must not converge."},
				{"test-coverage", "the same campaign gate as e2e-coverage, bounded to the suite the run's new unit tests live in, which drives coverage_complete rather than the matrix rows."},
			},
		},
		{
			Name:    "forge-mr-create.md",
			Source:  "app-dev",
			Mirrors: []string{"branch-improve-loop", "feature-dev", "instrument", "whole-improve-loop"},
			Variants: []Variant{
				{"docs-refresh", "adds §6 amend mode: when the run was launched ON a pull request, it lands its commits in that PR and comments instead of opening a second one."},
				{"product-docs", "adds §4b draft mode and its own iterion/product-docs branch prefix: functional documentation is marked ready by the people who own the product, never by the bot."},
			},
		},
		{
			Name:    "iterion-board.md",
			Source:  "rgaa-audit",
			Mirrors: []string{"sec-audit-deps", "sec-audit-source", "supply-shield", "supply-shield-cve"},
			Variants: []Variant{
				{"evolve", "names Evoly's own emit-node capability grant (read, create, assign, label — it does not close issues) and the backlog/horizon:next defaults its tickets carry."},
				{"whats-next", "carries the Nexie boundary paragraph: the conversational node holds board.read ONLY and expresses every write through the assistant_actions contract, so the mutating tools below it are not an escape hatch."},
			},
		},
		{
			Name:   "code-review-invariants.md",
			Source: "app-dev",
			Variants: []Variant{
				{"branch-improve-loop", "scoped to a branch's accumulated diff rather than one feature's, which changes what counts as an invariant the pass may not break."},
				{"feature-dev", "scoped to the feature under construction, where a new seam is expected rather than suspect."},
				{"instrument", "the longest of the four: instrumentation adds observability code to a tree it does not own, so it enumerates the invariants of the host tree as well as its own."},
			},
		},
		{
			Name:    "malware-signals.md",
			Source:  "sec-audit-deps",
			Mirrors: []string{"supply-shield", "supply-shield-cve"},
		},
		{
			Name:   "lang-go.md",
			Source: "sec-audit-deps",
			Variants: []Variant{
				{"supply-shield", "one line, the `[[supply-shield]]` self-reference to its own orchestrating playbook."},
				{"supply-shield-cve", "one line, the `[[supply-shield-cve]]` self-reference to its own orchestrating playbook."},
				{"sec-audit-source", "rewritten for source auditing: the Go signals of a codebase under review, not of a dependency set."},
				{"instrument", "rewritten for instrumentation: where to add tracing and metrics in a Go tree, not what to audit in it."},
			},
		},
		{
			Name:   "lang-js.md",
			Source: "sec-audit-deps",
			Variants: []Variant{
				{"supply-shield", "the js-x-ray AST malware analysis section — the always-on jsxray.json runner is this bot's high-value npm signal and the CVE bot's is not."},
				{"supply-shield-cve", "the npm CVE section instead: vuln-db-known from the trivy + osv floor, with the js-x-ray malware analysis named as the companion bot's job."},
				{"sec-audit-source", "rewritten for source auditing of a JS codebase rather than of its dependency set."},
				{"instrument", "rewritten for instrumentation: the JS seams tracing and metrics attach to."},
			},
		},
		{
			Name:   "lang-generic.md",
			Source: "sec-audit-deps",
			Variants: []Variant{
				{"supply-shield", "one line, the `[[supply-shield]]` self-reference to its own orchestrating playbook."},
				{"supply-shield-cve", "one line, the `[[supply-shield-cve]]` self-reference to its own orchestrating playbook."},
				{"sec-audit-source", "rewritten for source auditing: the language-agnostic signals of a codebase under review."},
			},
		},
		{
			Name:   "lang-py.md",
			Source: "sec-audit-deps",
			Variants: []Variant{
				{"supply-shield", "one line, the `[[supply-shield]]` self-reference to its own orchestrating playbook."},
				{"supply-shield-cve", "one line, the `[[supply-shield-cve]]` self-reference to its own orchestrating playbook."},
			},
		},
		{
			Name:   "lang-python.md",
			Source: "sec-audit-source",
			Variants: []Variant{
				{"instrument", "a different document that collided on a file name: instrumenting a Python tree, two and a half times the length, with no auditing content to share."},
			},
		},
		{
			Name:   "package-cache.md",
			Source: "sec-audit-deps",
			Variants: []Variant{
				{"supply-shield", "the cache discipline of a malware run, which reads a bare checkout where the dependency audit installs a tree."},
				{"supply-shield-cve", "the same malware-run discipline as supply-shield, plus its own `[[supply-shield-cve]]` self-reference."},
			},
		},
		{
			Name:    "package-managers.md",
			Source:  "dep-update-guard",
			Mirrors: []string{"secured-renovacy"},
		},
		{
			Name:    "plan-contract.md",
			Source:  "assessment",
			Mirrors: []string{"modernize"},
		},
		{
			Name:    "upgrade-archetypes.md",
			Source:  "assessment",
			Mirrors: []string{"modernize"},
		},
		{
			Name:   "verify-tests.md",
			Source: "test-coverage",
			Variants: []Variant{
				{"e2e-coverage", "an e2e suite's verdict is read differently from a unit suite's: a docker service this host cannot run leaves matrix rows uncovered with that reason, rather than failing the run."},
			},
		},
		{
			Name:   "session-continuity.md",
			Source: "whats-next",
			Variants: []Variant{
				{"evolve", "Evoly's memory scopes and write cadence differ from Nexie's: it reads the tree a campaign left behind, where whats-next writes the operator's own session trail."},
			},
		},
		{
			Name:   "iterion-bot-catalog.md",
			Source: "whats-next",
			Variants: []Variant{
				{"issue-triage", "its generated region is rebuilt per bundle by `iterion bots regen-catalog`, so the two copies cannot be byte-mirrors; the hand-authored preamble differs too, triage reading the catalogue to route a ticket rather than to answer an operator."},
			},
		},
		{
			Name:    "iterion-label-vocabulary.md",
			Source:  "whats-next",
			Mirrors: []string{"issue-triage"},
		},
		{
			Name:   "diff-scope.md",
			Source: "supply-shield",
			Variants: []Variant{
				{"supply-shield-cve", "one line, the `[[supply-shield-cve]]` self-reference to its own orchestrating playbook."},
			},
		},
		{
			Name:   "forge-report.md",
			Source: "supply-shield",
			Variants: []Variant{
				{"supply-shield-cve", "the report's own heading and verdict vocabulary: this bot reports CVEs from `[[cve-scanning]]` where supply-shield reports malware from `[[js-xray]]`."},
			},
		},
	}
}
