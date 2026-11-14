package bots

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The chain as the engine runs it: the real engine walks revi-converse's
// edges; converse_agent is answered with a draft in the shape of its real
// schema, and publish_reply and converse_health run on a real ClawExecutor
// under the run's own secret guard — model.BuildSecretGuard over the
// compiled bundle, the token handed in as the run's resolved credential —
// which the executor materialises on the host as an unsandboxed run does.

// converseChainExec answers converse_agent with a draft and runs every other
// node on a real ClawExecutor.
type converseChainExec struct {
	real  *model.ClawExecutor
	agent map[string]any

	mu   sync.Mutex
	outs map[string]map[string]any
}

func (c *converseChainExec) SetVars(v map[string]any) { c.real.SetVars(v) }

func (c *converseChainExec) Execute(ctx context.Context, n ir.Node, input map[string]any) (map[string]any, error) {
	var out map[string]any
	var err error
	if n.NodeID() == "converse_agent" {
		out = map[string]any{}
		for k, v := range c.agent {
			out[k] = v
		}
	} else {
		out, err = c.real.Execute(ctx, n, input)
	}
	c.mu.Lock()
	if c.outs == nil {
		c.outs = map[string]map[string]any{}
	}
	c.outs[n.NodeID()] = out
	c.mu.Unlock()
	return out, err
}

func (c *converseChainExec) output(id string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outs[id]
}

// reviConverseGuard is the run's secret guard, built by the production
// builder from the compiled bundle: the forge_token binding arrives as the
// run's resolved credential (held=false: a run without the binding).
func reviConverseGuard(t *testing.T, wf *ir.Workflow, held bool) *secretguard.Guard {
	t.Helper()
	if _, ok := wf.Secrets["forge_token"]; !ok {
		t.Fatal("revi-converse declares no forge_token secret")
	}
	creds := secrets.Credentials{Generic: map[string]string{}}
	if held {
		creds.Generic["forge_token"] = publishToken
	}
	return model.BuildSecretGuard(secrets.WithCredentials(context.Background(), creds), wf, nil, nil)
}

// The run's guard keeps forge_token off every materialiser: the bundle
// grants it the `file` sink only, so its placeholder resolves neither in an
// authored body (the `command` sink: a tool command, a script) nor in a
// model's tool input (`agent`), and one arriving in data is defused — while
// `{{secrets.forge_token.path}}` still renders a path. Built by the
// production builder, so a builder that dropped the declared grants would
// fail here, not only in a hand-built guard.
func TestReviConverseRunGuardKeepsTheTokenOffEveryMaterialiser(t *testing.T) {
	wf := reviConverseWorkflow(t)
	guard := reviConverseGuard(t, wf, true)
	text := "key " + tokenPlaceholder + " and '" + tokenPlaceholder + "'"
	for name, got := range map[string]string{
		"a script or command body (MaterializeShell, command sink)": guard.MaterializeShell(text),
		"a model's tool input (Materialize, agent sink)":            guard.Materialize(text),
	} {
		if strings.Contains(got, publishToken) {
			t.Errorf("%s resolves forge_token to its value: %q", name, got)
		}
	}
	authored, env := guard.MaterializeAuthoredShellEnv(text)
	if strings.Contains(authored, publishToken) || len(env) != 0 {
		t.Errorf("an authored command resolves forge_token: %q, env %v", authored, env)
	}
	if got := guard.DefuseUngranted(secretsink.Agent, text); strings.Contains(got, tokenPlaceholder) || !strings.Contains(got, tokenDefused) {
		t.Errorf("a placeholder in data is not defused: %q", got)
	}
	if path := guard.ResolveSecretRef("forge_token"); path == "" || strings.Contains(path, "__ITERION_SECRET_") {
		t.Errorf("{{secrets.forge_token.path}} renders %q, want the mounted path", path)
	}
}

// chainRun is one run of the chain against a fake GitLab discussion.
type chainRun struct {
	forge  *converseFakeForge
	pr     string
	exec   *converseChainExec
	runErr error
}

func (r chainRun) health(t *testing.T) (degraded any, banner string) {
	t.Helper()
	h := r.exec.output("converse_health")
	if h == nil {
		t.Fatalf("converse_health never ran (run error %v, publish_reply %v)", r.runErr, r.exec.output("publish_reply"))
	}
	b, _ := h["banner"].(string)
	return h["degraded"], b
}

// chainEnv clears the proxies (the fake forge is on loopback) and gives the
// run a public origin, so {{run.url}} resolves as on a deployment.
func chainEnv(t *testing.T) {
	t.Helper()
	requirePython(t)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv(deeplink.PublicBaseEnv, "https://iterion.example")
}

const chainRunID = "chain-run-1"

