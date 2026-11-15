package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// Evaluation mode — `publish_mode: off` — is an operator's measurement run
// (#3283): the bot does all its work and writes nothing a team reads. On
// revi-converse the agent drafts and publish_reply sends no request; on
// review-pr the review is built whole and publish_review sends nothing to
// the forge-publish endpoint, so neither a review nor a merge-gate status
// lands. The health gates read the withheld result as evaluation mode:
// neither degraded nor OK.

const evalWithheldReason = "evaluation mode (publish_mode=off)"

// A valid draft, a usable token mounted, every forge lane: in evaluation mode
// the forge sees no request at all — not the POST, not the duplicate check's
// read. The output says why, carries the draft's preview and the call it
// would have made, and flags the reply as withheld. The same case with the
// var at its default posts once: the var is what decides.
func TestReviConversePublishReplyEvaluationModeSendsNothing(t *testing.T) {
	requirePython(t)
	for _, tc := range publishForgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"off", ""} {
				f, pr := newCaseForge(t, tc)
				vars := publishVars(tc, f.srv.URL, pr)
				vars["publish_mode"] = mode
				script := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					vars, publishRunURL, writeToken(t))
				res, raw := runPublish(t, script, noProxyEnv())
				hits := f.seen()
				if mode == "" {
					if !*res.Posted || res.Evaluation || len(writes(hits)) != 1 {
						t.Fatalf("publish_mode unset: posted = %v, evaluation %v, %d writes — want the one POST (%s)", *res.Posted, res.Evaluation, len(writes(hits)), raw)
					}
					continue
				}
				if len(hits) != 0 {
					t.Fatalf("publish_mode=off: the forge saw %+v, want no request at all", hits)
				}
				if *res.Posted || res.ReplyURL != "" || !res.Evaluation {
					t.Fatalf("publish_mode=off: posted = %v, reply_url %q, evaluation %v — want a withheld reply (%s)", *res.Posted, res.ReplyURL, res.Evaluation, raw)
				}
				if !strings.HasPrefix(res.SkippedReason, evalWithheldReason) {
					t.Errorf("skipped_reason %q does not say evaluation mode", res.SkippedReason)
				}
				if res.AnswerPreview != tc.wantBody {
					t.Errorf("answer_preview = %q, want the body it would have posted %q", res.AnswerPreview, tc.wantBody)
				}
				if want := "POST " + f.srv.URL + tc.wantPost; !strings.Contains(res.Summary, want) {
					t.Errorf("summary %q does not name the call it withheld, %q", res.Summary, want)
				}
			}
		})
	}
}

// Evaluation mode needs no token — it sends nothing — but it keeps every
// check that is about the draft or its target, under that check's own
// reason: an empty draft, a pr_url this step cannot reply on, and an answer
// carrying the forge token (its preview withheld) are not evaluation runs
// that went fine. None of them reaches the forge either.
func TestReviConversePublishReplyEvaluationModeKeepsItsChecks(t *testing.T) {
	requirePython(t)
	answer := map[string]any{"answer": publishAnswer, "skipped_reason": ""}
	for _, c := range []struct {
		name           string
		input          map[string]any
		prPath         string
		noToken        bool
		wantEvaluation bool
		wantReason     string
		wantPreview    string
	}{
		{name: "no token mounted", input: answer, prPath: "/o/r/pull/1", noToken: true, wantEvaluation: true, wantReason: evalWithheldReason, wantPreview: publishAnswer},
		{name: "an empty draft", input: map[string]any{"answer": "", "skipped_reason": "no question in the note"}, prPath: "/o/r/pull/1", wantReason: "no question in the note"},
		{name: "a pr_url this step cannot reply on", input: answer, prPath: "/o/r/issues/1", wantReason: "not a pull request URL"},
		{name: "an answer carrying the forge token", input: map[string]any{"answer": "Re: op\n\nThe key is " + publishToken + " (app.go:3).", "skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "the answer carries the forge token", wantPreview: "(withheld: the answer carried a credential)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			pr := f.srv.URL + c.prPath
			tokenPath := writeToken(t)
			if c.noToken {
				tokenPath = filepath.Join(t.TempDir(), "absent", "forge_token")
			}
			script := renderReviConversePublish(t, c.input,
				map[string]any{"pr_url": pr, "publish_mode": "off"}, publishRunURL, tokenPath)
			res, raw := runPublish(t, script, noProxyEnv())
			if hits := f.seen(); len(hits) != 0 {
				t.Fatalf("the forge saw %+v, want no request", hits)
			}
			if *res.Posted || res.Evaluation != c.wantEvaluation {
				t.Fatalf("posted = %v, evaluation %v — want posted=false, evaluation=%v (%s)", *res.Posted, res.Evaluation, c.wantEvaluation, raw)
			}
			if !strings.Contains(res.SkippedReason, c.wantReason) {
				t.Errorf("skipped_reason %q does not name %q", res.SkippedReason, c.wantReason)
			}
			if c.wantPreview != "" && res.AnswerPreview != c.wantPreview {
				t.Errorf("answer_preview = %q, want %q", res.AnswerPreview, c.wantPreview)
			}
		})
	}
}

