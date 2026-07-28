package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestDepUpdateGuardGateVerdict pins the merge gate Vetty posts. The gate is
// the thing a repo can make a REQUIRED check, so two properties matter more
// than anything else in the bot:
//
//   - it is a TABLE over the verdict the graph already computed, never a
//     judgment made at publish time — otherwise the deterministic gates
//     upstream are decorative;
//   - it fails CLOSED. A gate that reports success on a state nobody
//     anticipated is worse than no gate: it converts an unknown into a merge.
//
// The test runs the real script against a stub of the server's publish
// endpoint and asserts the payload that actually goes over the wire.
func TestDepUpdateGuardGateVerdict(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := toolScript(t, "dep-update-guard/main.bot", "post_feedback")

	type gate struct {
		Enabled       bool   `json:"enabled"`
		Context       string `json:"context"`
		BlockingCount int    `json:"blocking_count"`
		Note          string `json:"note"`
	}
	type published struct {
		PRURL   string `json:"pr_url"`
		Summary string `json:"summary"`
		Gate    *gate  `json:"gate"`
	}

	run := func(t *testing.T, verdict, gateContext string, gateEnabled bool) (published, map[string]any) {
		t.Helper()
		var got published
		var seen bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The server registers exactly ONE path, and it is the only one
			// exempt from the auth middleware. A stub answering any path let a
			// bot post to a URL that production rejects with 401.
			if r.URL.Path != "/api/v1/forge/publish-review" {
				t.Errorf("published to %q, want the endpoint path — anything else hits the auth middleware", r.URL.Path)
				w.WriteHeader(404)
				return
			}
			seen = true
			if tok := r.Header.Get("X-Iterion-Run"); tok != "run-token" {
				t.Errorf("publish must authenticate with the run grant, got %q", tok)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode publish payload: %v", err)
			}
			state := "success"
			if got.Gate != nil && got.Gate.BlockingCount > 0 {
				state = "failure"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"published": true, "review_url": "https://forge/review/1",
				"gate_posted": got.Gate != nil, "gate_state": state,
			})
		}))
		defer srv.Close()

		body := script
		for ref, val := range map[string]string{
			"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":            `"` + verdict + `"`,
			"{{input.audit_summary}}":      `"audited"`,
			"{{input.cves}}":               `""`,
			"{{input.malware_signals}}":    `""`,
			"{{input.align_summary}}":      `""`,
			"{{input.validate_summary}}":   `""`,
			"{{input.escalation}}":         `""`,
			"{{input.commit_summary}}":     `""`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `"` + srv.URL + `/api/v1/forge/publish-review"`,
			"{{vars.forge_publish_token}}": `"run-token"`,
			"{{vars.gate_enabled}}":        boolLit(gateEnabled),
			"{{vars.gate_context}}":        `"` + gateContext + `"`,
		} {
			body = strings.ReplaceAll(body, ref, val)
		}
		if strings.Contains(body, "{{") {
			t.Fatalf("unsubstituted ref left in the script: %s", firstRef(body))
		}

		f, err := os.CreateTemp(t.TempDir(), "feedback-*.py")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command("python3", f.Name()).Output()
		if err != nil {
			t.Fatalf("post_feedback failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("post_feedback output is not feedback_output JSON: %v (%q)", err, out)
		}
		if gateEnabled && !seen {
			t.Fatal("the gate can only ride the server publish endpoint — a run token cannot post a status")
		}
		return got, res
	}

	for _, tc := range []struct {
		verdict     string
		wantBlocked bool
	}{
		{"clean", false},
		{"committed", false},
		{"hold_security", true},
		{"hold_unstable", true},
		{"needs_decision", true},
		// Not a verdict the graph can produce today. If one ever appears, the
		// gate must refuse it rather than wave it through.
		{"probably_fine", true},
		{"", true},
	} {
		t.Run("verdict="+tc.verdict, func(t *testing.T) {
			pub, res := run(t, tc.verdict, "iterion/review", true)
			if pub.Gate == nil {
				t.Fatal("no gate in the publish payload")
			}
			blocked := pub.Gate.BlockingCount > 0
			if blocked != tc.wantBlocked {
				t.Errorf("verdict %q: blocking_count=%d, want blocked=%v", tc.verdict, pub.Gate.BlockingCount, tc.wantBlocked)
			}
			if pub.Gate.Context != "iterion/review" {
				t.Errorf("gate context = %q, want the pinned shared name", pub.Gate.Context)
			}
			if res["gate_posted"] != true {
				t.Errorf("gate_posted must report what the server did: %v", res)
			}
			wantState := "success"
			if tc.wantBlocked {
				wantState = "failure"
			}
			if res["gate_state"] != wantState {
				t.Errorf("gate_state = %v, want %s", res["gate_state"], wantState)
			}
		})
	}

	t.Run("unknown verdict says why it blocked", func(t *testing.T) {
		pub, _ := run(t, "probably_fine", "iterion/review", true)
		if !strings.Contains(pub.Gate.Note, "fails closed") {
			t.Errorf("an operator must be able to tell an unrecognised verdict from a real finding, got note %q", pub.Gate.Note)
		}
	})

	// A redirect turns urllib's POST into a GET, which misses the POST-only
	// route and is answered by the auth middleware — a 401 that looks like a
	// bad token. Refuse to follow, and name the redirect.
	t.Run("a redirect is reported, not followed into a misleading 401", func(t *testing.T) {
		var hits int
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			if r.Method != "POST" {
				t.Errorf("the endpoint must never be reached by %s — a redirect degraded the method", r.Method)
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		defer redir.Close()

		body := script
		for ref, val := range map[string]string{
			"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":            `"committed"`,
			"{{input.audit_summary}}":      `"a"`,
			"{{input.cves}}":               `""`,
			"{{input.malware_signals}}":    `""`,
			"{{input.align_summary}}":      `""`,
			"{{input.validate_summary}}":   `""`,
			"{{input.escalation}}":         `""`,
			"{{input.commit_summary}}":     `""`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `"` + redir.URL + `/api/v1/forge/publish-review"`,
			"{{vars.forge_publish_token}}": `"run-token"`,
			"{{vars.gate_enabled}}":        `True`,
			"{{vars.gate_context}}":        `"iterion/review"`,
		} {
			body = strings.ReplaceAll(body, ref, val)
		}
		f, err := os.CreateTemp(t.TempDir(), "feedback-*.py")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command("python3", f.Name()).Output()
		if err != nil {
			t.Fatalf("post_feedback failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("not JSON: %v (%q)", err, out)
		}
		reason, _ := res["gate_skipped_reason"].(string)
		if !strings.Contains(reason, "redirect") {
			t.Errorf("the reason must name the redirect, got %q", reason)
		}
		if !strings.Contains(reason, "/api/v1/forge/publish-review") {
			t.Errorf("the reason must name the URL actually called, got %q", reason)
		}
		if hits != 1 {
			t.Errorf("the redirect must not be followed, got %d request(s)", hits)
		}
	})

	t.Run("gate disabled publishes the review without a status", func(t *testing.T) {
		pub, res := run(t, "committed", "iterion/review", false)
		if pub.Gate != nil {
			t.Errorf("gate_enabled=false must post no status, got %+v", pub.Gate)
		}
		if res["posted"] != true {
			t.Errorf("the verdict must still reach the PR: %v", res)
		}
	})

	// No publish grant (a local CLI run) → the verdict still reaches the PR
	// through the direct forge comment, and the missing gate is REPORTED
	// rather than silently absent.
	t.Run("no publish grant falls back and says so", func(t *testing.T) {
		body := script
		for ref, val := range map[string]string{
			"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
			"{{input.verdict}}":            `"committed"`,
			"{{input.audit_summary}}":      `"audited"`,
			"{{input.cves}}":               `""`,
			"{{input.malware_signals}}":    `""`,
			"{{input.align_summary}}":      `""`,
			"{{input.validate_summary}}":   `""`,
			"{{input.escalation}}":         `""`,
			"{{input.commit_summary}}":     `""`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `""`,
			"{{vars.forge_publish_token}}": `""`,
			"{{vars.gate_enabled}}":        `True`,
			"{{vars.gate_context}}":        `"iterion/review"`,
		} {
			body = strings.ReplaceAll(body, ref, val)
		}
		f, err := os.CreateTemp(t.TempDir(), "feedback-*.py")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command("python3", f.Name()).Output()
		if err != nil {
			t.Fatalf("post_feedback failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("not JSON: %v (%q)", err, out)
		}
		if res["gate_posted"] != false {
			t.Errorf("no grant → no gate, got %v", res)
		}
		if reason, _ := res["gate_skipped_reason"].(string); reason == "" {
			t.Errorf("a missing gate must say why — a required check that is absent blocks the PR: %v", res)
		}
	})
}

func boolLit(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func firstRef(s string) string {
	i := strings.Index(s, "{{")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "}}")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+2]
}

// toolScript extracts a tool node's `script:` body (language: py) from a
// compiled bundle, the counterpart of toolCommand for script-form nodes.
func toolScript(t *testing.T, rel, node string) string {
	t.Helper()
	src, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	pr := parser.Parse(rel, string(src))
	if pr.File == nil {
		t.Fatalf("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile produced no Workflow")
	}
	raw, ok := cr.Workflow.Nodes[node]
	if !ok {
		t.Fatalf("no %s node", node)
	}
	tn, ok := raw.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is %T, want *ir.ToolNode", node, raw)
	}
	if strings.TrimSpace(tn.Script) == "" {
		t.Fatalf("%s has no script body", node)
	}
	return tn.Script
}
