package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// commandBackticks extracts each `command: ` + "`...`" value from a .bot
// source. The value is backtick-delimited, so its content never contains a
// backtick — a plain scan is exact.
func commandBackticks(src string) []string {
	const marker = "command: `"
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], marker)
		if j < 0 {
			break
		}
		start := i + j + len(marker)
		end := strings.Index(src[start:], "`")
		if end < 0 {
			break
		}
		out = append(out, src[start:start+end])
		i = start + end + 1
	}
	return out
}

var tmplRef = regexp.MustCompile(`\{\{[^}]*\}\}`)

// TestReviewPRPublishCommandsRunCleanly guards a bug class that has bitten the
// review-pr publish path twice: a `python3 -I -c "<body>"` tool command whose BODY
// contains a shell-significant character (a bare double-quote, an unescaped
// backtick) that ends the double-quoted shell argument early and SILENTLY
// truncates the python — exit 0, empty stdout, no work done. `iterion validate`
// cannot see it (the DSL treats the command as opaque). This executes each
// python-`-c` tool command with its `{{…}}` refs blanked, and asserts the
// script still runs to a NON-EMPTY output — a truncated body prints nothing.
func TestReviewPRPublishCommandsRunCleanly(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	data, err := botUnitSource("review-pr/main.bot")
	if err != nil {
		t.Fatalf("read review-pr/main.bot: %v", err)
	}
	blocks := commandBackticks(string(data))
	ran := 0
	for _, blk := range blocks {
		if !strings.Contains(blk, `python3 -I -c "`) {
			continue
		}
		// Skip the diff_precheck command: it chdir's into a workspace and runs
		// git, so it can't execute standalone. Its shape (no findings/questions
		// interpolation) is not the fragile one; publish_review/publish_health
		// are. Detect them by a field they reference.
		if !strings.Contains(blk, "emit(") && !strings.Contains(blk, "degraded") {
			continue
		}
		// Render: blank every {{…}} ref (empty value). publish_review with an
		// empty PUB_URL emits its skip JSON; publish_health with blank counts
		// emits its OK banner — both NON-EMPTY when the body is intact.
		rendered := tmplRef.ReplaceAllString(blk, "")
		out, _ := exec.Command("sh", "-c", rendered).Output()
		if strings.TrimSpace(string(out)) == "" {
			t.Errorf("review-pr/main.bot: a python3 -I -c tool command produced EMPTY output when run — its body is truncated by a stray shell metacharacter (bare double-quote or backtick). Keep the python -c body free of unescaped \" and `. Command head: %q",
				firstLine(rendered))
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("no publish python commands found to exercise — the detection heuristic is stale")
	}

	// Removing either placeholder predicate must publish a false approval;
	// making it unconditional must refuse the healthy and empty-diff rows.
	for _, tc := range []struct {
		name, claude, gpt, scope, files, reason string
		gate                                    bool
	}{
		{"claude filler", "placeholder", "Reviewed", "code:main.go", "1", "summary=placeholder", true},
		{"gpt filler without a required gate", "Reviewed", "placeholder", "code:main.go", "1", "summary=placeholder", false},
		{"empty scanned areas", "Reviewed", "Reviewed", "", "1", "empty scanned_areas", true},
		{"healthy review", "Reviewed placeholder handling", "Reviewed", "code:main.go", "1", "", true},
		{"empty diff", "No changes", "", "", "0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestPath := filepath.Join(t.TempDir(), "request.json")
			t.Setenv("PUBLISH_TEST_REQUEST", requestPath)
			render := func(body string, refs map[string]any) string {
				return tmplRef.ReplaceAllStringFunc(body, func(ref string) string {
					value := ""
					if v, ok := refs[strings.TrimSuffix(strings.TrimPrefix(ref, "{{"), "}}")]; ok {
						if s, ok := v.(string); ok {
							value = s
						} else {
							raw, err := json.Marshal(v)
							if err != nil {
								t.Fatal(err)
							}
							value = string(raw)
						}
					}
					return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
				})
			}
			run := func(body string, refs map[string]any) map[string]any {
				t.Helper()
				out, err := exec.Command("sh", "-c", render(body, refs)).Output()
				if err != nil {
					t.Fatalf("publish command: %v, stdout: %s", err, out)
				}
				var result map[string]any
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatalf("decode publish output: %v, stdout: %s", err, out)
				}
				return result
			}
			// Stub only HTTP transport; the actual shell and Python body still
			// execute, including payload construction and response handling.
			pub := toolCommand(t, "review-pr/main.bot", "publish_review")
			pub = strings.Replace(pub, "import os, json, subprocess, urllib.request, urllib.error, hashlib, re", `import os, json, subprocess, urllib.request, urllib.error, hashlib, re, io

def publish_test_transport(req, **kwargs):
    with open(os.environ['PUBLISH_TEST_REQUEST'], 'wb') as f:
        f.write(req.data)
    return io.BytesIO(json.dumps({'published': True, 'forge': 'github', 'review_url': 'https://forge/review/1', 'gate_posted': True, 'gate_state': 'success'}).encode())
urllib.request.urlopen = publish_test_transport`, 1)
			result := run(pub, map[string]any{
				"vars.forge_publish_url": "https://forge.test/publish", "vars.forge_publish_token": "test-token",
				"vars.gate_enabled": tc.gate, "input.pr_url": "https://github.com/acme/widgets/pull/7",
				"input.findings": "[]", "input.scope_files": tc.files, "input.review_scope": tc.scope,
				"input.claude_summary": tc.claude, "input.gpt_summary": tc.gpt,
			})
			refused := tc.reason != ""
			if result["published"] != !refused || result["gate_posted"] != (!refused && tc.gate) {
				t.Fatalf("unexpected publish outcome: %+v", result)
			}
			if refused {
				if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
					t.Fatalf("refused review reaches HTTP transport: %v", err)
				}
				if !strings.Contains(result["skipped_reason"].(string), tc.reason) || !strings.Contains(result["gate_skipped_reason"].(string), tc.reason) {
					t.Fatalf("refusal must name its reason: %+v", result)
				}
			} else {
				raw, err := os.ReadFile(requestPath)
				if err != nil {
					t.Fatalf("healthy review does not publish: %v", err)
				}
				var payload struct {
					Gate struct {
						Blocking int `json:"blocking_count"`
					} `json:"gate"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil || payload.Gate.Blocking != 0 {
					t.Fatalf("healthy review does not approve: %s, err=%v", raw, err)
				}
			}
			healthRefs := map[string]any{"input.total_findings": 0}
			for k, v := range result {
				healthRefs["input."+k] = v
			}
			health := run(toolCommand(t, "review-pr/main.bot", "publish_health"), healthRefs)
			if health["degraded"] != refused || (refused && !strings.Contains(health["banner"].(string), tc.reason)) {
				t.Fatalf("health must report the refusal by name: %+v", health)
			}
		})
	}
}

func TestReviewPRPublishReceivesTheReviewEvidence(t *testing.T) {
	parsed := parseBotUnit("review-pr/main.bot")
	if parsed.File == nil {
		t.Fatal("review-pr does not parse")
	}
	wf := ir.Compile(parsed.File).Workflow
	if wf == nil {
		t.Fatal("review-pr does not compile")
	}
	want := map[string]string{
		"claude_summary": "{{outputs.merge_reviews.claude_summary}}",
		"gpt_summary":    "{{outputs.merge_reviews.gpt_summary}}",
		"review_scope":   "{{outputs.merge_reviews.review_scope}}",
		"scope_files":    "{{outputs.diff_precheck.changed_files}}",
	}
	for _, edge := range wf.Edges {
		if edge.From != "pr_gate" || edge.To != "publish_review" {
			continue
		}
		for _, mapping := range edge.With {
			if raw, ok := want[mapping.Key]; ok && mapping.Raw == raw {
				delete(want, mapping.Key)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("publish evidence is missing or miswired: %v", want)
	}
	for _, family := range []string{"claude", "gpt"} {
		for _, suffix := range []string{"", "_glance"} {
			found := false
			for _, e := range wf.Nodes["merge_reviews"].(*ir.ComputeNode).Exprs {
				if e.Key != family+"_summary" {
					continue
				}
				found = true
				got, err := e.AST.Eval(&expr.Context{Outputs: func(path []string) any {
					if len(path) == 2 && path[0] == "reviewer_"+family+suffix && path[1] == "summary" {
						return "placeholder"
					}
					return nil
				}})
				if err != nil || got != "placeholder" {
					t.Errorf("%s%s summary disappears before publish: %v, %v", family, suffix, got, err)
				}
			}
			if !found {
				t.Fatalf("merge_reviews does not carry %s_summary", family)
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