func runChain(t *testing.T, wf *ir.Workflow, agent map[string]any, held bool, setup func(*converseFakeForge)) chainRun {
	t.Helper()
	f := newConverseFakeForge(t)
	if setup != nil {
		setup(f)
	}
	pr := f.srv.URL + "/g/p/-/merge_requests/7"
	ex := &converseChainExec{
		real:  model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(t.TempDir()), model.WithSecretGuard(reviConverseGuard(t, wf, held))),
		agent: agent,
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"workspace_dir": t.TempDir(), "base_ref": "main", "pr_url": pr, "discussion_id": "6a1f0c",
		"trigger_note": "/revi why does A return 2?", "converse_question": "why does A return 2?",
		"thread_context": "", "replier": "op",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runErr := runtime.New(wf, st, ex).Run(ctx, chainRunID, vars)
	return chainRun{forge: f, pr: pr, exec: ex, runErr: runErr}
}

func chainDraft(t *testing.T, wf *ir.Workflow, answer, skipped string) map[string]any {
	t.Helper()
	var fields []string
	for _, f := range wf.Schemas["converse_output"].Fields {
		fields = append(fields, f.Name)
	}
	if strings.Join(fields, ",") != "answer,forge,skipped_reason,summary" {
		t.Fatalf("converse_output fields = %v, want [answer forge skipped_reason summary] — the agent drafts; posting is publish_reply's", fields)
	}
	return map[string]any{"answer": answer, "forge": "gitlab", "skipped_reason": skipped, "summary": "Question: why does A return 2?"}
}

func chainFooter() string {
	return "\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`" + chainRunID + "`](" + deeplink.Run("https://iterion.example", chainRunID) + ")"
}

// The agent's draft crosses the edge into publish_reply, which posts it with
// the token the run's guard mounts and the run's own link; its result
// crosses the edge into converse_health, which reads OK on a posted reply
// and degraded on every way it can fall short. A draft beside a filler
// skipped_reason is posted; a NUL in the draft and a skipped_reason past an
// environment entry still end in converse_health — the run informs, it
// never fails on its publish step.
func TestReviConverseChainPostsTheDraftAndGatesIt(t *testing.T) {
	chainEnv(t)
	wf := reviConverseWorkflow(t)
	huge := strings.Repeat("the diff could not be read; ", 5000)
	for _, c := range []struct {
		name         string
		answer       string
		skipped      string
		noToken      bool
		refuse       int
		missRead     bool
		wantWrites   int
		wantBody     string
		wantDegraded bool
		wantBanner   string
	}{
		{name: "posted", answer: publishAnswer, wantWrites: 1, wantBody: forge.InertGitLabQuickActions(publishAnswer), wantBanner: "forge reply OK: "},
		{name: "a filler skipped_reason beside the draft", answer: publishAnswer, skipped: "N/A", wantWrites: 1, wantBody: forge.InertGitLabQuickActions(publishAnswer), wantBanner: "forge reply OK: "},
		{name: "a NUL in the draft", answer: "Re: op\n\nA NUL\x00 here (app.go:3).", wantWrites: 1, wantBody: "Re: op\n\nA NUL here (app.go:3).", wantBanner: "forge reply OK: "},
		{name: "the agent skipped", skipped: "no question in the note", wantDegraded: true, wantBanner: "forge reply skipped: no question in the note"},
		{name: "a skipped_reason past an environment entry", skipped: huge, wantDegraded: true, wantBanner: "forge reply skipped: the diff could not be read"},
		{name: "a NUL in the skipped_reason", skipped: "no question\x00 in the note", wantDegraded: true, wantBanner: "forge reply skipped: no question in the note"},
		{name: "a filler draft beside a reason", answer: "N/A", skipped: "the diff could not be read", wantDegraded: true, wantBanner: "forge reply skipped: the diff could not be read"},
		{name: "an empty draft", wantDegraded: true, wantBanner: "forge reply skipped: the agent returned an empty answer"},
		{name: "no token", answer: publishAnswer, noToken: true, wantDegraded: true, wantBanner: "forge reply skipped: forge_token is not mounted"},
		{name: "the forge refuses", answer: publishAnswer, refuse: http.StatusForbidden, wantWrites: 1, wantBody: forge.InertGitLabQuickActions(publishAnswer), wantDegraded: true, wantBanner: "HTTP 403"},
		{name: "the re-read misses", answer: publishAnswer, missRead: true, wantWrites: 1, wantBody: forge.InertGitLabQuickActions(publishAnswer), wantDegraded: true, wantBanner: "re-reading it failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := runChain(t, wf, chainDraft(t, wf, c.answer, c.skipped), !c.noToken, func(f *converseFakeForge) {
				f.postStatus, f.postAnswer = c.refuse, `{"message":"403 Forbidden"}`
				if c.missRead {
					f.readStatus = http.StatusNotFound
				}
			})
			if r.runErr != nil {
				t.Fatalf("the run failed: %v — publish_reply and converse_health inform, they never fail it", r.runErr)
			}
			degraded, banner := r.health(t)
			if degraded != c.wantDegraded {
				t.Errorf("degraded = %v, want %v (banner %q, publish_reply %v)", degraded, c.wantDegraded, banner, r.exec.output("publish_reply"))
			}
			if !strings.Contains(banner, c.wantBanner) {
				t.Errorf("banner %.200q does not name %q", banner, c.wantBanner)
			}
			w := writes(r.forge.seen())
			if len(w) != c.wantWrites {
				t.Fatalf("the forge saw %d writes, want %d: %+v", len(w), c.wantWrites, w)
			}
			if c.wantWrites == 0 {
				return
			}
			if w[0].Auth != "Bearer "+publishToken {
				t.Errorf("the POST authenticated with %q — the guard's mounted token did not reach it", w[0].Auth)
			}
			if body, want := postedBody(t, w[0]), c.wantBody+chainFooter(); body != want {
				t.Errorf("the posted body is not the agent's draft, guarded, then this run's link:\n%q\nwant\n%q", body, want)
			}
			if !c.wantDegraded {
				if want := "forge reply OK: " + r.pr + "#note_9001"; banner != want {
					t.Errorf("banner = %q, want %q", banner, want)
				}
			}
		})
	}
}

