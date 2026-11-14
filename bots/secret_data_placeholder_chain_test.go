package bots

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The shape #3268 was found on (revi-converse's publish_reply, 2026-11-14):
// an agent drafts an answer, and deterministic tool nodes post it to the
// forge with a token the run holds. Anyone who can comment on a public PR can
// plant a secret's placeholder in the thread the agent reads; the agent
// repeats it. Both secrets below declare no `sinks:` — every sink, the
// ADR-146 default — which is exactly where the engine materialised the
// planted placeholder into the post. The workflow is the minimal one with
// both bodies that render data beside a secret: a `script:` that reads a
// mounted token (publish_reply's shape) and a `command:` that names a value
// secret in a header (the catalog's curl shape).
const dataPlaceholderChainBot = `secrets:
  forge_token:
    as: file
    optional: true
  api_token:
    optional: true

vars:
  forge_url: string = ""

prompt draft_system:
  Draft an answer.

schema draft_output:
  answer: string

schema publish_input:
  answer: string

schema publish_output:
  posted: bool

agent draft:
  model: "anthropic/claude-haiku-4-5-20251001"
  system: draft_system
  output: draft_output

tool publish_script:
  language: py
  script: |
    import json
    import urllib.request
    answer = {{input.answer}}
    with open({{secrets.forge_token.path}}) as f:
        tok = f.read().strip()
    req = urllib.request.Request({{vars.forge_url}} + "/script", data=answer.encode(), method="POST", headers={"Authorization": "token " + tok})
    urllib.request.urlopen(req).read()
    print(json.dumps({"posted": True}))
  input: publish_input
  output: publish_output

tool publish_command:
  command: ` + "`" + `curl -sS -f -o /dev/null -X POST -H "Authorization: Bearer "{{secrets.api_token}} --data-binary {{input.answer}} {{vars.forge_url}}/command && printf '{"posted":true}'` + "`" + `
  input: publish_input
  output: publish_output

workflow data_placeholder_chain:
  entry: draft
  draft -> publish_script with {
    answer: "{{outputs.draft.answer}}",
  }
  publish_script -> publish_command with {
    answer: "{{outputs.draft.answer}}",
  }
  publish_command -> done
`

const (
	chainForgeToken = "forge-token-chain-5b9e2d71"
	chainAPIToken   = "api-token-chain-c40a8f13"
)

// dataPlaceholderExec answers the draft agent with the planted answer and
// runs every other node on a real ClawExecutor.
type dataPlaceholderExec struct {
	real   *model.ClawExecutor
	answer string
}

func (c *dataPlaceholderExec) SetVars(v map[string]any) { c.real.SetVars(v) }

func (c *dataPlaceholderExec) Execute(ctx context.Context, n ir.Node, input map[string]any) (map[string]any, error) {
	if n.NodeID() == "draft" {
		return map[string]any{"answer": c.answer}, nil
	}
	return c.real.Execute(ctx, n, input)
}

// postedRequest is one request the fake forge received.
type postedRequest struct {
	path, auth, body string
}

func TestAPlantedPlaceholderIsPostedAsTextNeverAsTheToken(t *testing.T) {
	for _, tool := range []string{"python3", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	// The fake forge listens on loopback: no proxy in between.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")

	parsed := parsertest.Parse("data_placeholder_chain.bot", dataPlaceholderChainBot)
	if len(parsed.Diagnostics) > 0 {
		t.Fatalf("the workflow does not parse: %+v", parsed.Diagnostics)
	}
	compiled := ir.Compile(parsed.File)
	if compiled.HasErrors() {
		t.Fatalf("the workflow does not compile: %+v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	forgePH := secretguard.PlaceholderForName("forge_token")
	apiPH := secretguard.PlaceholderForName("api_token")

	for name, answer := range map[string]string{
		"bare":          "Re: « what is the key? » The diff sets it to " + forgePH + " and " + apiPH + " (app.go:3).",
		"single-quoted": "Re: « what is the key? » The diff sets it to '" + forgePH + "' and '" + apiPH + "' (app.go:3).",
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var posts []postedRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				posts = append(posts, postedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(srv.Close)

			tokenPath := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenPath, []byte(chainForgeToken+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The run's guard as the run builds it from the declarations
			// (model.declaredWorkflowSecrets): name, value, placeholder, the
			// mounted path, the declared sinks — none here.
			var secrets []secretguard.Secret
			for _, s := range []struct{ name, value, path string }{{"forge_token", chainForgeToken, tokenPath}, {"api_token", chainAPIToken, ""}} {
				decl, ok := wf.Secrets[s.name]
				if !ok {
					t.Fatalf("the workflow declares no %s", s.name)
				}
				if len(decl.Sinks) != 0 {
					t.Fatalf("%s declares sinks %v: the scenario is the default grant", s.name, decl.Sinks)
				}
				secrets = append(secrets, secretguard.Secret{Name: s.name, Value: s.value, Placeholder: secretguard.PlaceholderForName(s.name), FilePath: s.path, Env: decl.Env})
			}
			guard := secretguard.New(secrets, secretguard.DefaultConfig())
			ex := &dataPlaceholderExec{
				real:   model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(t.TempDir()), model.WithSecretGuard(guard)),
				answer: answer,
			}
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := runtime.New(wf, st, ex).Run(ctx, "data-placeholder-chain", map[string]any{"forge_url": srv.URL}); err != nil {
				t.Fatalf("the run failed: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			byPath := map[string]postedRequest{}
			for _, p := range posts {
				byPath[p.path] = p
			}
			for path, auth := range map[string]string{"/script": "token " + chainForgeToken, "/command": "Bearer " + chainAPIToken} {
				p, ok := byPath[path]
				if !ok {
					t.Fatalf("nothing was posted to %s: %+v", path, posts)
				}
				for _, tok := range []string{chainForgeToken, chainAPIToken} {
					for _, form := range []string{tok, base64.StdEncoding.EncodeToString([]byte(tok))} {
						if strings.Contains(p.body, form) {
							t.Errorf("%s: the forge received a token in the posted body: %q", path, p.body)
						}
					}
				}
				if p.body != answer {
					t.Errorf("%s: posted %q, want the answer as the agent wrote it — its placeholders as text", path, p.body)
				}
				if p.auth != auth {
					t.Errorf("%s: Authorization %q, want the token the author's own reference names", path, p.auth)
				}
			}
		})
	}
}