// The chain as the engine runs it, the var set at launch: the draft crosses
// into publish_reply, which sends nothing, and its result crosses into
// converse_health, which reads evaluation mode — not degraded, and never the
// OK of an answered question. The same launch without the var posts and
// reads OK.
func TestReviConverseChainEvaluationModeWithholdsTheReply(t *testing.T) {
	chainEnv(t)
	wf := reviConverseWorkflow(t)
	for _, mode := range []string{"off", ""} {
		t.Run("publish_mode="+mode, func(t *testing.T) {
			r := runChainWith(t, wf, chainDraft(t, wf, publishAnswer, ""), true, nil, map[string]any{"publish_mode": mode})
			if r.runErr != nil {
				t.Fatalf("the run failed: %v", r.runErr)
			}
			degraded, banner := r.health(t)
			hits := r.forge.seen()
			if mode == "" {
				if len(writes(hits)) != 1 || degraded != false || !strings.HasPrefix(banner, "forge reply OK: ") {
					t.Fatalf("publish_mode unset: %d writes, degraded %v, banner %q — want the reply posted and OK", len(writes(hits)), degraded, banner)
				}
				return
			}
			if len(hits) != 0 {
				t.Fatalf("publish_mode=off: the forge saw %+v, want no request", hits)
			}
			if pub := r.exec.output("publish_reply"); pub["evaluation"] != true || pub["posted"] != false {
				t.Errorf("publish_reply = %v, want posted=false and evaluation=true", pub)
			}
			if degraded != false {
				t.Errorf("degraded = %v (banner %q), want false — a reply withheld by evaluation mode is the run's purpose", degraded, banner)
			}
			if !strings.HasPrefix(banner, "evaluation mode: ") || strings.Contains(banner, "reply OK") {
				t.Errorf("banner %q, want it to read evaluation mode and never OK", banner)
			}
		})
	}
}

// reviewPRPublishOnce runs publish_review once, rendered over refs, against
// an endpoint answering resp, and returns how many requests it received, the
// last body, and the node's publish_output.
func reviewPRPublishOnce(t *testing.T, refs map[string]string, resp map[string]any) (int, []byte, map[string]any) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits++
		sent = body
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	all := map[string]string{
		"vars.forge_publish_url":      srv.URL + "/api/v1/forge/publish-review",
		"vars.forge_publish_token":    "run-token",
		"input.pr_url":                "https://github.com/acme/widgets/pull/7",
		"input.effective_review_mode": "mono",
		"input.scope_files":           "2",
		"input.review_scope":          "a.go ; b.go",
		"input.claude_summary":        "Reviewed a.go and b.go",
		"vars.gate_enabled":           "true",
	}
	for k, v := range refs {
		all[k] = v
	}
	out, err := exec.Command("sh", "-c", renderReviewPRBodyHealthNode(t, "publish_review", all)).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	var published map[string]any
	if err := dec.Decode(&published); err != nil {
		t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	return hits, sent, published
}

