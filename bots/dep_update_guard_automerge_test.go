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
	// Mutation: change/remove the declaration's true default. Old node inputs
	// may omit the var, but every NEW launch must respect the other actor.
	t.Run("new launches respect human arming by default", func(t *testing.T) {
		v := compilePlanPhaseBot(t, "dep-update-guard").Vars["respect_human_arming"]
		if v == nil || !v.HasDefault || v.Default != true {
			t.Fatalf("respect_human_arming must default to true, got %+v", v)
		}
	})

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
	const botLogin = "acme-vetty[bot]"

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
			// Ordered before the arming case deliberately, even though the two
			// names do not collide as substrings ("disable" carries no
			// "enable"): the reader should not have to verify that to know
			// which branch answers.
			case strings.Contains(body.Query, "disablePullRequestAutoMerge"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"disablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}},
				})
			case strings.Contains(body.Query, "enablePullRequestAutoMerge"):
				// The ARMING carries the same pin as the merge and the enqueue,
				// and for a sharper reason: an arming outlives this run. The
				// forge keeps waiting for the checks, and a push landing
				// afterwards moves the head under it — GitHub only cancels an
				// arming when the pusher LACKS write, which a dependency bot's
				// App does not. Unpinned, the commit that finally merges is
				// whichever one arrived last.
				if !regexp.MustCompile(`expectedHeadOid:\s*\$oid`).MatchString(body.Query) {
					t.Errorf("arming mutation is not pinned to the oid variable: %s", body.Query)
				}
				if body.Variables["oid"] != state["headRefOid"] {
					t.Errorf("arming pinned to %v, want the reviewed head %v", body.Variables["oid"], state["headRefOid"])
				}
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
			case strings.Contains(body.Query, "dequeuePullRequest"):
				// The dequeue door takes the pull request as `id`, NOT
				// `pullRequestId` — the API rejects the latter (unlike
				// enqueue, which takes both that and expectedHeadOid). A
				// stub answering either shape would certify a mutation
				// production refuses, so pin it exactly like the head pins
				// above.
				if !regexp.MustCompile(`input:\s*\{\s*id:\s*\$id`).MatchString(body.Query) {
					t.Errorf("dequeue mutation does not pass the pull request as id: %s", body.Query)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"dequeuePullRequest": map[string]any{"clientMutationId": nil}},
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
				// D1 mutation: remove enabledBy { login } (or viewer { login })
				// from the QUERY, leaving the explanatory comments intact. A
				// stub that returns unqueried identity would hide that regression.
				for _, field := range []string{
					`autoMergeRequest\s*\{[^}]*enabledBy\s*\{\s*login\s*\}`,
					`viewer\s*\{\s*login\s*\}`,
				} {
					if !regexp.MustCompile(field).MatchString(body.Query) {
						t.Errorf("PR read does not request arming ownership (%s): %s", field, body.Query)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"viewer":     map[string]any{"login": botLogin},
						"repository": map[string]any{"pullRequest": state},
					},
				})
			}
		}))
		defer srv.Close()

		base := map[string]string{
			"{{vars.arm_automerge}}": "True",
			// Legacy node input: missing refs render as null (the Python shim
			// maps it to None). W1-W3 override this with the new-launch default.
			"{{vars.respect_human_arming}}": "null",
			"{{vars.automerge_method}}":     `"squash"`,
			"{{input.pr_url}}":              `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":             `"committed"`,
			"{{input.gate_posted}}":         "True",
			"{{input.gate_state}}":          `"success"`,
			"{{input.gate_sha}}":            `"d34db33f"`,
			"{{input.audited_sha}}":         `"d34db33f"`,
			"{{input.committed_sha}}":       `""`,
			"{{vars.gate_enabled}}":         "True",
			"{{secrets.forge_token.path}}":  `"ghs_test"`,
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

	armedBy := func(login string) map[string]any {
		return withState(map[string]any{"autoMergeRequest": map[string]any{
			"enabledAt": "2026-09-22T09:00:00Z", "enabledBy": map[string]any{"login": login},
		}})
	}
	mutations := func(calls []call) []string {
		var names []string
		for _, c := range calls {
			for _, name := range []string{"disablePullRequestAutoMerge", "enablePullRequestAutoMerge", "dequeuePullRequest", "enqueuePullRequest", "mergePullRequest"} {
				if strings.Contains(c.query, name) {
					names = append(names, name)
				}
			}
		}
		return names
	}
	// D1/log mutation: replace state_note["disarmed"] = login with an unknown
	// owner (or omit the owner from the preservation/superseding reason).
	// The action still happens; these assertions must independently go red.
	assertNamed := func(t *testing.T, res map[string]any, login, action string) {
		t.Helper()
		reason, _ := res["reason"].(string)
		if !strings.Contains(reason, login) || !strings.Contains(reason, action) {
			t.Errorf("reason must name %q and %q, got %q", login, action, reason)
		}
	}

	// W1 mutation: remove `and not preserve_arming(pr)` from stand_down.
	// The PR starts armed; ANY mutation would destroy or supersede that human
	// decision. Exercise both verdict refusals and the vouched_sha refusals.
	for _, tc := range []struct {
		name string
		subs map[string]string
		head string
	}{
		{"hold", map[string]string{"{{input.verdict}}": `"hold_security"`}, ""},
		{"red gate", map[string]string{"{{input.gate_state}}": `"failure"`}, ""},
		{"missing gate", map[string]string{"{{input.gate_posted}}": "False"}, ""},
		{"foreign gate sha", map[string]string{"{{input.gate_sha}}": `"b33fb33f"`}, ""},
		{"moved head", nil, "b33fb33f"},
	} {
		t.Run("W1 human arming survives stand_down/"+tc.name, func(t *testing.T) {
			subs := map[string]string{"{{vars.respect_human_arming}}": "True"}
			for k, v := range tc.subs {
				subs[k] = v
			}
			pr := armedBy("maintainer-alice")
			if tc.head != "" {
				pr["headRefOid"] = tc.head
			}
			res, calls, _ := runWith(t, subs, pr, "", nil)
			if res["armed"] != false || len(mutations(calls)) != 0 {
				t.Fatalf("a refusal must preserve the human arming without claiming its own: %v, mutations=%v", res, mutations(calls))
			}
			assertNamed(t, res, "maintainer-alice", "preserved")
			if strings.Contains(res["reason"].(string), "re-arm by hand") {
				t.Error("an arming left standing must not ask its owner to re-arm it")
			}
		})
	}

	// W2 mutation: delete the login == bot_login return in preserve_arming.
	// Protecting everyone would pass W1 but leave the bot's own arming alive.
	t.Run("W2 bot arming still stands down", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{
			"{{vars.respect_human_arming}}": "True", "{{input.verdict}}": `"hold_security"`,
		}, armedBy(botLogin), "", nil)
		if got := strings.Join(mutations(calls), ","); got != "disablePullRequestAutoMerge" || res["armed"] != false {
			t.Fatalf("want exactly the usual disarm, got %s (%v)", got, res)
		}
		assertNamed(t, res, botLogin, "disarmed and NOT replaced")
	})

	// W3 mutation: remove the preserve_arming early exit in the arm path.
	// The old disarm-then-arm sequence must not replace the maintainer's choice.
	t.Run("W3 human arming survives the arm path", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{"{{vars.respect_human_arming}}": "True"},
			armedBy("maintainer-alice"), "", nil)
		if res["armed"] != true || len(mutations(calls)) != 0 {
			t.Fatalf("want the existing arming standing, no mutation: %v, mutations=%v", res, mutations(calls))
		}
		assertNamed(t, res, "maintainer-alice", "preserved")
	})

	// W3/re-read mutation: remove preserve_arming in merge_now or enqueue_now.
	// An enable refusal can mean someone armed between reads: the owner now
	// OBSERVED must survive the fallback too. W4 controls keep the old actions.
	for _, queue := range []bool{false, true} {
		fallback, armErr := "mergePullRequest", cleanRefusal
		if queue {
			fallback, armErr = "enqueuePullRequest", "Pull request already enabled for auto merge"
		}
		for _, policy := range []string{"True", "False", "null"} {
			for _, login := range []string{"maintainer-alice", botLogin} {
				t.Run("W3 re-read/"+fallback+"/"+policy+"/"+login, func(t *testing.T) {
					after := armedBy(login)
					if !queue {
						after["mergeStateStatus"] = "CLEAN"
					}
					res, calls, _ := runWith(t, map[string]string{"{{vars.respect_human_arming}}": policy},
						blocked, armErr, after, queue)
					want := "enablePullRequestAutoMerge"
					if policy != "True" || login == botLogin {
						want += "," + fallback
						assertNamed(t, res, login, "superseded")
					} else {
						assertNamed(t, res, login, "preserved")
					}
					if got := strings.Join(mutations(calls), ","); got != want || res["armed"] != true {
						t.Fatalf("want %s, got %s (%v)", want, got, res)
					}
				})
			}
		}
		t.Run("W3 re-read after own disarm/"+fallback, func(t *testing.T) {
			after := armedBy("maintainer-alice")
			after["mergeStateStatus"] = "CLEAN"
			res, calls, _ := runWith(t, map[string]string{"{{vars.respect_human_arming}}": "True"},
				armedBy(botLogin), armErr, after, queue)
			if res["armed"] != true || strings.Join(mutations(calls), ",") != "disablePullRequestAutoMerge,enablePullRequestAutoMerge" {
				t.Fatalf("want own disarm followed by preserving the new human arming: %v, %v", res, mutations(calls))
			}
			assertNamed(t, res, botLogin, "was disarmed")
			assertNamed(t, res, "maintainer-alice", "preserved")
			if strings.Contains(res["reason"].(string), "this run's merge commitment") {
				t.Fatalf("this run placed no new commitment: %v", res)
			}
		})
		// The ownership guard must not move BEFORE vouching: a head that
		// changed is still refused, with the human arming preserved and named.
		t.Run("W3 re-read moved head/"+fallback, func(t *testing.T) {
			after := armedBy("maintainer-alice")
			after["headRefOid"], after["mergeStateStatus"] = "b33fb33f", "CLEAN"
			res, calls, _ := runWith(t, map[string]string{"{{vars.respect_human_arming}}": "True"},
				blocked, armErr, after, queue)
			if res["armed"] != false || strings.Join(mutations(calls), ",") != "enablePullRequestAutoMerge" {
				t.Fatalf("a moved head must be refused without taking over the human arming: %v, %v", res, mutations(calls))
			}
			assertNamed(t, res, "maintainer-alice", "preserved")
			if !strings.Contains(res["reason"].(string), "branch moved") {
				t.Fatalf("missing moved-head refusal: %v", res)
			}
		})
	}

	// The same ownership comparison on the primary arm path, case-insensitive.
	t.Run("bot arming is replaced with respect enabled", func(t *testing.T) {
		login := strings.ToUpper(botLogin)
		res, calls, _ := runWith(t, map[string]string{"{{vars.respect_human_arming}}": "True"}, armedBy(login), "", nil)
		if got := strings.Join(mutations(calls), ","); got != "disablePullRequestAutoMerge,enablePullRequestAutoMerge" || res["armed"] != true {
			t.Fatalf("want own arming replaced, got %s (%v)", got, res)
		}
		assertNamed(t, res, login, "was disarmed")
	})

	// W4 mutations: remove the respect_human_arming opt-out (false fails), or
	// treat null as true (the absent-input row fails). Assert the exact ordered
	// mutations, including a failed replacement, and name WHO was displaced.
	for _, policy := range []string{"False", "null"} {
		for _, path := range []string{"stand_down", "arm", "failed replacement"} {
			t.Run("W4 legacy policy/"+policy+"/"+path, func(t *testing.T) {
				subs := map[string]string{"{{vars.respect_human_arming}}": policy}
				want, armed, armErr := "disablePullRequestAutoMerge,enablePullRequestAutoMerge", true, ""
				switch path {
				case "stand_down":
					subs["{{input.verdict}}"] = `"hold_security"`
					want, armed = "disablePullRequestAutoMerge", false
				case "failed replacement":
					armErr, armed = "Auto merge is not allowed for this repository", false
				}
				res, calls, _ := runWith(t, subs, armedBy("maintainer-alice"), armErr, nil)
				if got := strings.Join(mutations(calls), ","); got != want || res["armed"] != armed {
					t.Fatalf("want %s and armed=%v, got %s (%v)", want, armed, got, res)
				}
				assertNamed(t, res, "maintainer-alice", "was disarmed")
			})
		}
	}

	// Ownership is the forge identity, not a hard-coded App or a [bot] suffix.
	// Mutation: classify every [bot] as ours, or compare empty owners as ours.
	for _, login := range []string{"another-app[bot]", ""} {
		for _, verdict := range []string{"clean", "hold_security"} {
			t.Run("external or unknown arming survives/"+login+"/"+verdict, func(t *testing.T) {
				res, calls, _ := runWith(t, map[string]string{
					"{{vars.respect_human_arming}}": "True", "{{input.verdict}}": `"` + verdict + `"`,
				}, armedBy(login), "", nil)
				if len(mutations(calls)) != 0 {
					t.Fatalf("unknown/external ownership permits no takeover, got %v", mutations(calls))
				}
				ownerLabel := login
				if ownerLabel == "" {
					ownerLabel = "(unknown owner)"
				}
				assertNamed(t, res, ownerLabel, "preserved")
			})
		}
	}

	// Under the legacy policy an arming placed by an earlier run — or by a
	// human — cannot be trusted,
	// because its pin cannot be read: `AutoMergeRequest` exposes enabledAt,
	// enabledBy, mergeMethod and the commit message, and NOT expectedHeadOid.
	// So "already armed" says nothing about which commit the forge will merge.
	// The only arming whose pin is known is the one this run places itself.
	t.Run("an existing arming is replaced, never trusted", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"autoMergeRequest": map[string]any{"enabledAt": "2026-09-22T09:00:00Z"}}), "", nil)
		if !queried(calls, "disablePullRequestAutoMerge") {
			t.Error("an opaque arming must be DISARMED first — its expectedHeadOid is unreadable, so accepting it certifies a commit nobody chose")
		}
		if !queried(calls, "enablePullRequestAutoMerge") {
			t.Error("after disarming, this run must place its OWN pinned arming")
		}
		if res["armed"] != true {
			t.Fatalf("want armed after the replacement, got %v", res)
		}
	})

	// Taking an arming down is only defensible when this run can put one back —
	// for the STRUCTURAL refusals. `vouched_sha` exits on four disagreements, and
	// they are not the same kind: two say "the gate never ran / this run cannot
	// name what it read", where the head may well still be the audited one, and
	// two say "the head is demonstrably NOT what was audited". A disarm placed
	// ahead of all four left the pull request bare on refusals this run could
	// have made without touching the forge at all — worst with gate_enabled off,
	// where no gate_sha is ever produced and EVERY pass tore an arming down.
	for _, tc := range []struct {
		name string
		subs map[string]string
	}{
		{"the gate is off, so nothing can ever be vouched",
			map[string]string{"{{vars.gate_enabled}}": "False", "{{input.gate_sha}}": `""`}},
		{"this run cannot name the commit it audited",
			map[string]string{"{{input.audited_sha}}": `""`, "{{input.committed_sha}}": `""`}},
	} {
		t.Run("a STRUCTURAL refusal touches nothing: "+tc.name, func(t *testing.T) {
			res, calls, _ := runWith(t, tc.subs, withState(map[string]any{
				"autoMergeRequest": map[string]any{"enabledAt": "2026-09-22T09:00:00Z"}}), "", nil)
			if res["armed"] != false {
				t.Fatalf("want a refusal, got %v", res)
			}
			if queried(calls, "disablePullRequestAutoMerge") || queried(calls, "dequeuePullRequest") {
				t.Errorf("%s: a commitment was taken down by a pass that then refused to replace it — the PR is left bare (reason=%q)", tc.name, res["reason"])
			}
		})
	}

	// The opposite half: when the head is demonstrably NOT the audited one, any
	// standing commitment would land an unaudited revision. Leaving it is the
	// expensive mistake, so these DO stand down — and say so.
	for _, tc := range []struct {
		name  string
		subs  map[string]string
		state map[string]any
		want  string
	}{
		{"the branch moved after the gate", nil,
			map[string]any{"headRefOid": "b33fb33fb33f", "autoMergeRequest": map[string]any{"enabledAt": "x"}},
			"disablePullRequestAutoMerge"},
		{"the gate landed on a commit this run never audited",
			map[string]string{"{{input.gate_sha}}": `"0ther5ha0000"`},
			map[string]any{"autoMergeRequest": map[string]any{"enabledAt": "x"}},
			"disablePullRequestAutoMerge"},
		// Once the forge moves an armed PR into the queue, autoMergeRequest is
		// null and a disarm is a no-op while the queue still merges the bump.
		{"the branch moved and the PR is already queued", nil,
			map[string]any{"headRefOid": "b33fb33fb33f", "mergeQueueEntry": map[string]any{"id": "MQE_1"}},
			"dequeuePullRequest"},
	} {
		t.Run("an unaudited head stands the commitment down: "+tc.name, func(t *testing.T) {
			res, calls, _ := runWith(t, tc.subs, withState(tc.state), "", nil)
			if res["armed"] != false {
				t.Fatalf("want a refusal, got %v", res)
			}
			if !queried(calls, tc.want) {
				t.Errorf("%s: %s was never sent — the forge still merges this PR the moment its checks go green (reason=%q)", tc.name, tc.want, res["reason"])
			}
			if !strings.Contains(res["reason"].(string), "re-arm by hand") {
				t.Errorf("%s: a PR left bare must say so, got %q", tc.name, res["reason"])
			}
		})
	}

	// The replacement must never leave the PR bare: if the disarm succeeds and
	// the re-arm fails, saying "armed" would be a lie in the most expensive
	// direction — and silently dropping a human's arming is not acceptable
	// either. The reason has to carry it.
	t.Run("replacement that cannot re-arm reports it, and claims nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{
			"autoMergeRequest": map[string]any{"enabledAt": "2026-09-22T09:00:00Z"}}),
			"Auto merge is not allowed for this repository", nil)
		if !queried(calls, "disablePullRequestAutoMerge") {
			t.Fatal("the disarm should still have been attempted")
		}
		if res["armed"] != false {
			t.Fatalf("a failed re-arm must not report armed: %v", res)
		}
		if !strings.Contains(strings.ToLower(res["reason"].(string)), "disarm") {
			t.Errorf("the reason must say the prior arming was removed and not replaced, got %q", res["reason"])
		}
	})

	// A hold is a statement that this revision must NOT merge. An arming a
	// previous pass left behind outlives that statement unless something takes
	// it down — the forge keeps waiting for the checks either way.
	for _, tc := range []struct {
		name string
		subs map[string]string
	}{
		{"held bump", map[string]string{"{{input.verdict}}": `"hold_security"`}},
		{"unstable build", map[string]string{"{{input.verdict}}": `"hold_unstable"`}},
		{"alignment the credential may not push", map[string]string{"{{input.verdict}}": `"hold_delivery_permission"`}},
		// The verdict preflight_standdown hands this node when the delivery
		// preflight did not answer: no verdict was published, so this is the
		// only thing standing between an arming an earlier pass placed and a
		// merge of the bump without its alignment.
		{"preflight that did not answer", map[string]string{"{{input.verdict}}": `"preflight_unavailable"`}},
		{"red gate", map[string]string{"{{input.gate_state}}": `"failure"`}},
		{"gate never landed", map[string]string{"{{input.gate_posted}}": "False"}},
	} {
		t.Run("a refusal disarms what a previous pass armed: "+tc.name, func(t *testing.T) {
			res, calls, _ := runWith(t, tc.subs, withState(map[string]any{
				"autoMergeRequest": map[string]any{"enabledAt": "2026-09-22T09:00:00Z"}}), "", nil)
			if res["armed"] != false {
				t.Fatalf("want a refusal, got %v", res)
			}
			if !queried(calls, "disablePullRequestAutoMerge") {
				t.Errorf("%s: refused without disarming — the forge still merges this PR the moment its checks go green", tc.name)
			}
			if queried(calls, "enablePullRequestAutoMerge") {
				t.Errorf("%s: a refusal must never arm", tc.name)
			}
		})
	}

	// The queue half of the same scenario (#1633): pass 1 went green and armed,
	// the forge moved the PR into the merge queue, and a later pass holds — a
	// CVE published in the interval is the ordinary way a dependency guard
	// changes its mind. Once queued, autoMergeRequest is null, so the disarm
	// alone is a no-op: if the refusal does not dequeue, the queue still
	// merges the very bump this pass refuses.
	t.Run("a refusal dequeues what an earlier pass queued", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{"{{input.verdict}}": `"hold_security"`},
			withState(map[string]any{"mergeQueueEntry": map[string]any{"id": "MQE_1"}}), "", nil)
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
		if !queried(calls, "dequeuePullRequest") {
			t.Error("refused without dequeuing — the merge queue still merges this PR the moment its group checks go green")
		}
		if queried(calls, "enablePullRequestAutoMerge") {
			t.Error("a refusal must never arm")
		}
		if reason, _ := res["reason"].(string); !strings.Contains(reason, "removed from the merge queue") {
			t.Errorf("a PR taken out of the queue must say so, got %q", reason)
		}
	})

	// Off means no mandate, not "undo what someone else decided". The bot must
	// not touch the forge at all.
	t.Run("switched off: the forge is not touched, even to disarm", func(t *testing.T) {
		_, calls, paths := runWith(t, map[string]string{"{{vars.arm_automerge}}": "False"},
			withState(map[string]any{
				"autoMergeRequest": map[string]any{"enabledAt": "2026-09-22T09:00:00Z"}}), "", nil)
		if len(calls) != 0 || len(paths) != 0 {
			t.Errorf("arm_automerge off must make no forge call, got %d call(s) %v", len(calls), paths)
		}
	})

	// `silent` marks the refusals that cannot reach the forge at all — no
	// mandate, wrong forge, no credential. The others DO read the pull request,
	// because a refusal now has to take down an arming an earlier pass left:
	// the property that matters is that nothing merges, not that nothing is
	// read. Asserting "zero calls" on those would forbid the takedown.
	//
	// `still` is the clause a refusal owes when it could not reach the forge
	// (#1635): a live arming may still stand with nobody able to take it down,
	// and the PR is then left in a state the verdict contradicts — allowed,
	// but never silently. "Zero calls" stays asserted on those rows: the point
	// is the MESSAGE, not new forge traffic.
	for _, tc := range []struct {
		name   string
		subs   map[string]string
		want   string
		silent bool
		still  string
	}{
		{"off by default", map[string]string{"{{vars.arm_automerge}}": "False"}, "off", true, ""},
		{"held bump", map[string]string{"{{input.verdict}}": `"hold_security"`}, "not green", false, ""},
		{"unstable build", map[string]string{"{{input.verdict}}": `"hold_unstable"`}, "not green", false, ""},
		{"pending human decision", map[string]string{"{{input.verdict}}": `"needs_decision"`}, "not green", false, ""},
		{"alignment missing from the branch", map[string]string{"{{input.verdict}}": `"hold_lost_alignment"`}, "not green", false, ""},
		{"alignment the credential may not push", map[string]string{"{{input.verdict}}": `"hold_delivery_permission"`}, "not green", false, ""},
		{"preflight that did not answer", map[string]string{"{{input.verdict}}": `"preflight_unavailable"`}, "not green", false, ""},
		{"unknown verdict", map[string]string{"{{input.verdict}}": `"probably_fine"`}, "not green", false, ""},
		// A verdict whose status never reached the PR must not merge it: the
		// gate is what the repo actually gates on.
		{"gate never landed", map[string]string{"{{input.gate_posted}}": "False"}, "did not land", false, ""},
		{"gate red", map[string]string{"{{input.gate_state}}": `"failure"`}, "merge gate is failure", false, ""},
		{"non-github forge", map[string]string{"{{input.pr_url}}": `"https://gitlab.com/a/b/-/merge_requests/3"`}, "GitHub-only", true, ""},
		{"no token", map[string]string{"{{secrets.forge_token.path}}": `""`}, "no forge_token", true, ""},
		// The same refusals with a HOLD on the table: the arming an earlier
		// pass may have placed is just as alive, and nothing can reach it.
		{"held bump, non-github forge", map[string]string{
			"{{input.verdict}}": `"hold_security"`,
			"{{input.pr_url}}":  `"https://gitlab.com/a/b/-/merge_requests/3"`},
			"not green", true, "may still stand"},
		{"held bump, GHES host the bot cannot speak to", map[string]string{
			"{{input.verdict}}": `"hold_security"`,
			"{{input.pr_url}}":  `"https://github.acme.example/acme/widgets/pull/7"`},
			"not green", true, "may still stand"},
		{"held bump, no token", map[string]string{
			"{{input.verdict}}":            `"hold_security"`,
			"{{secrets.forge_token.path}}": `""`},
			"not green", true, "may still stand"},
		{"held bump, unparseable pr_url", map[string]string{
			"{{input.verdict}}": `"hold_security"`,
			"{{input.pr_url}}":  `"not a url"`},
			"not green", true, "may still stand"},
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
			if tc.still != "" && !strings.Contains(reason, tc.still) {
				t.Errorf("reason = %q, want it to warn that a commitment %s — the refusal could not reach the forge", reason, tc.still)
			}
			// The property worth pinning on every refusal: nothing that could
			// land the bump was sent.
			for _, mutation := range []string{"enablePullRequestAutoMerge", "mergePullRequest", "enqueuePullRequest"} {
				if queried(calls, mutation) {
					t.Errorf("a refusal sent %s", mutation)
				}
			}
			if tc.silent && len(calls) != 0 {
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
