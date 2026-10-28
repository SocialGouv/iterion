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
	"sync"
	"testing"
)

// The commits of the one scenario a head pin exists for: the run reads, gates
// and vouches `pinAuditedHead`, and `pinPushedHead` lands on the branch before the
// mutation the run then sends arrives at the forge.
const (
	pinAuditedHead = "1f0a9c3b5d7e2048c6b1a4f8e9d0c3b5a7698012"
	pinPushedHead  = "b7c2d4e6f8091a3b5c7d9e0f1a2b3c4d5e6f7081"
)

// stalePinRefusal is the forge's answer to a pin that is not the commit at
// the tip. Only its SHAPE is load-bearing — an `errors` payload whose message
// is not the clean-status refusal, which is the one message `arm_automerge`
// branches on. GitHub's own wording for a stale `expectedHeadOid` is of this
// form; a stub must not pretend to know it to the letter.
const stalePinRefusal = "Expected head sha did not match current head ref."

// armingPinForge is a stub GitHub that ENFORCES its own head pin.
//
// `served` is what a read answers — the commit this run audits. `tip` is the
// commit actually at the branch head when a mutation arrives. The two differ
// in exactly one situation, and it is the situation `expectedHeadOid` exists
// for: a push that lands after read_pr() and before the mutation.
//
// The rule it applies is the real API's, in both directions:
//
//   - a mutation that BINDS expectedHeadOid is refused when the pin is not the
//     tip — the forge will not act on a commit the caller no longer sees;
//   - a mutation that OMITS it is accepted, because the field is optional
//     there — and then the gesture lands on the tip, which is the commit
//     nobody audited.
//
// A stub that answered success whatever it was sent (the sibling suite's)
// cannot tell those apart, which is why dropping the pin from a mutation
// reddens an orthography assertion there and changes no behaviour at all.
type armingPinForge struct {
	// The handler runs on the server's goroutines; the test reads the
	// gestures afterwards. Both sides take the mutex — the -race leg has
	// reddened on exactly this shape of test observer before.
	mu       sync.Mutex
	served   map[string]any
	tip      string
	hasQueue bool

	armedOn  string // the head the forge ended up armed on, "" when it refused
	mergedOn string
	queuedOn string

	// Which doors were knocked on at all. A door that was never reached
	// reports the same "" as a door that refused, so a witness that read the
	// outcome alone would stay green on a node that stopped arming
	// altogether — or on a queue fallback that never ran.
	attempted []string
}

// knocked reports whether a mutation reached the forge, refused or not.
func (forge *armingPinForge) knocked(field string) bool {
	forge.mu.Lock()
	defer forge.mu.Unlock()
	for _, seen := range forge.attempted {
		if seen == field {
			return true
		}
	}
	return false
}

// gestures reports which head each door of the forge ended up acting on.
func (forge *armingPinForge) gestures() (armed, merged, queued string) {
	forge.mu.Lock()
	defer forge.mu.Unlock()
	return forge.armedOn, forge.mergedOn, forge.queuedOn
}

func (forge *armingPinForge) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	pinned := regexp.MustCompile(`expectedHeadOid:\s*\$oid`)
	writeData := func(w http.ResponseWriter, field string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{field: map[string]any{"clientMutationId": nil}},
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("forge received a body that is not JSON: %v (%q)", err, raw)
			w.WriteHeader(400)
			return
		}
		// The same invariants the real API enforces on the query text: a stub
		// that answers a corrupted query certifies a request production
		// rejects.
		if err := assertValidGraphQL(body.Query); err != "" {
			t.Errorf("invalid GraphQL sent: %s\nquery: %s", err, body.Query)
			w.WriteHeader(400)
			return
		}
		// door answers one pinned mutation and records the head it acted on.
		door := func(landedOn *string, field string) {
			forge.mu.Lock()
			defer forge.mu.Unlock()
			forge.attempted = append(forge.attempted, field)
			if pinned.MatchString(body.Query) && body.Variables["oid"] != forge.tip {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"type": "UNPROCESSABLE", "message": stalePinRefusal}},
				})
				return
			}
			// The forge acts on its own tip, never on the pin it was sent:
			// that is what makes an unpinned gesture land on the commit
			// nobody audited.
			*landedOn = forge.tip
			writeData(w, field)
		}
		switch {
		// Ordered before the arming case deliberately, as in the sibling
		// suite: a reader should not have to check that "disable" carries no
		// "enable" to know which branch answers.
		case strings.Contains(body.Query, "disablePullRequestAutoMerge"):
			writeData(w, "disablePullRequestAutoMerge")
		case strings.Contains(body.Query, "enablePullRequestAutoMerge"):
			door(&forge.armedOn, "enablePullRequestAutoMerge")
		case strings.Contains(body.Query, "mergePullRequest"):
			door(&forge.mergedOn, "mergePullRequest")
		case strings.Contains(body.Query, "enqueuePullRequest"):
			door(&forge.queuedOn, "enqueuePullRequest")
		case strings.Contains(body.Query, "mergeQueue("):
			var mq map[string]any
			forge.mu.Lock()
			if forge.hasQueue {
				mq = map[string]any{"id": "MQ_1"}
			}
			forge.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"repository": map[string]any{"mergeQueue": mq}},
			})
		case strings.Contains(body.Query, "mutation"):
			// An unhandled mutation answered with a pull request read would
			// pass for success here while production refused it.
			t.Errorf("unhandled mutation reached the forge: %s", body.Query)
			w.WriteHeader(400)
		default:
			// mergeStateStatus is preview-gated: the real API rejects the read
			// without the media type.
			if strings.Contains(body.Query, "mergeStateStatus") &&
				!strings.Contains(r.Header.Get("Accept"), "merge-info-preview") {
				t.Errorf("mergeStateStatus queried without the merge-info-preview Accept header")
				w.WriteHeader(400)
				return
			}
			forge.mu.Lock()
			served := forge.served
			forge.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"viewer":     map[string]any{"login": "acme-vetty[bot]"},
					"repository": map[string]any{"pullRequest": served},
				},
			})
		}
	}
}