// review-pr in evaluation mode: two anchored findings, a gate requested, a
// publish grant bound — and the forge-publish endpoint receives nothing, so
// neither the review nor the merge-gate status is posted. publish_output says
// what the request would have carried, the gate verdict included, asks
// publish_health for no gate, and flags the review as withheld; through the
// real edge publish_health reads evaluation mode, not degraded and never OK.
// Without the var the same fixture posts once, the gate in the request.
func TestReviewPRPublishReviewEvaluationModeSendsNothing(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	const findings = `[{"file":"a.go","line":1,"title":"first finding","severity":"high"},{"file":"b.go","line":9,"title":"second finding","severity":"medium"}]`
	answer := map[string]any{
		"published": true, "provider": "github", "review_url": "https://forge/review/7",
		"comments_posted": 2, "suggestions_posted": 0, "verified": true,
		"gate_posted": true, "gate_state": "failure", "gate_sha": "deadbeefcafe",
	}
	t.Run("publish_mode=off", func(t *testing.T) {
		hits, _, published := reviewPRPublishOnce(t, map[string]string{"input.findings": findings, "vars.publish_mode": "off"}, answer)
		if hits != 0 {
			t.Fatalf("the forge-publish endpoint received %d request(s), want none — no review, no merge-gate status", hits)
		}
		if published["published"] != false || published["evaluation"] != true || published["gate_requested"] != false {
			t.Fatalf("publish_output = %v, want published=false, evaluation=true, gate_requested=false", published)
		}
		skipped, _ := published["skipped_reason"].(string)
		for _, part := range []string{evalWithheldReason, "2 finding(s) in the review", "2 inline comment(s)", "revi/review would read failure (1 blocking at high)"} {
			if !strings.Contains(skipped, part) {
				t.Errorf("skipped_reason %q does not say %q", skipped, part)
			}
		}
		if got := published["comments_sent"]; got != json.Number("2") {
			t.Errorf("comments_sent = %v, want the 2 inline comments the withheld request carried", got)
		}
		degraded, banner := runReviewPRHealthOverEdge(t, published, "2")
		if degraded {
			t.Errorf("publish_health degraded: %q — a review withheld by evaluation mode is the run's purpose", banner)
		}
		if !strings.HasPrefix(banner, evalWithheldReason) {
			t.Errorf("banner %q does not read evaluation mode", banner)
		}
		for _, deny := range []string{"forge publish OK", "MERGE GATE NOT POSTED", "FAILED", "MISSING"} {
			if strings.Contains(banner, deny) {
				t.Errorf("banner %q must not contain %q", banner, deny)
			}
		}
	})
	t.Run("publish_mode=off without a publish grant", func(t *testing.T) {
		_, _, published := reviewPRPublishOnce(t, map[string]string{
			"input.findings": findings, "vars.publish_mode": "off",
			"vars.forge_publish_url": "", "vars.forge_publish_token": "",
		}, answer)
		skipped, _ := published["skipped_reason"].(string)
		if published["evaluation"] != true || !strings.HasPrefix(skipped, evalWithheldReason) {
			t.Errorf("publish_output = %v — evaluation mode sends nothing, so it needs no grant", published)
		}
	})
	t.Run("publish_mode unset", func(t *testing.T) {
		hits, sent, published := reviewPRPublishOnce(t, map[string]string{"input.findings": findings}, answer)
		var req struct {
			Comments []json.RawMessage `json:"comments"`
			Gate     *struct {
				Blocking int `json:"blocking_count"`
			} `json:"gate"`
		}
		if hits != 1 || json.Unmarshal(sent, &req) != nil || len(req.Comments) != 2 || req.Gate == nil || req.Gate.Blocking != 1 {
			t.Fatalf("%d request(s), body %q — want the review posted once with its 2 comments and the gate", hits, sent)
		}
		if published["published"] != true || published["evaluation"] != false || published["gate_requested"] != true {
			t.Errorf("publish_output = %v, want a published review with its gate requested", published)
		}
		if degraded, banner := runReviewPRHealthOverEdge(t, published, "2"); degraded || !strings.HasPrefix(banner, "forge publish OK") {
			t.Errorf("degraded = %v, banner %q — want the healthy publish", degraded, banner)
		}
	})
	// Evaluation mode never masks a refused review: a placeholder summary is
	// refused under its own banner, and the gate it would not have posted
	// anyway is not reported missing.
	t.Run("publish_mode=off over a placeholder review", func(t *testing.T) {
		hits, _, published := reviewPRPublishOnce(t, map[string]string{
			"input.findings": findings, "vars.publish_mode": "off", "input.claude_summary": "placeholder",
		}, answer)
		if hits != 0 || published["evaluation"] != false || published["gate_requested"] != false {
			t.Fatalf("%d request(s), publish_output %v — want the refusal, no request, no gate owed", hits, published)
		}
		degraded, banner := runReviewPRHealthOverEdge(t, published, "2")
		if !degraded || !strings.Contains(banner, "REVIEW REFUSED") || strings.Contains(banner, "MERGE GATE NOT POSTED") {
			t.Errorf("degraded = %v, banner %q — want REVIEW REFUSED and no gate alarm", degraded, banner)
		}
	})
}