// plantedAnswers quote the forge token's placeholder the way a commenter
// plants it in a public thread or a diff, and the agent repeats it.
var plantedAnswers = map[string]string{
	"bare":          "Re: op — « what is the key? »\n\nThe diff sets the key to " + tokenPlaceholder + " (app.go:3).",
	"single-quoted": "Re: op — « what is the key? »\n\nThe diff sets the key to '" + tokenPlaceholder + "' (app.go:3).",
}

// The guarantee: whatever the draft quotes, the forge token never reaches
// the forge — in a body, a path or a query, raw or encoded. Two guards hold
// it, each enough alone: the declaration's sinks (the engine defuses the
// placeholder before the body runs) and the body's own refusal.
func TestReviConverseChainNeverPostsTheForgeToken(t *testing.T) {
	chainEnv(t)
	wf := reviConverseWorkflow(t)
	for name, answer := range plantedAnswers {
		t.Run(name, func(t *testing.T) {
			r := runChain(t, wf, chainDraft(t, wf, answer, ""), true, nil)
			if r.runErr != nil {
				t.Fatalf("the run failed: %v", r.runErr)
			}
			for _, h := range r.forge.seen() {
				seen := h.Path + "?" + h.Query + "\n" + h.Body
				leaked := strings.Contains(squash(seen), squash(publishToken))
				for _, form := range tokenForms(publishToken) {
					leaked = leaked || strings.Contains(seen, form)
				}
				if leaked {
					t.Fatalf("the forge token reached the forge in %s %s: %q", h.Method, h.Path, h.Body)
				}
			}
		})
	}
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// The engine's guard, alone: forge_token is granted the `file` sink only, so
// a placeholder the draft carries is defused before publish_reply's body
// runs — the body receives `…_redacted__`, never the value. Read from what
// the body reports: a refusal naming the placeholder (not the token), or, if
// it posted, a body holding the defused form.
func TestReviConverseChainDefusesAPlantedTokenPlaceholder(t *testing.T) {
	chainEnv(t)
	wf := reviConverseWorkflow(t)
	for name, answer := range plantedAnswers {
		t.Run(name, func(t *testing.T) {
			r := runChain(t, wf, chainDraft(t, wf, answer, ""), true, nil)
			if r.runErr != nil {
				t.Fatalf("the run failed: %v", r.runErr)
			}
			pub := r.exec.output("publish_reply")
			if w := writes(r.forge.seen()); len(w) > 0 {
				if body := postedBody(t, w[0]); !strings.Contains(body, tokenDefused) {
					t.Errorf("publish_reply posted %q, which does not hold the defused placeholder — the engine did not defuse it", body)
				}
				return
			}
			reason, _ := pub["skipped_reason"].(string)
			if !strings.Contains(reason, "the answer carries the forge token's placeholder;") {
				t.Errorf("publish_reply's reason %q: the body received the token itself, not its defused placeholder — the declaration's sinks let the engine materialise it", reason)
			}
		})
	}
}

// The design, both guards on: an answer quoting the token's placeholder is
// not posted at all — the forge receives no request — and the run reads
// degraded, saying why.
func TestReviConverseChainRefusesAnAnswerQuotingTheTokenPlaceholder(t *testing.T) {
	chainEnv(t)
	wf := reviConverseWorkflow(t)
	for name, answer := range plantedAnswers {
		t.Run(name, func(t *testing.T) {
			r := runChain(t, wf, chainDraft(t, wf, answer, ""), true, nil)
			if r.runErr != nil {
				t.Fatalf("the run failed: %v", r.runErr)
			}
			if hits := r.forge.seen(); len(hits) != 0 {
				t.Errorf("the forge saw %d requests, want none: %+v", len(hits), hits)
			}
			degraded, banner := r.health(t)
			if degraded != true || !strings.Contains(banner, "nothing was posted") {
				t.Errorf("degraded = %v, banner %q — want degraded, saying nothing was posted", degraded, banner)
			}
		})
	}
}