// run drives arm_automerge against this forge and returns its automerge_output.
func (forge *armingPinForge) run(t *testing.T, script string) map[string]any {
	t.Helper()
	srv := httptest.NewServer(forge.handler(t))
	defer srv.Close()

	body := script
	for ref, val := range map[string]string{
		"{{vars.arm_automerge}}": "True",
		// The new-launch default: another actor's arming is respected. No
		// arming stands on the pull request in these scenarios, so the var
		// only has to be the one a fresh launch carries.
		"{{vars.respect_human_arming}}": "True",
		"{{vars.automerge_method}}":     `"squash"`,
		"{{input.pr_url}}":              `"https://github.com/acme/widgets/pull/7"`,
		"{{input.verdict}}":             `"committed"`,
		"{{input.gate_posted}}":         "True",
		"{{input.gate_state}}":          `"success"`,
		"{{input.gate_sha}}":            `"` + pinAuditedHead + `"`,
		"{{input.audited_sha}}":         `"` + pinAuditedHead + `"`,
		"{{input.committed_sha}}":       `""`,
		"{{vars.gate_enabled}}":         "True",
		"{{secrets.forge_token.path}}":  `"ghs_test"`,
	} {
		body = strings.ReplaceAll(body, ref, val)
	}
	body = strings.ReplaceAll(body, `"https://api.github.com/graphql"`, `"`+srv.URL+`/graphql"`)
	if strings.Contains(body, "{{") {
		t.Fatalf("unsubstituted ref: %s", firstRef(body))
	}

	tmp, err := os.CreateTemp(t.TempDir(), "arming-pin-*.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	out, err := exec.Command("python3", tmp.Name()).Output()
	if err != nil {
		t.Fatalf("arm_automerge failed: %v (out %q)", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not automerge_output JSON: %v (%q)", err, out)
	}
	return res
}

// pinScenarioPR is the state auto-merge exists for: checks still running, at a
// given head, with nothing standing on the pull request yet.
func pinScenarioPR(head string) map[string]any {
	return map[string]any{
		"id": "PR_node_1", "headRefOid": head, "baseRefName": "main",
		"autoMergeRequest": nil, "mergeQueueEntry": nil,
		"mergeable": "MERGEABLE", "mergeStateStatus": "BLOCKED",
	}
}

// TestDepUpdateGuardArmingPinIsEnforcedByTheForge asks of the ARMING what the
// sibling suite only asks of its spelling: what does the node DO when the
// forge enforces `expectedHeadOid`?
//
// TestDepUpdateGuardArmAutomerge asserts that the mutation text contains
// `expectedHeadOid: $oid` and that the variable equals the reviewed head —
// against a stub that answers success whatever it is sent. Drop the pin from
// the mutation and that suite reddens on orthography while the node's
// behaviour is unchanged: the forge still arms, and the pin it describes is a
// guarantee nothing exercises. A pin is a BEHAVIOUR — the forge refuses the
// gesture when the head is no longer the audited one — and a behaviour is
// proved by its consequence. Hence the forge above, and the one question it
// can answer: which commit does the forge end up acting on?
//
// Mutations that redden it, each on the assertion of its own subtest (in
// bots/dep-update-guard/main.bot):
//
//   - drop `, expectedHeadOid: @oid` from `arm_mut` (and `@oid: GitObjectID!`
//     from its declaration) -> "the forge is armed on …";
//   - the same on `enqueue_mut` -> "the merge queue took …".
//
// SCOPE, which is the whole of #1637: this proves the pin at ENABLE time,
// which is all GitHub documents it to be ("the expected head OID of the pull
// request"). Whether an arming carries its pin into the EVENTUAL merge is not
// established — `AutoMergeRequest` exposes enabledAt, enabledBy, mergeMethod
// and the commit message, never expectedHeadOid, so a deferred pin has no
// visible place to live — and no stub can settle it, because a stub is the
// thing being asked. The experiment that can is filed on #1637.
func TestDepUpdateGuardArmingPinIsEnforcedByTheForge(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := toolScript(t, "dep-update-guard/main.bot", "arm_automerge")

	// The control: the pin costs nothing in the ordinary case. Without it this
	// suite could redden for a reason that has nothing to do with the pin —
	// and a witness that cannot come out the other way is not a witness.
	t.Run("the audited head is still at the tip: the arming lands on it", func(t *testing.T) {
		forge := &armingPinForge{served: pinScenarioPR(pinAuditedHead), tip: pinAuditedHead}
		res := forge.run(t, script)
		armed, merged, queued := forge.gestures()
		if armed != pinAuditedHead {
			t.Errorf("the forge is armed on %q, want the audited head %q", armed, pinAuditedHead)
		}
		if merged != "" || queued != "" {
			t.Errorf("a pull request with checks still pending was merged (%q) or enqueued (%q)", merged, queued)
		}
		if res["armed"] != true {
			t.Errorf("node reports %v, want armed on a successful arming", res)
		}
		if !forge.knocked("enablePullRequestAutoMerge") {
			t.Error("no arming mutation reached the forge — the assertions above would read the same on a node that stopped arming altogether")
		}
	})

	t.Run("a push lands in the read-to-arm window: the forge refuses, nothing is armed", func(t *testing.T) {
		forge := &armingPinForge{served: pinScenarioPR(pinAuditedHead), tip: pinPushedHead}
		res := forge.run(t, script)
		armed, merged, queued := forge.gestures()
		if armed != "" {
			t.Errorf("the forge is armed on %s, a commit this run never audited (it audited %s) — the arming reached the forge without its expectedHeadOid pin, so the forge armed whatever was at the tip",
				armed, pinAuditedHead)
		}
		if merged != "" || queued != "" {
			t.Errorf("the unaudited head was merged (%q) or enqueued (%q) after the arming was refused", merged, queued)
		}
		if res["armed"] != false {
			t.Errorf("node reports %v, want armed=false: the forge refused the only gesture it was sent", res)
		}
		// Fail-closed is half the property; the other half is that an
		// un-merged pull request is never a mystery.
		if reason, _ := res["reason"].(string); !strings.Contains(reason, stalePinRefusal) {
			t.Errorf("reason %q does not carry the forge's refusal (%q)", reason, stalePinRefusal)
		}
		if !forge.knocked("enablePullRequestAutoMerge") {
			t.Error("no arming mutation reached the forge — an empty forge proves nothing about a pin nobody sent")
		}
	})

	// The queue is the only door on a queue-protected base, and the node takes
	// it when the arming is refused. It is pinned for the same reason, and the
	// same push proves it: a fallback that enqueued the tip would land the
	// unaudited commit through the back door, with the arming refusal in the
	// log looking like the system working.
	t.Run("queue-protected base: the enqueue fallback is pinned too", func(t *testing.T) {
		forge := &armingPinForge{served: pinScenarioPR(pinAuditedHead), tip: pinPushedHead, hasQueue: true}
		res := forge.run(t, script)
		armed, merged, queued := forge.gestures()
		if queued != "" {
			t.Errorf("the merge queue took %s, a commit this run never audited (it audited %s) — the enqueue fallback reached the forge without its expectedHeadOid pin",
				queued, pinAuditedHead)
		}
		if armed != "" || merged != "" {
			t.Errorf("the unaudited head was armed (%q) or merged (%q)", armed, merged)
		}
		if res["armed"] != false {
			t.Errorf("node reports %v, want armed=false: both doors refused the stale pin", res)
		}
		if reason, _ := res["reason"].(string); !strings.Contains(reason, stalePinRefusal) {
			t.Errorf("reason %q does not carry the forge's refusal (%q)", reason, stalePinRefusal)
		}
		// Without this, the subtest reads identically to one where the queue
		// fallback never ran: the arming refusal alone would satisfy every
		// assertion above while the enqueue door went untested.
		if !forge.knocked("enqueuePullRequest") {
			t.Error("the enqueue fallback never reached the forge — the queue door this subtest exists for was not exercised")
		}
	})
}