// publish_mode=off silences a review or a reply, so only an operator's own
// launch (--var, bot_args, a remote launch) may set it. A forge webhook
// builds its launch from fixed payload keys (pkg/server's lane builders: the
// PR's url, branches, title and body, the comment) and then lays on what
// manifests declare — the union of every co-enabled bot's
// forge.webhook.launch_vars, an invocation's context_vars, the args_var a
// commenter's text lands in — and a board card adds its dispatch_vars. None
// of those manifest channels may carry the var, in ANY bot of the catalog: a
// launch_var one manifest declares reaches every bot co-enabled on the same
// webhook. On the two bots that read it, it is a declared "" | "off" var,
// publishing by default, and a hidden launch input, so no form offers it.
func TestPublishModeIsNeverAWebhookVar(t *testing.T) {
	const name = "publish_mode"
	manifests, err := filepath.Glob(filepath.Join(botsDir(t), "*", bundle.ManifestFile))
	if err != nil || len(manifests) < 20 {
		t.Fatalf("the catalog walk found %d manifests (%v)", len(manifests), err)
	}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		bot := filepath.Base(filepath.Dir(path))
		if m.Forge != nil && m.Forge.Webhook != nil {
			if _, ok := m.Forge.Webhook.LaunchVars[name]; ok {
				t.Errorf("%s: forge.webhook.launch_vars sets %s — every webhook launch of every co-enabled bot would carry it", bot, name)
			}
		}
		for i, inv := range m.Invocations {
			if inv.ArgsVar == name {
				t.Errorf("%s: invocations[%d].args_var is %s — a commenter's text would set it", bot, i, name)
			}
			if _, ok := inv.ContextVars[name]; ok {
				t.Errorf("%s: invocations[%d].context_vars sets %s — every launch through that trigger would carry it", bot, i, name)
			}
		}
		if _, ok := m.DispatchVars[name]; ok {
			t.Errorf("%s: dispatch_vars sets %s — a board card's launch would carry it", bot, name)
		}
	}
	for _, bot := range []string{"revi-converse", "review-pr"} {
		m, err := bundle.LoadManifest(filepath.Join(botsDir(t), bot, bundle.ManifestFile))
		if err != nil {
			t.Fatalf("%s: %v", bot, err)
		}
		if m.Launch == nil || !slices.Contains(m.Launch.Hidden, name) {
			t.Errorf("%s: %s is not a hidden launch input", bot, name)
		}
		u := unittest.Whole(unit.LoadDir(filepath.Join(bot, "main.bot")))
		if u.Merged == nil {
			t.Fatalf("%s: the bundle did not parse: %v", bot, u.Diagnostics)
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Fatalf("%s: the bundle did not compile: %v", bot, cr.Diagnostics)
		}
		v := cr.Workflow.Vars[name]
		if v == nil || v.Type != ir.VarString || !slices.Equal(v.EnumValues, []string{"", "off"}) || !v.HasDefault || v.Default != "" {
			t.Errorf("%s: %s = %+v, want a string var [enum: \"\", \"off\"] defaulting to \"\" (publish)", bot, name, v)
		}
	}
}
