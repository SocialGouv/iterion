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
	// Observed live on socialgouv/buildkit-operator#15: the alignment was a
	// no-op, yet the check displayed "alignment committed, build verified".
	// The two verdicts are both green and differ ONLY in what they claim was
	// done, so each must state its own case and neither may borrow the
	// other's. A required check that asserts work nobody did is the same
	// false-statement class this bot exists to catch in other people's diffs.
	t.Run("clean: claims no alignment, never a commit", func(t *testing.T) {
		got, _ := run(t, "clean", "iterion/review", true)
		if got.Gate == nil {
			t.Fatal("no gate payload")
		}
		if strings.Contains(got.Gate.Note, "alignment committed") {
			t.Errorf("check claims a commit on the no-alignment verdict: %q", got.Gate.Note)
		}
		if !strings.Contains(got.Gate.Note, "no alignment needed") {
			t.Errorf("note should say what actually happened, got %q", got.Gate.Note)
		}
		if strings.Contains(got.Summary, "Committed the alignment") ||
			strings.Contains(got.Summary, "code updated on this branch") {
			t.Errorf("PR comment claims a commit that never happened:\n%s", got.Summary)
		}
	})

	// iterion#400: `align` fixed an otel break, the run died on a usage window,
	// the retry re-cloned, and the fix was gone. The verdict must not read like
	// the bump needed nothing — the operator's next move is to re-run Vetty,
	// and nothing on the PR would have told them so.
	t.Run("hold_lost_alignment: names the missing work, and blocks", func(t *testing.T) {
		got, _ := run(t, "hold_lost_alignment", "iterion/review", true)
		if got.Gate == nil {
			t.Fatal("no gate payload")
		}
		if got.Gate.BlockingCount == 0 {
			t.Error("a bump whose alignment is missing must not be mergeable")
		}
		if strings.Contains(got.Gate.Note, "no alignment needed") {
			t.Errorf("the check reports a missing alignment as a bump that needed none: %q", got.Gate.Note)
		}
		if !strings.Contains(got.Gate.Note, "missing") {
			t.Errorf("note must say the alignment is missing, got %q", got.Gate.Note)
		}
		if !strings.Contains(got.Summary, "NOT on this branch") {
			t.Errorf("the comment must state the alignment is absent:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "Re-run Vetty") {
			t.Errorf("the comment must name the recovery, else the PR just sits red:\n%s", got.Summary)
		}
		// This case renders with an EMPTY align_summary, and `section()` emits
		// nothing for an empty body. So the verdict message must not point at a
		// section title to explain itself: a reader who cannot find what the
		// comment cites doubts the HOLD rather than acting on it.
		for _, title := range []string{"Code alignment", "Security & supply-chain", "Reliability"} {
			if strings.Contains(got.Summary, `"`+title+`"`) && !strings.Contains(got.Summary, "## "+title) {
				t.Errorf("the comment cites section %q, which it did not render:\n%s", title, got.Summary)
			}
		}
	})

	t.Run("committed: says so, in the badge and the check", func(t *testing.T) {
		got, _ := run(t, "committed", "iterion/review", true)
		if got.Gate == nil || !strings.Contains(got.Gate.Note, "alignment committed") {
			t.Errorf("a real alignment must be stated in the check, got %+v", got.Gate)
		}
		if !strings.Contains(got.Summary, "Committed the alignment") {
			t.Errorf("a real alignment must be stated in the comment:\n%s", got.Summary)
		}
	})

	for _, tc := range []struct {
		verdict     string
		wantBlocked bool
	}{
		{"clean", false},
		{"committed", false},
		{"hold_security", true},
		{"hold_unstable", true},
		{"needs_decision", true},
		{"hold_lost_alignment", true},
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
