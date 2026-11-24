package permission

import "testing"

// The claw backend advertises an MCP tool as `mcp_<server>_<tool>` while the
// docs spell rules `mcp__<server>__*`, and dispatch resolves either spelling
// of a call to the same tool. A rule and a call that name the SAME MCP tool
// must therefore reach one verdict per mode and list, whichever spelling each
// side uses — the spelling the model emits may not pick the verdict. The
// mixed call spelling (`mcp__fs_read_file`) matches no literal form and
// exercises the collapsed key alone.
//
// Every row is judged through the public surface a consumer reads: Evaluate,
// CanAskAbout, HasExplicitDeny, LeavesTool and RuleCovers.
func TestAnMCPRuleMeetsTheCallWhicheverSpellingEachSideUses(t *testing.T) {
	ruleSpellings := map[string]string{
		"fqn_glob":   "mcp__fs__*",
		"claw_glob":  "mcp_fs_*",
		"fqn_exact":  "mcp__fs__read_file",
		"claw_exact": "mcp_fs_read_file",
	}
	callSpellings := []string{"mcp__fs__read_file", "mcp_fs_read_file", "mcp__fs_read_file"}

	for _, mode := range []Mode{ModeAsk, ModeDeny} {
		for _, ruleRaw := range ruleSpellings {
			for _, list := range []string{"deny", "ask", "allow", "grant"} {
				p := NewPolicyForTest(t, mode, list, ruleRaw)
				for _, call := range callSpellings {
					wantVerdict, wantRule := expectedForList(mode, list, ruleRaw)
					got, rule := p.Evaluate(call, map[string]any{})
					if got != wantVerdict || rule != wantRule {
						t.Errorf("%s mode, %s list, rule %q, call %q = (%v, %q), want (%v, %q)",
							mode, list, ruleRaw, call, got, rule, wantVerdict, wantRule)
					}
					if gotHasDeny := p.HasExplicitDeny(call, map[string]any{}); gotHasDeny != (list == "deny") {
						t.Errorf("%s mode, %s list, rule %q, call %q: HasExplicitDeny = %v",
							mode, list, ruleRaw, call, gotHasDeny)
					}
					if gotLeaves := p.LeavesTool(call); gotLeaves != (list != "deny") {
						t.Errorf("%s mode, %s list, rule %q, call %q: LeavesTool = %v",
							mode, list, ruleRaw, call, gotLeaves)
					}
					if !RuleCovers(ruleRaw, call) {
						t.Errorf("%s mode, rule %q, call %q: RuleCovers = false, want true",
							mode, ruleRaw, call)
					}
					// CanAskAbout answers the ask rules alone; in ask mode every
					// unmatched call is a pause, in deny mode only an ask rule
					// names a pause.
					wantCanAsk := mode == ModeAsk || list == "ask"
					if gotCanAsk := p.CanAskAbout(call); gotCanAsk != wantCanAsk {
						t.Errorf("%s mode, %s list, rule %q, call %q: CanAskAbout = %v, want %v",
							mode, list, ruleRaw, call, gotCanAsk, wantCanAsk)
					}
				}
			}
		}
	}
}

// NewPolicyForTest builds a policy holding exactly one rule, in the named
// list. Grants are engine-owned and added after construction, the way the
// executor does.
func NewPolicyForTest(t *testing.T, mode Mode, list, raw string) *Policy {
	t.Helper()
	var allow, ask, deny []string
	switch list {
	case "allow":
		allow = []string{raw}
	case "ask":
		ask = []string{raw}
	case "deny":
		deny = []string{raw}
	}
	p, err := NewPolicy(mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("NewPolicy(%s, %s %q): %v", mode, list, raw, err)
	}
	if list == "grant" {
		p.AddGrantRule(raw)
	}
	return p
}

// expectedForList is the one verdict a bare rule in the named list must
// produce, in the named mode. The rule string travels back so a refusal
// names the rule that caused it.
func expectedForList(mode Mode, list, ruleRaw string) (Decision, string) {
	switch list {
	case "deny":
		return Deny, ruleRaw
	case "ask":
		return Ask, ruleRaw
	case "allow", "grant":
		return Allow, ruleRaw
	}
	if mode == ModeDeny {
		return Deny, ""
	}
	return Ask, ""
}

