package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDepUpdateGuardArmAutomerge pins the one node in the fleet that can end
// with code merged into a default branch. Two properties carry all the weight:
//
//   - it never merges past a REQUIRED check. Which of the three forge calls it makes
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

	// rollup is what the stub answers to the check-rollup query: the contexts
	// on the head commit (nil answers a null rollup, which reads as
	// unreadable) and the commit they are about ("" = the served head).
	// hasNext says the contexts run past the page; then, when set, is what
	// every read after the first answers — a check flipping mid-settle.
	var rollup struct {
		nodes   []map[string]any
		oid     string
		hasNext bool
		then    []map[string]any
		reads   int
	}
	withRollup := func(t *testing.T, oid string, nodes ...map[string]any) {
		t.Helper()
		rollup.nodes, rollup.oid = nodes, oid
		t.Cleanup(func() {
			rollup.nodes, rollup.oid, rollup.hasNext, rollup.then, rollup.reads = nil, "", false, nil, 0
		})
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
			case strings.Contains(body.Query, "statusCheckRollup"):
				oid := rollup.oid
				if oid == "" {
					oid, _ = state["headRefOid"].(string)
				}
				nodes := rollup.nodes
				if rollup.reads > 0 && rollup.then != nil {
					nodes = rollup.then
				}
				rollup.reads++
				var sc any
				if nodes != nil {
					sc = map[string]any{"contexts": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": rollup.hasNext}, "nodes": nodes}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"commits": map[string]any{"nodes": []any{
						map[string]any{"commit": map[string]any{"oid": oid, "statusCheckRollup": sc}}}}}}}})
			// Ordered before the arming case deliberately, even though the two
			// names do not collide as substrings ("disable" carries no
			// "enable"): the reader should not have to verify that to know
			// which branch answers.
			case strings.Contains(body.Query, "disablePullRequestAutoMerge"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"disablePullRequestAutoMerge": map[string]any{"clientMutationId": nil}},
				})
			case strings.Contains(body.Query, "enablePullRequestAutoMerge"):
				// The ARMING carries the same pin as the merge and the
				// enqueue, for the window a run can close: a push landing
				// between the head read and this mutation is refused instead
				// of armed. The pin says nothing about the EVENTUAL merge —
				// AutoMergeRequest exposes no expectedHeadOid, so a deferred
				// pin has nowhere to live, and the experiment that would
				// settle it is filed on #1637.
				// The two assertions below are that pin's ORTHOGRAPHY, and
				// this stub cannot be more: it answers success whatever it is
				// sent, so dropping the pin from the mutation reddens them
				// while changing no behaviour at all. The BEHAVIOUR — a forge
				// that refuses a stale pin, and what the node does then — is
				// TestDepUpdateGuardArmingPinIsEnforcedByTheForge.
				if !regexp.MustCompile(`expectedHeadOid:\s*\$oid`).MatchString(body.Query) {
					t.Errorf("arming mutation is not pinned to the oid variable: %s", body.Query)
				}
				if body.Variables["oid"] != state["headRefOid"] {
					t.Errorf("arming pinned to %v, want the reviewed head %v", body.Variables["oid"], state["headRefOid"])
				}
				if code, _ := state["__armHTTP"].(int); code != 0 {
					// A transport failure, not a GraphQL refusal.
					if after != nil {
						state = after
					}
					w.WriteHeader(code)
					_, _ = w.Write([]byte(`{"message":"upstream failure"}`))
					return
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
				// A state carrying __next is served once, then replaced by
				// it: the pull request settling between two reads. Every
				// "__" key is the stub's own, never a pull request field;
				// __autoMergeAllowed is the repository's switch (on unless
				// a state says otherwise).
				served := map[string]any{}
				for k, v := range state {
					if !strings.HasPrefix(k, "__") {
						served[k] = v
					}
				}
				allowed := any(true)
				if v, ok := state["__autoMergeAllowed"]; ok {
					allowed = v
				}
				if nx, ok := state["__next"].(map[string]any); ok {
					state = nx
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"viewer":     map[string]any{"login": botLogin},
						"repository": map[string]any{"autoMergeAllowed": allowed, "pullRequest": served},
					},
				})
			}
		}))
		defer srv.Close()

		base := map[string]string{
			"{{vars.arm_automerge}}": "True",
			// Legacy node input: missing refs render as null (the Python shim
			// maps it to None). W1-W3 override this with the new-launch default.
			"{{vars.respect_human_arming}}":  "null",
			"{{vars.automerge_method}}":      `"squash"`,
			"{{input.pr_url}}":               `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":              `"committed"`,
			"{{input.gate_posted}}":          "True",
			"{{input.gate_state}}":           `"success"`,
			"{{input.gate_sha}}":             `"d34db33f"`,
			"{{input.audited_sha}}":          `"d34db33f"`,
			"{{input.committed_sha}}":        `""`,
			"{{vars.gate_enabled}}":          "True",
			"{{secrets.forge_token.path}}":   `"ghs_test"`,
			"{{vars.automerge_on_unstable}}": `"merge"`,
			"{{vars.automerge_settle_s}}":    "600",
			"{{vars.gate_context}}":          `"vetty/deps"`,
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
			"{{input.audited_sha}}":   `"a11gned0"`,
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
			"{{input.audited_sha}}":   `"a11gne"`,
			"{{input.committed_sha}}": `"a11gne"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": full}), cleanRefusal, nil)
		if queried(calls, "mergePullRequest") {
			t.Fatalf("merged on a 6-hex prefix: %v", res)
		}
		if res["armed"] != false {
			t.Fatalf("want a refusal, got %v", res)
		}
	})

	// When the bot pushed an alignment, THAT is the commit it vouches for:
	// post_feedback pinned the verdict to it, and audited_sha is that pin.
	t.Run("aligned bump: merges the commit it pushed", func(t *testing.T) {
		res, calls, _ := runWith(t, map[string]string{
			"{{input.gate_sha}}":      `"a11gned0"`,
			"{{input.audited_sha}}":   `"a11gned0"`,
			"{{input.committed_sha}}": `"a11gned0"`,
		}, withState(map[string]any{"mergeStateStatus": "CLEAN", "headRefOid": "a11gned0"}), cleanRefusal, nil)
		if !queried(calls, "mergePullRequest") {
			t.Fatal("the commit Vetty pushed and gated must be mergeable")
		}
		if res["armed"] != true {
			t.Fatalf("want merged, got %v", res)
		}
	})

	// A repin carried the verdict onto the head a pure rebase produced, after
	// this run had pushed its alignment: the gate is on the new head, and
	// post_feedback's pin (audited_sha) says so. Mutation: let committed_sha
	// outrank audited_sha again -> the run refuses the head it just gated,
	// with a reason that reads as a fault.
	t.Run("a repinned verdict arms the head it was carried onto", func(t *testing.T) {
		// A carried verdict is armed only where a check the base requires,
		// other than the gate, builds the head: here CI, still running.
		withRollup(t, "", map[string]any{"__typename": "CheckRun", "name": "ci/test", "status": "IN_PROGRESS", "conclusion": nil, "isRequired": true})
		res, calls, _ := runWith(t, map[string]string{
			"{{input.verdict}}":       `"carried"`,
			"{{input.gate_sha}}":      `"d34db33f"`,
			"{{input.audited_sha}}":   `"d34db33f"`,
			"{{input.committed_sha}}": `"a11gned0"`,
		}, blocked, "", nil)
		if res["armed"] != true {
			t.Fatalf("the head the verdict was repinned onto was not armed: %v", res)
		}
		armed := false
		for _, c := range calls {
			if strings.Contains(c.query, "enablePullRequestAutoMerge") && c.vars["oid"] == "d34db33f" {
				armed = true
			}
		}
		if !armed {
			t.Error("no arming pinned to the repinned head was sent")
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

	// A failed gate on this run's own revision still stands down. A different
	// head belongs to its newer run, even when this older run cannot vouch for it.
	for _, tc := range []struct {
		name  string
		subs  map[string]string
		state map[string]any
		want  string
	}{
		{"the branch moved after the gate", nil,
			map[string]any{"headRefOid": "b33fb33fb33f", "autoMergeRequest": map[string]any{"enabledAt": "x"}},
			""},
		{"the gate landed on a commit this run never audited",
			map[string]string{"{{input.gate_sha}}": `"0ther5ha0000"`},
			map[string]any{"autoMergeRequest": map[string]any{"enabledAt": "x"}},
			"disablePullRequestAutoMerge"},
		// Once the forge moves an armed PR into the queue, autoMergeRequest is
		// null and a disarm is a no-op while the queue still merges the bump.
		{"the branch moved and the PR is already queued", nil,
			map[string]any{"headRefOid": "b33fb33fb33f", "mergeQueueEntry": map[string]any{"id": "MQE_1"}},
			""},
	} {
		t.Run("a gate refusal stays within its revision: "+tc.name, func(t *testing.T) {
			res, calls, _ := runWith(t, tc.subs, withState(tc.state), "", nil)
			if res["armed"] != false {
				t.Fatalf("want a refusal, got %v", res)
			}
			if tc.want == "" {
				if len(mutations(calls)) != 0 {
					t.Fatalf("a stale run touched a newer revision: %v", mutations(calls))
				}
				return
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

	// A run resumed after its push may hold against the old audited sha.
	// It must not disarm or dequeue a newer run on the now-current head.
	for _, verdict := range []string{"hold_lost_alignment", "hold_security", "committed"} {
		for _, current := range []string{"d34db33f", "b33fb33fb33f", ""} {
			t.Run("stand-down belongs to its revision/"+verdict+"/"+current, func(t *testing.T) {
				res, calls, _ := runWith(t, map[string]string{"{{input.verdict}}": `"` + verdict + `"`, "{{input.gate_state}}": `"failure"`},
					withState(map[string]any{"headRefOid": current, "autoMergeRequest": map[string]any{"enabledBy": map[string]any{"login": botLogin}}, "mergeQueueEntry": map[string]any{"id": "MQE_1"}}), "", nil)
				wantTakeDown := current == "d34db33f"
				for _, mutation := range []string{"disablePullRequestAutoMerge", "dequeuePullRequest"} {
					if queried(calls, mutation) != wantTakeDown {
						t.Fatalf("%s: old run changed the newer revision's commitment, or failed to stand down its own: %v", mutation, res)
					}
				}
				if res["armed"] != false {
					t.Fatalf("a refusal must not arm: %v", res)
				}
			})
		}
	}

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

	// UNSTABLE: mergeable, every REQUIRED check passing, a non-required one
	// failing or still running. GitHub refuses to arm it as it refuses CLEAN —
	// nothing required is left to wait for — and the bot used to leave it
	// there, green and unarmed, until a human acted.
	const unstableRefusal = "Pull request Pull request is in unstable status"
	unstablePR := withState(map[string]any{"mergeStateStatus": "UNSTABLE"})
	optionalRed := map[string]any{"__typename": "CheckRun", "name": "cloud-e2e", "status": "COMPLETED", "conclusion": "FAILURE", "isRequired": false}
	requiredGreen := map[string]any{"__typename": "StatusContext", "context": "test", "state": "SUCCESS", "isRequired": true}
	mergedOn := func(calls []call) []any {
		var oids []any
		for _, c := range calls {
			if strings.Contains(c.query, "mergePullRequest") {
				oids = append(oids, c.vars["oid"])
			}
		}
		return oids
	}

	// Mutation: revert to the message-only `"clean status" not in …: raise`
	// with a CLEAN-only ready() -> no mergePullRequest.
	t.Run("UNSTABLE, failing context not required: arm refused, merges pinned to the reviewed head", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		res, calls, _ := runWith(t, nil, unstablePR, unstableRefusal, nil)
		if got := mergedOn(calls); len(got) != 1 || got[0] != "d34db33f" {
			t.Fatalf("want exactly one merge pinned to the reviewed head d34db33f, got %v (%v)", got, res)
		}
		if res["armed"] != true || !strings.Contains(res["reason"].(string), "cloud-e2e") {
			t.Errorf("want armed, the non-passing context named, got %v", res)
		}
	})

	// Mutation: drop the isRequired filter -> it merges past a required check.
	t.Run("UNSTABLE with a REQUIRED context not passing: merges nothing", func(t *testing.T) {
		withRollup(t, "", optionalRed, map[string]any{"__typename": "StatusContext", "context": "test", "state": "PENDING", "isRequired": true})
		res, calls, _ := runWith(t, nil, unstablePR, unstableRefusal, nil)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged past a required check: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "REQUIRED") || !strings.Contains(reason, "test") {
			t.Errorf("the refusal must name the required context, got %q", reason)
		}
	})

	for _, tc := range []struct {
		name  string
		oid   string
		nodes []map[string]any
	}{
		{"a null rollup", "", nil},
		{"a context whose requiredness is not stated", "", []map[string]any{{"__typename": "CheckRun", "name": "cloud-e2e", "status": "COMPLETED", "conclusion": "FAILURE"}}},
		// Mutation: drop the rollup's commit-oid check -> it judges the
		// checks of a head this run never audited.
		{"a rollup about another commit", "0ther5ha", []map[string]any{optionalRed}},
		// Mutation: skip a context of an unknown kind instead of refusing
		// -> it merges past a check it cannot read.
		{"a context of a kind it does not know", "", []map[string]any{optionalRed,
			{"__typename": "FutureCheck", "name": "license", "isRequired": true}}},
	} {
		t.Run("UNSTABLE, "+tc.name+": merges nothing", func(t *testing.T) {
			withRollup(t, tc.oid, tc.nodes...)
			res, calls, _ := runWith(t, nil, unstablePR, unstableRefusal, nil)
			if len(mergedOn(calls)) != 0 || res["armed"] != false {
				t.Fatalf("merged on a rollup it could not read whole: %v", res)
			}
		})
	}

	// Mutation: remove the settle loop -> the flip case is refused.
	t.Run("wait mode: UNSTABLE, then CLEAN on the next read, merges", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		after := withState(map[string]any{"mergeStateStatus": "UNSTABLE", "__next": withState(map[string]any{"mergeStateStatus": "CLEAN"})})
		res, calls, _ := runWith(t, map[string]string{"{{vars.automerge_on_unstable}}": `"wait"`, "{{vars.automerge_settle_s}}": "1"},
			blocked, unstableRefusal, after)
		if got := mergedOn(calls); len(got) != 1 || got[0] != "d34db33f" || res["armed"] != true {
			t.Fatalf("want the settled pull request merged pinned, got merges %v (%v)", got, res)
		}
	})

	t.Run("wait mode: still UNSTABLE past automerge_settle_s refuses, naming the contexts", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		res, calls, _ := runWith(t, map[string]string{"{{vars.automerge_on_unstable}}": `"wait"`, "{{vars.automerge_settle_s}}": "1"},
			unstablePR, unstableRefusal, nil)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged a pull request that never settled: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "cloud-e2e") || !strings.Contains(reason, "automerge_settle_s") {
			t.Errorf("the refusal must name the contexts and the bound, got %q", reason)
		}
	})

	// A node input from before the var renders null: the historical refusal.
	t.Run("an unset UNSTABLE policy merges nothing", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		res, calls, _ := runWith(t, map[string]string{"{{vars.automerge_on_unstable}}": "null"}, unstablePR, unstableRefusal, nil)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("an unset policy merged: %v", res)
		}
	})

	// Mutation: drop the hasNextPage check -> it merges on the first page of
	// contexts, past whatever the next one holds.
	t.Run("UNSTABLE, a rollup past one page: merges nothing", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		rollup.hasNext = true
		res, calls, _ := runWith(t, nil, unstablePR, unstableRefusal, nil)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged on a rollup it read one page of: %v", res)
		}
	})

	waitFor1s := map[string]string{"{{vars.automerge_on_unstable}}": `"wait"`, "{{vars.automerge_settle_s}}": "1"}
	// Mutation: drop the re-vouch inside the settle loop -> the refusal no
	// longer says the branch moved. The rollup stays about the audited head,
	// so only the re-vouch sees the move.
	t.Run("wait mode: the head moves while settling: merges nothing, says the branch moved", func(t *testing.T) {
		withRollup(t, "d34db33f", optionalRed, requiredGreen)
		after := withState(map[string]any{"mergeStateStatus": "UNSTABLE",
			"__next": withState(map[string]any{"mergeStateStatus": "UNSTABLE", "headRefOid": "0ther5ha"})})
		res, calls, _ := runWith(t, waitFor1s, blocked, unstableRefusal, after)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged while the head moved: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "branch moved") {
			t.Errorf("the refusal must say the branch moved, got %q", reason)
		}
	})
	// Mutation: drop the loop's 'left UNSTABLE' exit -> it waits out the
	// bound and says it is still UNSTABLE.
	t.Run("wait mode: the pull request leaves UNSTABLE while settling: merges nothing, says so", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		after := withState(map[string]any{"mergeStateStatus": "UNSTABLE",
			"__next": withState(map[string]any{"mergeStateStatus": "BLOCKED"})})
		res, calls, _ := runWith(t, waitFor1s, blocked, unstableRefusal, after)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged a pull request that left UNSTABLE: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "left UNSTABLE for BLOCKED") {
			t.Errorf("the refusal must say the pull request left UNSTABLE, got %q", reason)
		}
	})
	// Mutation: drop the loop's required re-check -> it waits out the bound
	// and says it is still UNSTABLE, never that a required check broke.
	t.Run("wait mode: a required check stops passing while settling: merges nothing, says so", func(t *testing.T) {
		withRollup(t, "", optionalRed, requiredGreen)
		rollup.then = []map[string]any{optionalRed,
			{"__typename": "StatusContext", "context": "test", "state": "FAILURE", "isRequired": true}}
		after := withState(map[string]any{"mergeStateStatus": "UNSTABLE",
			"__next": withState(map[string]any{"mergeStateStatus": "UNSTABLE"})})
		res, calls, _ := runWith(t, waitFor1s, blocked, unstableRefusal, after)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("merged past a required check that broke while settling: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "REQUIRED check stopped passing") {
			t.Errorf("the refusal must name the broken required check, got %q", reason)
		}
	})

	// A transport failure is not a refusal. Mutation: drop the HTTPError
	// re-raise -> the failure is classified by re-reading, and a CLEAN pull
	// request is merged on it.
	t.Run("arming fails in transport on a CLEAN pull request: reported, merges nothing", func(t *testing.T) {
		res, calls, _ := runWith(t, nil, withState(map[string]any{"mergeStateStatus": "CLEAN", "__armHTTP": 502}), "", nil)
		if len(mergedOn(calls)) != 0 || res["armed"] != false {
			t.Fatalf("a transport failure was turned into a merge: %v", res)
		}
		if reason := res["reason"].(string); !strings.Contains(reason, "forge HTTP 502") {
			t.Errorf("the transport failure must be reported as such, got %q", reason)
		}
	})

	// The repository's own switch. Mutation: drop the autoMergeAllowed check
	// -> a repository whose owner switched auto-merge off gets the merge
	// Vetty makes in its place.
	for _, state := range []string{"CLEAN", "UNSTABLE"} {
		t.Run("a repository that does not allow auto-merge, "+state+": merges nothing", func(t *testing.T) {
			withRollup(t, "", optionalRed, requiredGreen)
			pr := withState(map[string]any{"mergeStateStatus": state, "__autoMergeAllowed": false})
			res, calls, _ := runWith(t, nil, pr, "Pull request Auto merge is not allowed for this repository", nil)
			if len(mergedOn(calls)) != 0 || res["armed"] != false {
				t.Fatalf("merged on a repository that does not allow auto-merge: %v", res)
			}
			if reason := res["reason"].(string); !strings.Contains(reason, "does not allow auto-merge") {
				t.Errorf("the refusal must name the repository's switch, got %q", reason)
			}
		})
	}

	// The policy is one rule for every green verdict. An arming the forge
	// accepts while a required check still runs lets GitHub merge once the
	// REQUIRED checks pass, whatever a non-required check says, for every
	// verdict; a refused arming on UNSTABLE follows the same rule, so CI
	// timing never decides which of the two a verdict gets. Mutation: give
	// `carried` its own policy in settle_unstable -> the carried row refuses.
	for _, verdict := range depUpdateGuardGreenVerdicts {
		t.Run("verdict "+verdict+" on an UNSTABLE pull request follows automerge_on_unstable", func(t *testing.T) {
			withRollup(t, "", optionalRed, requiredGreen)
			res, calls, _ := runWith(t, map[string]string{"{{input.verdict}}": `"` + verdict + `"`, "{{vars.automerge_settle_s}}": "1"},
				unstablePR, unstableRefusal, nil)
			if got := mergedOn(calls); len(got) != 1 || got[0] != "d34db33f" || res["armed"] != true {
				t.Fatalf("a %s verdict did not follow the default merge policy: merges %v (%v)", verdict, got, res)
			}
		})
	}

	// A base that requires no check: UNSTABLE says nothing about the build,
	// and GitHub could not have been asked to wait on anything. Mutation: drop
	// the required-and-passing condition -> it merges past a red or running
	// check on a repository that gates nothing.
	for _, tc := range []struct {
		name  string
		nodes []map[string]any
	}{
		{"a red check, nothing required", []map[string]any{optionalRed}},
		{"a red check beside a passing one, nothing required", []map[string]any{optionalRed,
			{"__typename": "StatusContext", "context": "lint", "state": "SUCCESS", "isRequired": false}}},
	} {
		t.Run("UNSTABLE, "+tc.name+": merges nothing", func(t *testing.T) {
			withRollup(t, "", tc.nodes...)
			res, calls, _ := runWith(t, nil, unstablePR, unstableRefusal, nil)
			if len(mergedOn(calls)) != 0 || res["armed"] != false {
				t.Fatalf("merged an UNSTABLE pull request whose base requires no check: %v", res)
			}
			if reason := res["reason"].(string); !strings.Contains(reason, "requires no check") {
				t.Errorf("the refusal must say the base requires no check, got %q", reason)
			}
		})
	}

	t.Run("the default UNSTABLE policy is merge", func(t *testing.T) {
		v := compilePlanPhaseBot(t, "dep-update-guard").Vars["automerge_on_unstable"]
		if v == nil || !v.HasDefault || v.Default != "merge" || !slices.Equal(v.EnumValues, []string{"merge", "wait"}) {
			t.Fatalf("automerge_on_unstable must be the enum merge|wait defaulting to merge, got %+v", v)
		}
	})

	// Every green verdict arms, pinned; `carried` is the identical change's
	// audit carried onto this head. Mutation: remove 'carried' from the green
	// set -> the carried row refuses.
	for _, verdict := range depUpdateGuardGreenVerdicts {
		t.Run("verdict "+verdict+" is green: arms pinned", func(t *testing.T) {
			// CI the base requires is still running: what a carried verdict
			// leans on, and what the forge holds the arming to.
			withRollup(t, "", map[string]any{"__typename": "CheckRun", "name": "ci/test", "status": "IN_PROGRESS", "conclusion": nil, "isRequired": true})
			res, calls, _ := run(t, map[string]string{"{{input.verdict}}": `"` + verdict + `"`})
			if res["armed"] != true {
				t.Fatalf("a %s verdict with a green gate must arm: %v", verdict, res)
			}
			armed := false
			for _, c := range calls {
				if strings.Contains(c.query, "enablePullRequestAutoMerge") && c.vars["oid"] == "d34db33f" {
					armed = true
				}
			}
			if !armed {
				t.Errorf("no arming pinned to the reviewed head was sent for %s", verdict)
			}
		})
	}

	// A carried verdict attests no build: Vetty audited an identical change
	// elsewhere and never built this head. On a base that requires no check
	// but the gate, nothing would build it before the forge merged it — so it
	// is neither armed nor merged, at any door, and the run's own earlier
	// arming on this revision is taken down. Mutation: drop the carried door
	// before the arming in arm_automerge -> every row below arms or merges.
	gateGreen := map[string]any{"__typename": "StatusContext", "context": "vetty/deps", "state": "SUCCESS", "isRequired": true}
	for _, tc := range []struct {
		name  string
		pr    map[string]any
		err   string
		nodes []map[string]any
	}{
		{"BLOCKED on a human review: the arming would be accepted", withState(map[string]any{"mergeStateStatus": "BLOCKED"}), "", []map[string]any{gateGreen}},
		{"CLEAN with no CI at all", withState(map[string]any{"mergeStateStatus": "CLEAN"}), cleanRefusal, []map[string]any{gateGreen}},
		{"UNSTABLE, CI red and not required", unstablePR, unstableRefusal, []map[string]any{gateGreen, optionalRed}},
		{"UNSTABLE, CI still running and not required", unstablePR, unstableRefusal, []map[string]any{gateGreen,
			{"__typename": "CheckRun", "name": "ci/test", "status": "IN_PROGRESS", "conclusion": nil, "isRequired": false}}},
		{"a rollup that cannot be read", withState(map[string]any{"mergeStateStatus": "CLEAN"}), cleanRefusal, nil},
	} {
		t.Run("a carried verdict on a base that requires only the gate, "+tc.name+": neither armed nor merged", func(t *testing.T) {
			withRollup(t, "", tc.nodes...)
			pr := withState(tc.pr)
			pr["autoMergeRequest"] = map[string]any{"enabledAt": "2026-11-10T08:00:00Z", "enabledBy": map[string]any{"login": botLogin}}
			res, calls, _ := runWith(t, map[string]string{"{{input.verdict}}": `"carried"`, "{{vars.respect_human_arming}}": "True"}, pr, tc.err, nil)
			for _, door := range []string{"enablePullRequestAutoMerge", "mergePullRequest", "enqueuePullRequest"} {
				if queried(calls, door) {
					t.Errorf("a carried head no required check builds went through %s: %v", door, res)
				}
			}
			if res["armed"] != false {
				t.Fatalf("want a refusal, got %v", res)
			}
			if reason, _ := res["reason"].(string); !strings.Contains(reason, "vetty/deps") || !strings.Contains(reason, "carried") {
				t.Errorf("the refusal must say a carried verdict attests no build and name the gate, got %q", reason)
			}
			if !queried(calls, "disablePullRequestAutoMerge") {
				t.Errorf("the run's own arming on this revision was left standing: %v", res)
			}
		})
	}
	// The control: a required CI context that passes is the build the
	// carried verdict leans on, and the pull request merges pinned.
	t.Run("a carried verdict on a base that requires CI, CLEAN: merges pinned to the reviewed head", func(t *testing.T) {
		withRollup(t, "", gateGreen, requiredGreen)
		res, calls, _ := runWith(t, map[string]string{"{{input.verdict}}": `"carried"`}, withState(map[string]any{"mergeStateStatus": "CLEAN"}), cleanRefusal, nil)
		if got := mergedOn(calls); len(got) != 1 || got[0] != "d34db33f" || res["armed"] != true {
			t.Fatalf("want one merge pinned to d34db33f, got %v (%v)", got, res)
		}
	})

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
	// The API rejects a bare isRequired: it needs the pull request it is
	// asked about.
	for _, at := range regexp.MustCompile(`isRequired\b`).FindAllStringIndex(q, -1) {
		if !regexp.MustCompile(`^isRequired\(\s*pullRequest(Number|Id):\s*\$[A-Za-z_]`).MatchString(q[at[0]:]) {
			return "isRequired without its pullRequestNumber/pullRequestId argument"
		}
	}
	return ""
}
