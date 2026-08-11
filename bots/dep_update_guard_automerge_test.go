package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestDepUpdateGuardArmAutomerge pins the one node in the fleet that can end
// with code merged into a default branch. Two properties carry all the weight:
//
//   - it never merges past a check. Which of the three forge calls it makes
//     is the forge's own answer, and arming is always tried first (on a
//     merge-queue base it is "merge when ready" — the only door a queue
//     leaves open): auto-merge while there is anything left to wait for; on
//     a refusal, an enqueue pinned to the reviewed head when the base has a
//     merge queue; a direct merge pinned to the reviewed head only when
//     there is no queue and the forge itself reported CLEAN (mergeable and
//     passing commit status), the state where it refuses to arm at all.
//   - it is fail-closed everywhere: off by default, green verdict required,
//     the gate must actually have landed green, GitHub only, and every refusal
//     carries a reason so an un-merged PR is never a mystery.
func TestDepUpdateGuardArmAutomerge(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := toolScript(t, "dep-update-guard/main.bot", "arm_automerge")

	// The REST merge endpoint merges whatever the state; it must not appear at
	// all. Asserted on each marker on its own — a conjunction with "does it
	// also mention the auto-merge mutation" is always false, and would let the
	// single most dangerous regression this bot can have sail through.
	for _, forbidden := range []string{`/merge"`, "/merge'", "/merge%s", "/merge?"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("arm_automerge must not reference a direct merge endpoint (%q)", forbidden)
		}
	}
	// The GraphQL merge is allowed only in its pinned form. Asserting that on
	// the SOURCE is worthless: the word expectedHeadOid also appears in a
	// comment, so deleting it from the mutation keeps a source-level check
	// green. The real assertion is on the query that goes over the wire, in
	// the stub below.

	type call struct {
		query string
		vars  map[string]any
	}

	// The forge's own words when a PR has nothing left to wait for.
	const cleanRefusal = "Pull request Pull request is in clean status"

	// blocked is the state where checks are still running: auto-merge is
	// exactly what it is for.
	blocked := map[string]any{
		"id": "PR_node_1", "headRefOid": "d34db33f", "baseRefName": "main",
		"autoMergeRequest": nil, "mergeQueueEntry": nil,
		"mergeable": "MERGEABLE", "mergeStateStatus": "BLOCKED",
	}
	withState := func(over map[string]any) map[string]any {
		pr := map[string]any{}
		for k, v := range blocked {
			pr[k] = v
		}
		for k, v := range over {
			pr[k] = v
		}
		return pr
	}

	// runWith drives the node against a stub forge holding `pr`. A non-empty
	// armErr makes the auto-merge mutation fail with that message, and `after`
	// (when set) is the state served on any read that follows — the flip a
	// finishing check produces mid-decision. An optional trailing true makes
	// the base branch merge-queue-protected.
	runWith := func(t *testing.T, subs map[string]string, pr map[string]any, armErr string, after map[string]any, queueProtected ...bool) (map[string]any, []call, []string) {
		t.Helper()
		hasQueue := len(queueProtected) > 0 && queueProtected[0]
		var calls []call
		var paths []string
		state := pr
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
			// mergeStateStatus is preview-gated: the real API rejects the query
			// without the media type, so a stub that ignores it would certify a
			// request production refuses.
			if strings.Contains(body.Query, "mergeStateStatus") &&
				!strings.Contains(r.Header.Get("Accept"), "merge-info-preview") {
				t.Errorf("mergeStateStatus queried without the merge-info-preview Accept header")
				w.WriteHeader(400)
				return
			}
			switch {
			case strings.Contains(body.Query, "enablePullRequestAutoMerge"):
				if armErr != "" {
					if after != nil {
						state = after
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"errors": []map[string]any{{"type": "UNPROCESSABLE", "message": armErr}},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"enablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}},
				})
			case strings.Contains(body.Query, "mergePullRequest"):
				// The mutation ITSELF must carry the pin. Sending oid in the
				// variables proves nothing — merge_now populates it whether or
				// not the query references it, so an unpinned merge (the single
				// most dangerous regression this node can have) would sail
				// through a variables-only check.
				if !strings.Contains(body.Query, "expectedHeadOid") {
					t.Errorf("merge mutation is not pinned to a head: %s", body.Query)
				}
				if !regexp.MustCompile(`expectedHeadOid:\s*\$oid`).MatchString(body.Query) {
					t.Errorf("expectedHeadOid is not bound to the oid variable: %s", body.Query)
				}
				if body.Variables["oid"] != state["headRefOid"] {
					t.Errorf("merge pinned to %v, want the reviewed head %v", body.Variables["oid"], state["headRefOid"])
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"mergePullRequest": map[string]any{"clientMutationId": nil}},
				})
			case strings.Contains(body.Query, "enqueuePullRequest"):
				// The queue door carries the same pin as the direct merge, for
				// the same reason: enqueue the audited commit or nothing.
				if !regexp.MustCompile(`expectedHeadOid:\s*\$oid`).MatchString(body.Query) {
					t.Errorf("enqueue mutation is not pinned to the oid variable: %s", body.Query)
				}
				if body.Variables["oid"] != state["headRefOid"] {
					t.Errorf("enqueue pinned to %v, want the reviewed head %v", body.Variables["oid"], state["headRefOid"])
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"enqueuePullRequest": map[string]any{"clientMutationId": nil}},
				})
			case strings.Contains(body.Query, "mergeQueue("):
				var mq map[string]any
				if hasQueue {
					mq = map[string]any{"id": "MQ_1"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"repository": map[string]any{"mergeQueue": mq}},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"repository": map[string]any{"pullRequest": state}},
				})
			}
		}))
		defer srv.Close()

		base := map[string]string{
			"{{vars.arm_automerge}}":       "True",
			"{{vars.automerge_method}}":    `"squash"`,
			"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":            `"committed"`,
			"{{input.gate_posted}}":        "True",
			"{{input.gate_state}}":         `"success"`,
			"{{input.gate_sha}}":           `"d34db33f"`,
			"{{input.audited_sha}}":        `"d34db33f"`,
			"{{input.committed_sha}}":      `""`,
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
	run := func(t *testing.T, subs map[string]string) (map[string]any, []call, []string) {
		t.Helper()
		return runWith(t, subs, blocked, "", nil)
	}

	queried := func(calls []call, needle string) bool {
		for _, c := range calls {
			if strings.Contains(c.query, needle) {
				return true
			}
		}
		return false
	}

	t.Run("checks still pending: arms auto-merge and merges nothing", func(t *testing.T) {
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
			// The whole guarantee: with a check still pending, nothing merges.
			if strings.Contains(c.query, "mergePullRequest") {
				t.Fatal("merged a PR whose checks had not landed — that bypasses CI")
			}
		}
		if !armed {
			t.Fatal("no enablePullRequestAutoMerge call was made")
		}
	})

	// The ordinary no-queue case: the audit outlives CI, so by the time the
	// bot decides, the forge has nothing left to wait for and REFUSES to arm
	// auto-merge ("clean status") — that refusal is what routes to the pinned
	// direct merge. Arming is still attempted first: on a queue-protected
	// repo the same call succeeds ("merge when ready"), and skipping it there
	// would walk straight into the queue's direct-merge rejection.
	t.Run("forge reports CLEAN: arm refused, merges pinned to the reviewed head", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{"mergeStateStatus": "CLEAN"}), cleanRefusal, nil)
		if res["armed"] != true {
			t.Fatalf("want merged, got %v", res)
		}
		if !queried(calls, "enablePullRequestAutoMerge") {
			t.Fatal("arming must be attempted first — it is what lands the queue-repo case")
		}
		if !queried(calls, "mergePullRequest") {
			t.Fatal("a PR the forge reports CLEAN must be merged, not left open forever")
		}
	})

	t.Run("arming refused as clean: re-reads, then merges", func(t *testing.T) {
		// The state can flip between the read and the arm call; the refusal is
		// the forge telling us so.
		res, calls, _ := runWith(t, nil, blocked, cleanRefusal, withState(map[string]any{"mergeStateStatus": "CLEAN"}))
		if res["armed"] != true {
			t.Fatalf("want merged after the refusal, got %v", res)
		}
		if !queried(calls, "mergePullRequest") {
			t.Fatal("refusal-to-arm left the PR unmerged")
		}
	})

	t.Run("CLEAN but conflicting: merges nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"mergeStateStatus": "CLEAN", "mergeable": "CONFLICTING"}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatal("merged a conflicting PR")
		}
		if res["armed"] != false {
			t.Errorf("want a refusal with a reason, got %v", res)
		}
	})

	// The audit takes minutes. A rebase or force-push in that window produces a
	// head no auditor has seen, and the forge does NOT hold it back: a commit
	// status blocks a merge only once a maintainer has made its context
	// required, so the gate's absence on the new head still reads as CLEAN.
	// Merging what the head says at merge time would ship exactly the
	// unreviewed dependency commit this bot exists to catch.
	t.Run("branch moved after the gate: merges nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"mergeStateStatus": "CLEAN", "headRefOid": "0ther5ha"}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatal("merged a head that was never audited")
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
		if reason, _ := res["reason"].(string); !strings.Contains(reason, "moved after the audit") {
			t.Errorf("reason = %q, want it to name the moved branch", reason)
		}
	})

	// gate_sha is the head the FORGE reported when the server posted the
	// status, not something this run vouched for. A push landing between the
	// alignment and that read carries the gate onto a commit no auditor saw —
	// and then head == gate_sha holds, so every other guard is satisfied.
	t.Run("gate landed on a foreign commit: merges nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{
			"{{input.gate_sha}}":    `"f0re1gn5"`,
			"{{input.audited_sha}}": `"d34db33f"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": "f0re1gn5"}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatal("merged a commit the gate covered but this run never audited")
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
		if reason, _ := res["reason"].(string); !strings.Contains(reason, "this run audited") {
			t.Errorf("reason = %q, want it to name the disagreement", reason)
		}
	})

	// The commit agent reports whatever `git commit` printed, which is the
	// ABBREVIATED sha. An exact compare refused the merge here — on the
	// align-and-commit path, with a message ("something else pushed") that was
	// simply false. Every other case in this file uses same-width literals, so
	// equality held by construction and the class went uncovered.
	t.Run("aligned bump: an abbreviated committed sha still merges", func(t *testing.T) {
		const full = "a11gned0c0ffee0c0ffee0c0ffee0c0ffee0c0ff"
		res, calls, _ := runWith(t, map[string]string{
			"{{input.gate_sha}}":      `"` + full + `"`,
			"{{input.audited_sha}}":   `"d34db33f"`,
			"{{input.committed_sha}}": `"a11gned0"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": full}), cleanRefusal, nil)
		if !queried(calls, "mergePullRequest") {
			t.Fatalf("refused the merge on an abbreviated sha: %v", res)
		}
		if res["armed"] != true {
			t.Fatalf("want armed, got %v", res)
		}
	})

	// A prefix shorter than git's own floor must NOT be treated as a match:
	// two different commits can share six hex digits.
	t.Run("a too-short sha is refused, not guessed", func(t *testing.T) {
		const full = "a11gne0c0ffee0c0ffee0c0ffee0c0ffee0c0fff"
		res, calls, _ := runWith(t, map[string]string{
			"{{input.gate_sha}}":      `"` + full + `"`,
			"{{input.audited_sha}}":   `"d34db33f"`,
			"{{input.committed_sha}}": `"a11gne"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": full}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatalf("merged on a 6-hex prefix: %v", res)
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
	})

	// When the bot pushed an alignment, THAT is the commit it vouches for.
	t.Run("aligned bump: merges the commit it pushed", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{
			"{{input.gate_sha}}":      `"a11gned0"`,
			"{{input.audited_sha}}":   `"d34db33f"`,
			"{{input.committed_sha}}": `"a11gned0"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": "a11gned0"}), cleanRefusal, nil)
		if !queried(calls, "mergePullRequest") {
			t.Fatal("the commit Vetty pushed and gated must be mergeable")
		}
		if res["armed"] != true {
			t.Fatalf("want merged, got %v", res)
		}
	})

	t.Run("no gated sha: merges nothing", func(t *testing.T) {
		// Without a gate there is no anchor for "the commit we audited", so the
		// merge path has nothing safe to target.
		res, calls, _ := runWith(t, map[string]string{"{{input.gate_sha}}": `""`},
			withState(map[string]any{"mergeStateStatus": "CLEAN"}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatal("merged with no audited sha to pin to")
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
	})

	t.Run("blocked and arming refused for another reason: reports it", func(t *testing.T) {
		// Only "clean status" means "nothing left to wait for". Any other
		// refusal must surface, never be retried into a merge.
		res, calls, _ := runWith(t, nil, blocked, "Base branch was modified. Review and try the merge again.", nil)
		if queried(calls, "mergePullRequest") {
			t.Fatal("merged a BLOCKED PR after a failed arm")
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
	})

	// A merge queue rejects direct merges outright, so on a queue-protected
	// base the refusal-to-arm falls back to the queue's own door — pinned to
	// the audited head exactly like the direct merge is.
	t.Run("queue repo, arming refused: enqueues pinned, never merges directly", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{"mergeStateStatus": "CLEAN"}),
			"Pull request is already in clean status", nil, true)
		if res["armed"] != true {
			t.Fatalf("want enqueued, got %v", res)
		}
		if !queried(calls, "enqueuePullRequest") {
			t.Fatal("a queue-protected PR must go through the queue door")
		}
		if queried(calls, "mergePullRequest") {
			t.Fatal("direct-merged on a queue-protected base — the queue rejects that, and bypassing it would skip the merge-group checks")
		}
		if reason, _ := res["reason"].(string); !strings.Contains(reason, "merge queue") {
			t.Errorf("reason = %q, want it to say the queue decides from here", reason)
		}
	})

	t.Run("queue repo, branch moved after the gate: enqueues nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"mergeStateStatus": "CLEAN", "headRefOid": "0ther5ha"}), cleanRefusal, nil, true)
		if queried(calls, "enqueuePullRequest") {
			t.Fatal("enqueued a head that was never audited")
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
	})

	t.Run("already in the merge queue: touches nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"mergeQueueEntry": map[string]any{"id": "MQE_1"}}), "", nil, true)
		if res["armed"] != true {
			t.Fatalf("want acknowledged as armed, got %v", res)
		}
		for _, needle := range []string{"enablePullRequestAutoMerge", "mergePullRequest", "enqueuePullRequest"} {
			if queried(calls, needle) {
				t.Errorf("called %s on a PR already queued", needle)
			}
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
		{"alignment missing from the branch", map[string]string{"{{input.verdict}}": `"hold_lost_alignment"`}, "not green"},
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