// A bare exact ask is the one shape a literal allow may supersede, and the
// supersession must hold across spellings: an operator's approval written
// against the advertised spelling retires the pause for the FQN-spelled
// retry too, on the same complete argument. The literal scope is the k=v
// candidate the gate opens an MCP call by (summarize's generic join — the
// gate opens by path for no MCP tool). A different argument still asks.
func TestABareExactAskYieldsToALiteralAllowOnEitherSpelling(t *testing.T) {
	for _, tc := range []struct{ name, allowRule, askRule string }{
		{"claw allow over fqn ask", "mcp_fs_read_file(path=/etc/motd)", "mcp__fs__read_file"},
		{"fqn allow over claw ask", "mcp__fs__read_file(path=/etc/motd)", "mcp_fs_read_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPolicy(t, ModeAsk, []string{tc.allowRule}, []string{tc.askRule}, nil)
			for _, call := range []string{"mcp__fs__read_file", "mcp_fs_read_file", "mcp__fs_read_file"} {
				got, rule := p.Evaluate(call, map[string]any{"path": "/etc/motd"})
				if got != Allow || rule != tc.allowRule {
					t.Errorf("allowed argument, call %q = (%v, %q), want (allow, %q)", call, got, rule, tc.allowRule)
				}
				got, rule = p.Evaluate(call, map[string]any{"path": "/etc/shadow"})
				if got != Ask || rule != tc.askRule {
					t.Errorf("other argument, call %q = (%v, %q), want (ask, %q)", call, got, rule, tc.askRule)
				}
			}
		})
	}
}

// A tree deny over an MCP server binds the search that WALKS the tree —
// rooted above it, its text never meeting the scope — and the single read
// inside it, whichever spelling each side uses (#3086's containment, the
// docs' `deny: ["mcp__fs__*(/run/iterion/secrets/**)"]` reading).
func TestAnMCPTreeDenyBindsTheSearchThatWalksItWhicheverSpelling(t *testing.T) {
	for _, mode := range []Mode{ModeAsk, ModeDeny} {
		for _, ruleRaw := range []string{"mcp__fs__*(/run/iterion/secrets/**)", "mcp_fs_*(/run/iterion/secrets/**)"} {
			p := mustPolicy(t, mode, nil, nil, []string{ruleRaw})
			for _, call := range []string{"mcp__fs__search_files", "mcp_fs_search_files", "mcp__fs_read_file", "mcp_fs_read_file"} {
				got, rule := p.Evaluate(call, map[string]any{"path": "/run/iterion/secrets/token", "pattern": "*"})
				if got != Deny || rule != ruleRaw {
					t.Errorf("%s mode, rule %q, secret read %q = (%v, %q), want (deny, %q)",
						mode, ruleRaw, call, got, rule, ruleRaw)
				}
			}
			// A search rooted ABOVE the tree walks it whole.
			for _, call := range []string{"mcp_fs_search_files", "mcp__fs__search_files"} {
				got, rule := p.Evaluate(call, map[string]any{"path": "/run/iterion", "pattern": "*"})
				if got != Deny || rule != ruleRaw {
					t.Errorf("%s mode, rule %q, search above the tree %q = (%v, %q), want (deny, %q)",
						mode, ruleRaw, call, got, rule, ruleRaw)
				}
			}
			// A call that names no denied path keeps the mode default — the
			// rule quoted is "" exactly when no rule matched.
			for _, call := range []string{"mcp_fs_read_file", "mcp__fs__read_file"} {
				got, rule := p.Evaluate(call, map[string]any{"path": "/work/notes.md"})
				wantVerdict, wantRule := expectedForList(mode, "none", "")
				if got != wantVerdict || rule != wantRule {
					t.Errorf("%s mode, rule %q, ordinary read %q = (%v, %q), want (%v, %q)",
						mode, ruleRaw, call, got, rule, wantVerdict, wantRule)
				}
			}
		}
	}
}

