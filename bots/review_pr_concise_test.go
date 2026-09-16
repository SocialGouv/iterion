package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Exercise the actual publisher: concise prose must not suppress findings,
// lose replacements when anchors are missing, or turn a degraded gate green.
func TestReviewPRConcisePublication(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	finding := `{"file":"a.go","line":12,"severity":"high","title":"Lost update","detail":"Concurrent saves overwrite a newer value.","suggestion":"Use a compare-and-swap.","replacement":"saveIfCurrent(v)","reviewers":"gpt"}`
	// The id the bot derives for that finding, computed the same way the
	// engine does (goFindingID, pinned against the bot in
	// review_pr_finding_id_test.go).
	dupID := goFindingID("a.go", "Lost update")
	otherFinding := `{"file":"b.go","line":7,"severity":"medium","title":"Unbounded retry","detail":"Retries never stop.","suggestion":"Cap them.","reviewers":"gpt"}`
	for _, tc := range []struct {
		name                    string
		refs                    map[string]string
		comments, blocking      int
		visible, hidden, absent []string
	}{
		{name: "clean review is one sentence with scope inside collapsed details", refs: map[string]string{
			"input.findings": "[]", "input.ticket_conformance": "#1172: covered — all criteria met\n(no ticket refs): unverifiable — no linked issues", "input.review_scope": "correctness: a.go; tests: a_test.go <details open>",
		}, visible: []string{"**Revi — aucun problème détecté dans le périmètre revu.**"}, hidden: []string{"correctness: a.go; tests: a_test.go &lt;details open&gt;", "(no ticket refs): unverifiable — no linked issues"}, absent: []string{"all criteria met", "raised no open questions", "<details open>"}},
		{name: "inline finding is not repeated in the summary", refs: map[string]string{"input.findings": "[" + finding + "]"}, comments: 1, blocking: 1,
			visible: []string{"**Revi — 1 problème à corriger.**", "1 high"}, absent: []string{"Lost update", "Concurrent saves", "Use a compare-and-swap", "saveIfCurrent"}},
		// A fixer escalation is the repo's to declare: the reviewer cannot
		// know which repository it is reviewing. Undeclared, the gate must
		// not instruct a developer to invoke a fixer that is paused or does
		// not exist — and it instructs at the exact moment the findings are
		// read, which is why the wrong default is expensive.
		{name: "a repo that declares no fixer is not told to invoke one", refs: map[string]string{"input.findings": "[" + finding + "]"}, comments: 1, blocking: 1,
			hidden: []string{"Identifiants des findings : R"}, absent: []string{"Correction :", "/billy"}},
		// Two findings with DIFFERENT ids, so "the first finding's id" is a
		// claim the case can refute rather than a coincidence of one.
		{name: "the fixer hint a repo declares is published with its findings", comments: 2, blocking: 1,
			refs:   map[string]string{"input.findings": "[" + finding + "," + otherFinding + "]", "vars.fixer_hint": "Correction : /billy ; arbitrage : /billy skip {finding} suivi du motif."},
			hidden: []string{"arbitrage : /billy skip " + dupID + " suivi"}, absent: []string{"{finding}"}},
		// The hint is operator prose landing in a public comment body.
		// Unescaped, an honest `<motif>` is eaten as a tag, and a stray
		// `</details>` would lift the rest of the block out of its fold.
		{name: "an angle-bracketed word in the hint reaches the reader", comments: 1, blocking: 1,
			refs:   map[string]string{"input.findings": "[" + finding + "]", "vars.fixer_hint": "Arbitrer : /billy skip {finding} <motif>"},
			hidden: []string{"&lt;motif&gt;"}, absent: []string{"<motif>"}},
		// The escaping is narrow on purpose. A character reference inside a
		// code span is literal text in CommonMark, so escaping quotes would
		// publish `&quot;` where the operator wrote `"` — in the one place a
		// hint is most likely to carry a command meant to be copied.
		{name: "a command in the hint stays copy-pasteable", comments: 1, blocking: 1,
			refs:   map[string]string{"input.findings": "[" + finding + "]", "vars.fixer_hint": "Lancer `/billy skip {finding} \"motif\"`"},
			hidden: []string{"`/billy skip " + dupID + " \"motif\"`"}, absent: []string{"&quot;", "&#34;"}},
		// Two findings on the same file and title derive the SAME id, so a list
		// that repeats it reads as two handles to arbitrate instead of one.
		// The id is DERIVED here, not spelled: a literal guessed wrong makes
		// this case pass while asserting nothing.
		{name: "the ids line names each finding once", comments: 2, blocking: 2,
			refs:   map[string]string{"input.findings": "[" + finding + "," + finding + "]"},
			hidden: []string{"Identifiants des findings : " + dupID},
			absent: []string{dupID + ", " + dupID}},
		{name: "only ticket gaps and real questions remain visible", refs: map[string]string{
			"input.findings": "[" + finding + "]", "input.ticket_conformance": "#1: covered — met\n#2: partial — missing retries\n#3: unverifiable — HTTP 403\nmalformed verdict: unknown",
			"input.questions": "Should retries preserve request order?\nShould retries preserve request order?",
		}, comments: 1, blocking: 1, visible: []string{"#2: partial — missing retries", "malformed verdict: unknown", "Should retries preserve request order?"}, hidden: []string{"#3: unverifiable — HTTP 403"}, absent: []string{"#1: covered — met"}},
		{name: "missing anchors preserve all finding content and replacement", refs: map[string]string{"input.findings": "[" + strings.Replace(finding, `"line":12`, `"line":0`, 1) + "]"}, blocking: 1,
			visible: []string{"Findings sans ancrage", "Lost update", "Concurrent saves", "Use a compare-and-swap", "saveIfCurrent(v)"}},
		{name: "unreadable output cannot look clean", refs: map[string]string{"input.findings": "not-json", "input.claude_findings": "[]", "input.gpt_findings": "[]"}, blocking: 1,
			visible: []string{"revue inexploitable", "gate bloquée"}, absent: []string{"aucun problème détecté", "/revi", "/billy"}},
		{name: "recovery stays explicitly degraded", refs: map[string]string{"input.findings": "not-json", "input.gpt_findings": "[" + finding + "]"}, comments: 1, blocking: 1,
			visible: []string{"Revue dégradée", "seuil, limite et accord entre modèles non appliqués"}, absent: []string{"/revi", "/billy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Summary  string           `json:"summary"`
				Comments []map[string]any `json:"comments"`
				Gate     struct {
					Blocking int `json:"blocking_count"`
				} `json:"gate"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode: %v", err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "comments_posted": len(got.Comments)})
			}))
			defer srv.Close()
			// scope_files is what says the review had a diff to read at all: an
			// absent or unparseable count fails the gate closed, so every case
			// here — each of which describes a review that DID read code — has
			// to carry a real one or it would block for the wrong reason.
			refs := map[string]string{"vars.forge_publish_url": srv.URL, "vars.forge_publish_token": "test", "input.pr_url": "https://github.com/acme/repo/pull/1", "input.effective_review_mode": "mono", "vars.gate_enabled": "true", "vars.gate_severity": "high", "input.scope_files": "3"}
			for k, v := range tc.refs {
				refs[k] = v
			}
			command := regexp.MustCompile(`\{\{([^}]+)\}\}`).ReplaceAllStringFunc(toolCommand(t, "review-pr/main.bot", "publish_review"), func(ref string) string { return "'" + strings.ReplaceAll(refs[ref[2:len(ref)-2]], "'", `'\''`) + "'" })
			if out, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
				t.Fatalf("publisher: %v\n%s", err, out)
			}
			if len(got.Comments) != tc.comments || got.Gate.Blocking != tc.blocking {
				t.Fatalf("comments/gate: %+v", got)
			}
			visible, details, found := strings.Cut(got.Summary, "<details>")
			if !found {
				t.Fatal("missing collapsed details")
			}
			if tc.name == "clean review is one sentence with scope inside collapsed details" && strings.TrimSpace(visible) != tc.visible[0] {
				t.Errorf("clean summary is verbose: %s", visible)
			}
			for _, v := range tc.visible {
				if !strings.Contains(visible, v) {
					t.Errorf("visible summary missing %q: %s", v, visible)
				}
			}
			for _, v := range tc.hidden {
				if strings.Contains(visible, v) || !strings.Contains(details, v) {
					t.Errorf("%q must be in collapsed details only: %s", v, got.Summary)
				}
			}
			for _, v := range tc.absent {
				if strings.Contains(got.Summary, v) {
					t.Errorf("summary should omit %q: %s", v, got.Summary)
				}
			}
			if strings.Count(got.Summary, "Should retries preserve request order?") > 1 {
				t.Error("duplicate question")
			}
			if tc.comments == 1 {
				if got.Comments[0]["suggestion"] != "saveIfCurrent(v)" || !strings.Contains(got.Comments[0]["body"].(string), "Concurrent saves overwrite a newer value.") {
					t.Errorf("finding detail or replacement lost: %+v", got.Comments)
				}
			}
		})
	}
}

// Scope wiring must handle skipped branches and glance just like telemetry.
func TestReviewPRConciseScope(t *testing.T) {
	parsed := parseBotUnit("review-pr/main.bot")
	if parsed.File == nil {
		t.Fatal("parse")
	}
	compiled := ir.Compile(parsed.File)
	if compiled.Workflow == nil {
		t.Fatal("compile")
	}
	var ast *expr.AST
	for _, e := range compiled.Workflow.Nodes["merge_reviews"].(*ir.ComputeNode).Exprs {
		if e.Key == "review_scope" {
			ast = e.AST
		}
	}
	if ast == nil {
		t.Fatal("scope is not wired")
	}
	for _, nodes := range []map[string]any{
		{}, {"reviewer_gpt": []any{"a.go", "test.go"}}, {"reviewer_claude_glance": []any{"a.go"}, "reviewer_gpt_glance": []any{"a.go", "test.go"}},
		{"reviewer_gpt": []string{"a.go", "test.go"}}, {"reviewer_claude": []string{"a.go"}, "reviewer_gpt": []any{"a.go", "test.go"}},
	} {
		ctx := &expr.Context{Outputs: func(path []string) any {
			if len(path) == 2 && path[1] == "scanned_areas" {
				return nodes[path[0]]
			}
			return nil
		}}
		got, err := ast.Eval(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := "a.go ; test.go"
		if len(nodes) == 0 {
			want = ""
		}
		if got != want {
			t.Errorf("scope %v, want %q", got, want)
		}
	}
}
