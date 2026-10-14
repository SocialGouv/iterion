package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

type deliveryProbeExecutor struct {
	*model.ClawExecutor
	analysisCalls int
}

func (e *deliveryProbeExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	switch node.(type) {
	case *ir.AgentNode, *ir.RouterNode:
		e.analysisCalls++
		return nil, errors.New("analysis must not start before delivery permission is established")
	default:
		return e.ClawExecutor.Execute(ctx, node, input)
	}
}

// unanswered names the rows where NOBODY answered the question — no proof
// service, no token to ask about, a status the endpoint never answers verdicts
// with, a body that is not a verdict, a redirect, a dropped connection, a body
// cut mid-transfer. They end the run on the SERVICE
// (FORGE_PREFLIGHT_UNAVAILABLE: relaunch, nothing to approve), never on the
// credential (FORGE_PERMISSION_DENIED: a grant an operator must perform).
// Telling them apart is the whole point of the pair: an unattended lane reads
// the code, not the wording.
var unanswered = map[string]bool{
	"missing-proof": true, "missing-token": true, "server-error": true,
	"invalid-response": true, "redirect": true, "reset": true,
	"truncated": true, "garbage": true,
}

func TestFixerWorkflowDiffRefusesBeforeAnalysis(t *testing.T) {
	for _, name := range []string{"missing-proof", "missing-token", "denied", "allowed", "server-error", "invalid-response", "redirect", "reset", "truncated", "garbage", "ordinary-code", "workflow-reverted", "rename-out", "rename-in", "dirty-workflow", "untracked-workflow", "gitlab"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "bots", "branch-improve-loop", "main.bot")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed := parser.Parse(path, string(src))
			compiled := ir.Compile(parsed.File)
			if compiled.Workflow == nil {
				t.Fatalf("compile: %+v", compiled.Diagnostics)
			}
			wf := compiled.Workflow
			wf.Worktree = "none"
			ws := t.TempDir()
			gittest.Run(t, ws, "init", "-q", "-b", "main")
			workflow := ".github/workflows/ci.yml"
			if err := os.MkdirAll(filepath.Join(ws, ".github", "workflows"), 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(path string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(ws, path), []byte("name: ci\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if name == "rename-out" {
				write(workflow)
			}
			if name == "rename-in" {
				write("ordinary.yml")
			}
			gittest.Run(t, ws, "add", ".")
			gittest.Run(t, ws, "commit", "--allow-empty", "-qm", "base")
			gittest.Run(t, ws, "checkout", "-qb", "fix-ci")
			switch name {
			case "rename-out":
				gittest.Run(t, ws, "mv", workflow, "ordinary.yml")
			case "rename-in":
				gittest.Run(t, ws, "mv", "ordinary.yml", workflow)
			case "ordinary-code":
				write("ordinary.txt")
			default:
				write(workflow)
			}
			if name != "untracked-workflow" {
				gittest.Run(t, ws, "add", ".")
				if name != "dirty-workflow" {
					gittest.Run(t, ws, "commit", "-qm", "change")
				}
			}
			if name == "workflow-reverted" {
				gittest.Run(t, ws, "rm", workflow)
				gittest.Run(t, ws, "commit", "-qm", "revert workflow change")
			}
			origin, prURL := "https://github.com/example/project.git", "https://github.com/example/project/pull/1"
			if name == "gitlab" {
				origin, prURL = "https://gitlab.example/example/project.git", "https://gitlab.example/example/project/-/merge_requests/1"
			}
			gittest.Run(t, ws, "remote", "add", "origin", origin)
			token := "ghs_test_runtime_token"
			if name == "missing-token" {
				token = ""
			}
			// A GitLab connection mounts a GitLab credential: the exemption
			// is read from the token's kind, and an installation token on a
			// GitLab remote is a combination iterion cannot mint.
			if name == "gitlab" {
				token = "glpat-test_project_token"
			}
			// Bind an isolated file even for an absent token. Neither a standard
			// mount on the test host nor its credential environment may enter this run.
			tokenPath := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_CONFIG_DIR", t.TempDir())
			// Atomic: the handler goroutine writes them and the assertions
			// below read them without joining the server first.
			var calls, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer target.Close()
			check := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				sum := sha256.Sum256([]byte(token))
				if payload["credential_sha256"] != hex.EncodeToString(sum[:]) || payload["pr_url"] != prURL || r.Header.Get("X-Iterion-Run") != "scoped-grant" {
					t.Error("preflight did not bind the actual runtime token and PR to its run grant")
				}
				switch name {
				case "server-error":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "invalid-response":
					_, _ = w.Write([]byte(`{"ok":true}`))
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "reset":
					// The pod going down drops the request it holds.
					conn, _, herr := w.(http.Hijacker).Hijack()
					if herr != nil {
						t.Error(herr)
						return
					}
					_ = conn.Close()
				case "truncated":
					// The pod dies with the headers already out: a body
					// shorter than the Content-Length it promised. urllib
					// raises NOTHING on that — read(amt) returns a short read
					// — so only the framing says the answer is incomplete.
					conn, _, herr := w.(http.Hijacker).Hijack()
					if herr != nil {
						t.Error(herr)
						return
					}
					_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 300\r\n\r\n{\"ok\": false, \"code\": \"FORGE_PERM"))
					_ = conn.Close()
				case "garbage":
					_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": name == "allowed", "code": map[bool]string{true: "", false: "FORGE_PERMISSION_DENIED"}[name == "allowed"], "reason": "workflows:write on the mounted token"})
				}
			}))
			defer check.Close()
			checkURL := check.URL
			if name == "missing-proof" {
				checkURL = ""
			}
			guard := secretguard.New([]secretguard.Secret{{Name: "forge_token", FilePath: tokenPath}}, secretguard.DefaultConfig())
			executor := &deliveryProbeExecutor{ClawExecutor: model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))}
			t.Cleanup(func() { _ = executor.Close() })
			s := tmpStore(t)
			err = New(wf, s, executor, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "delivery-refusal", map[string]any{
				"workspace_dir": ws, "base_ref": "main", "plan_phase": "off", "pr_url": prURL,
				"forge_delivery_preflight_url": checkURL, "forge_publish_token": "scoped-grant",
			})
			run, loadErr := s.LoadRun(t.Context(), "delivery-refusal")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			allowed := name == "allowed" || name == "ordinary-code" || name == "gitlab"
			if allowed {
				// NEITHER delivery refusal may stop a mission whose delivery
				// was permitted or never needed the permission.
				if executor.analysisCalls != 1 || run.FailureCode == "FORGE_PERMISSION_DENIED" || run.FailureCode == "FORGE_PREFLIGHT_UNAVAILABLE" {
					t.Fatalf("valid mission blocked: %s %s", run.FailureCode, run.Error)
				}
			} else {
				wantCode := store.FailureCode("FORGE_PERMISSION_DENIED")
				if unanswered[name] {
					wantCode = "FORGE_PREFLIGHT_UNAVAILABLE"
				}
				if err == nil || run.FailureCode != wantCode || !strings.Contains(run.Error, "workflows") || executor.analysisCalls != 0 {
					t.Fatalf("failure=%s want %s, reason=%s analysis=%d err=%v", run.FailureCode, wantCode, run.Error, executor.analysisCalls, err)
				}
			}
			wantCall := name != "missing-proof" && name != "missing-token" && name != "ordinary-code" && name != "gitlab"
			wantCalls := map[bool]int{true: 1, false: 0}[wantCall]
			if unanswered[name] && wantCall {
				// No answer is asked again once, after the pause — a rolling
				// restart drops one request and serves the next. An ANSWER,
				// permitted or refused, is never asked twice.
				wantCalls = 2
			}
			if int(calls.Load()) != wantCalls || redirected.Load() != 0 {
				t.Fatalf("callback calls=%d want %d, redirected=%d", calls.Load(), wantCalls, redirected.Load())
			}
		})
	}
}