// The collapsed key carries no server/tool boundary: `mcp__github__*` also
// bounds a server named `github_admin`, whose FQN spelling collapses to the
// same key — the documented limit an author covers by naming the longer
// server's rule explicitly (deny wins), a remedy that also bounds the
// shorter server's tools spelled with the longer server's suffix. The
// collapse reaches exact names and operator grants as well as globs. The
// key is still anchored on the server prefix — another server's name stays
// outside it — and the `mcp_`/`mcp.` prefix is the whole MCP recognition, so
// claw's status-only built-in `mcp_auth` is read through it like an MCP
// call. A claw-spelled deny keeps precedence over a catch-all allow
// whichever spelling the model emits.
func TestTheCollapsedKeyCarriesNoServerBoundary(t *testing.T) {
	p := mustPolicy(t, ModeAsk, nil, nil, []string{"mcp__github__*"})
	if got, rule := p.Evaluate("mcp_git_x", map[string]any{}); got != Ask || rule != "" {
		t.Errorf("mcp_git_x = (%v, %q), want (ask, \"\") — the key is anchored on mcp_github_", got, rule)
	}
	if got, rule := p.Evaluate("mcp__github__list_issues", map[string]any{}); got != Deny || rule != "mcp__github__*" {
		t.Errorf("mcp__github__list_issues = (%v, %q), want (deny, mcp__github__*)", got, rule)
	}
	// The boundary limit: a server named github_admin is inside the glob,
	// both spellings of it. Deliberate — pinned so the docs' limit is the
	// code's behaviour.
	for _, call := range []string{"mcp_github_admin_x", "mcp__github__admin_x"} {
		if got, rule := p.Evaluate(call, map[string]any{}); got != Deny || rule != "mcp__github__*" {
			t.Errorf("%s = (%v, %q), want (deny, mcp__github__*) — the key carries no server boundary", call, got, rule)
		}
	}
	// The prefix is the whole MCP recognition: claw's built-in mcp_auth
	// collapses to mcp_auth, outside mcp_github_*, yet inside an mcp__*
	// rule, which binds it like any MCP call.
	if got, rule := p.Evaluate("mcp_auth", map[string]any{}); got != Ask || rule != "" {
		t.Errorf("mcp_auth = (%v, %q), want (ask, \"\") — outside the github key", got, rule)
	}
	pAll := mustPolicy(t, ModeAsk, nil, nil, []string{"mcp__*"})
	if got, rule := pAll.Evaluate("mcp_auth", map[string]any{}); got != Deny || rule != "mcp__*" {
		t.Errorf("mcp_auth under deny mcp__* = (%v, %q), want (deny, mcp__*) — the mcp_ prefix makes it an MCP name", got, rule)
	}
	if !RuleCovers("mcp__*", "mcp_auth") {
		t.Errorf("RuleCovers(mcp__*, mcp_auth) = false, want true — the coverage matcher reads the same key")
	}
	// The registry's dotted qualified spelling of the SAME server collapses
	// onto the same key (and another server's dotted spelling — mcp.fs.list
	// — stays outside the glob, like mcp_git_x above).
	if got, rule := p.Evaluate("mcp.github.list", map[string]any{}); got != Deny || rule != "mcp__github__*" {
		t.Errorf("mcp.github.list = (%v, %q), want (deny, mcp__github__*)", got, rule)
	}
	if got, rule := p.Evaluate("mcp.fs.list", map[string]any{}); got != Ask || rule != "" {
		t.Errorf("mcp.fs.list = (%v, %q), want (ask, \"\") — another server's dotted spelling", got, rule)
	}

	// A claw-spelled deny is not dodged by an FQN-emitted call, not even
	// under a catch-all allow: deny wins before allow, and the exact rule
	// meets the call on the collapsed key.
	p2 := mustPolicy(t, ModeAsk, []string{"*"}, nil, []string{"mcp_fs_delete_file"})
	if got, rule := p2.Evaluate("mcp__fs__delete_file", map[string]any{}); got != Deny || rule != "mcp_fs_delete_file" {
		t.Errorf("fqn call under allow * = (%v, %q), want (deny, mcp_fs_delete_file) — the spelling the model emits picks no verdict", got, rule)
	}
	if got, rule := p2.Evaluate("mcp__fs__delete", map[string]any{}); got != Allow || rule != "*" {
		t.Errorf("mcp__fs__delete = (%v, %q), want (allow, *) — the catch-all keeps reaching the tools no deny names", got, rule)
	}

	// The documented remedy — a deny on the longer server's glob — also
	// refuses the shorter server's own tools spelled with that suffix.
	p3 := mustPolicy(t, ModeAsk, []string{"mcp__fs__*"}, nil, []string{"mcp__fs_more__*"})
	if got, rule := p3.Evaluate("mcp__fs__more_info", map[string]any{}); got != Deny || rule != "mcp__fs_more__*" {
		t.Errorf("mcp__fs__more_info = (%v, %q), want (deny, mcp__fs_more__*) — the remedy over-denies fs's more_* tools", got, rule)
	}

	// An operator grant carries the same collapse: approving mcp__a__b_c
	// also approves mcp__a_b__c, both collapsing to mcp_a_b_c.
	p4 := mustPolicy(t, ModeAsk, nil, nil, nil)
	grant := GrantRuleFor("mcp__a__b_c", map[string]any{"q": "1"}, false)
	p4.AddGrantRule(grant)
	if got, rule := p4.Evaluate("mcp__a_b__c", map[string]any{"q": "1"}); got != Allow || rule != grant {
		t.Errorf("mcp__a_b__c under grant %q = (%v, %q), want (allow, %q) — a grant binds every name collapsing to its key", grant, got, rule, grant)
	}
	// So does an exact allow.
	p5 := mustPolicy(t, ModeDeny, []string{"mcp__fs__read_file"}, nil, nil)
	if got, rule := p5.Evaluate("mcp__fs_read__file", map[string]any{}); got != Allow || rule != "mcp__fs__read_file" {
		t.Errorf("mcp__fs_read__file = (%v, %q), want (allow, mcp__fs__read_file) — an exact name collapses too", got, rule)
	}
}
