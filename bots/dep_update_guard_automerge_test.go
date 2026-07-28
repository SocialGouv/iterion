package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDepUpdateGuardArmAutomerge pins the one node in the fleet that can end
// with code merged into a default branch. Two properties carry all the weight:
//
//   - it NEVER merges. The only forge call it may make is
//     enablePullRequestAutoMerge, which hands the decision to the repo's own
//     required checks. A direct merge call would bypass CI — the opposite of
//     what this bot exists to guarantee.
//   - it is fail-closed everywhere: off by default, green verdict required,
//     the gate must actually have landed green, GitHub only, and every refusal
//     carries a reason so an un-merged PR is never a mystery.
func TestDepUpdateGuardArmAutomerge(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := toolScript(t, "dep-update-guard/main.bot", "arm_automerge")

	// The source must not even contain a way to merge outright: the guarantee
	// should be readable, not merely untested. Asserted on each forbidden
	// marker on its own — a conjunction with "does it also mention the
	// auto-merge mutation" is always false, since that mutation is the node's
	// only forge call, and would let the single most dangerous regression
	// this bot can have sail through.
	for _, forbidden := range []string{"mergePullRequest", `/merge"`, "/merge'", "/merge%s", "/merge?"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("arm_automerge must not reference a direct merge endpoint (%q)", forbidden)
		}
	}

	type call struct {
		query string
		vars  map[string]any
	}

	run := func(t *testing.T, subs map[string]string) (map[string]any, []call, []string) {
		t.Helper()
		var calls []call
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.Method+" "+r.URL.Path)
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(raw, &body)
			calls = append(calls, call{query: body.Query, vars: body.Variables})
			// The stub answered success for ANY body, so a query corrupted into
			// invalid GraphQL passed CI while being rejected by the real API —
			// the feature was green here and dead in production. Reject
			// anything the API would.
			if err := assertValidGraphQL(body.Query); err != "" {
				t.Errorf("invalid GraphQL sent: %s\nquery: %s", err, body.Query)
				w.WriteHeader(400)
				return
			}
			if strings.Contains(body.Query, "enablePullRequestAutoMerge") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"enablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"id": "PR_node_1", "autoMergeRequest": nil},
				}},
			})
		}))
		defer srv.Close()

		base := map[string]string{
			"{{vars.arm_automerge}}":       "True",
			"{{vars.automerge_method}}":    `"squash"`,
			"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":            `"committed"`,
			"{{input.gate_posted}}":        "True",
			"{{input.gate_state}}":         `"success"`,
			"{{vars.gate_enabled}}":        "True",
			"{{secrets.forge_token.path}}": `"ghs_test"`,
		}
		for k, v := range subs {
			base[k] = v
		}
		body := script
		for ref, val := range base {
			body = strings.ReplaceAll(body, ref, val)
		}
		// Point the GraphQL endpoint at the stub without touching the logic.
		body = strings.ReplaceAll(body, `"https://api.github.com/graphql"`, `"`+srv.URL+`/graphql"`)
		if strings.Contains(body, "{{") {
			t.Fatalf("unsubstituted ref: %s", firstRef(body))
		}

		f, err := os.CreateTemp(t.TempDir(), "arm-*.py")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command("python3", f.Name()).Output()
		if err != nil {
			t.Fatalf("arm_automerge failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not automerge_output JSON: %v (%q)", err, out)
		}
		return res, calls, paths
	}

	t.Run("green verdict + green gate arms via the auto-merge mutation", func(t *testing.T) {
		res, calls, _ := run(t, nil)
		if res["armed"] != true {
			t.Fatalf("want armed, got %v", res)
		}
		var armed bool
		for _, c := range calls {
			if strings.Contains(c.query, "enablePullRequestAutoMerge") {
				armed = true
				if c.vars["method"] != "SQUASH" {
					t.Errorf("merge method = %v, want SQUASH", c.vars["method"])
				}
			}
			// The whole guarantee: nothing may merge the PR outright.
			if strings.Contains(c.query, "mergePullRequest") {
				t.Fatal("arm_automerge must never call mergePullRequest — that bypasses the checks")
			}
		}
		if !armed {
			t.Fatal("no enablePullRequestAutoMerge call was made")
		}
	})

	for _, tc := range []struct {
		name string
		subs map[string]string
		want string
	}{
		{"off by default", map[string]string{"{{vars.arm_automerge}}": "False"}, "off"},
		{"held bump", map[string]string{"{{input.verdict}}": `"hold_security"`}, "not green"},
		{"unstable build", map[string]string{"{{input.verdict}}": `"hold_unstable"`}, "not green"},
		{"pending human decision", map[string]string{"{{input.verdict}}": `"needs_decision"`}, "not green"},
		{"unknown verdict", map[string]string{"{{input.verdict}}": `"probably_fine"`}, "not green"},
		// A verdict whose status never reached the PR must not merge it: the
		// gate is what the repo actually gates on.
		{"gate never landed", map[string]string{"{{input.gate_posted}}": "False"}, "did not land"},
		{"gate red", map[string]string{"{{input.gate_state}}": `"failure"`}, "merge gate is failure"},
		{"non-github forge", map[string]string{"{{input.pr_url}}": `"https://gitlab.com/a/b/-/merge_requests/3"`}, "GitHub-only"},
		{"no token", map[string]string{"{{secrets.forge_token.path}}": `""`}, "no forge_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, calls, _ := run(t, tc.subs)
			if res["armed"] != false {
				t.Fatalf("must not arm: %v", res)
			}
			reason, _ := res["reason"].(string)
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
			if len(calls) != 0 {
				t.Errorf("must not touch the forge at all, made %d call(s)", len(calls))
			}
		})
	}

	t.Run("a clean bump arms too", func(t *testing.T) {
		// "clean" (safe bump, nothing to align) is as green as "committed" —
		// it must not need a code change to be mergeable.
		res, _, _ := run(t, map[string]string{"{{input.verdict}}": `"clean"`})
		if res["armed"] != true {
			t.Fatalf("a clean bump with a green gate arms too: %v", res)
		}
	})
}

// assertValidGraphQL catches the corruption class that got past the original
// stub: a sigil swap that also rewrote GraphQL's own separators, producing a
// query the real API rejects while the test answered success to anything. It
// is not a parser — it checks the invariants a careless string substitution
// breaks. Returns "" when the query looks well-formed.
func assertValidGraphQL(q string) string {
	if strings.TrimSpace(q) == "" {
		return "empty query"
	}
	// Every declared variable is `$name: Type` and every use `$name` — never
	// `$name$`, which is exactly what swapping colons produces.
	for _, frag := range strings.Split(q, "$")[1:] {
		name := frag
		for i, r := range frag {
			isNameRune := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isNameRune {
				name = frag[:i]
				break
			}
		}
		if name == "" {
			return "empty variable name after $"
		}
		if strings.HasPrefix(strings.TrimPrefix(frag, name), "$") {
			return "variable $" + name + " is followed by another $ (a separator was rewritten)"
		}
	}
	// Fields and arguments are separated by a colon; if the swap ate them,
	// none are left where the query needs them.
	if !strings.Contains(q, ": ") {
		return "no `: ` separator left in the query"
	}
	if strings.Count(q, "{") != strings.Count(q, "}") {
		return "unbalanced braces"
	}
	return ""
}
