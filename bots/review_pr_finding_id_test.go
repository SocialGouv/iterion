package bots

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestFindingIDMatchesTheEngineDerivation pins the one thing that makes a
// finding an addressable OBJECT rather than a line of prose.
//
// The same id has to appear in three places written by two languages: Revi's
// inline PR comment (python, in publish_review), the prior-review digest the
// fixer is seeded with (Go, pkg/server/webhooks_handoff.go findingID), and
// the fixer's report of what it did with each finding. If the two derivations
// drift, nothing errors — the operator's `skip R7a3f` silently matches nothing
// and every ledger entry dangles. So the python is executed for real and its
// ids are compared against the Go algorithm, byte for byte.
//
// The Go side is reproduced here rather than imported: pkg/server is not a
// dependency of this package, and a copy that must agree with a hard-coded
// vector is exactly what catches a one-sided change.
func TestFindingIDMatchesTheEngineDerivation(t *testing.T) {
	for _, bin := range []string{"python3", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	// Titles chosen to exercise every normalization step: inner whitespace runs,
	// surrounding space, mixed case, and a title past the 80-char cap.
	findings := []map[string]any{
		{"file": "pkg/db.go", "line": 42, "severity": "high", "category": "security",
			"title": "SQL   injection in the\tuser lookup", "detail": "d"},
		{"file": "pkg/db.go", "line": 9, "severity": "low", "category": "tests",
			"title": "  Missing Coverage For The Error Path  ", "detail": "d"},
		{"file": "cmd/main.go", "line": 3, "severity": "medium", "category": "correctness",
			"title": strings.Repeat("very long title ", 12), "detail": "d"},
		// Non-ASCII past the cap: python slices CHARACTERS, so a byte-slicing Go
		// side both cut somewhere else AND hashed a split rune. Every id on a
		// French or CJK finding disagreed, silently.
		{"file": "pkg/db.go", "line": 7, "severity": "high", "category": "correctness",
			"title": "Injection SQL dans la récupération de l'utilisateur authentifié — vérifier les paramètres échappés", "detail": "d"},
		{"file": "pkg/db.go", "line": 8, "severity": "low", "category": "tests",
			"title": "ユーザー認証のカバレッジが不足している、エラーパスの検証が行われていない", "detail": "d"},
	}

	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--quiet", "-b", "main")
	if err := os.WriteFile(ws+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "one")

	var got struct {
		Summary  string           `json:"summary"`
		Comments []map[string]any `json:"comments"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("publish payload is not JSON: %v (%q)", err, raw)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "comments_posted": len(got.Comments)})
	}))
	defer srv.Close()

	encoded, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	body := toolCommand(t, "review-pr/main.bot", "publish_review")
	for ref, val := range map[string]string{
		"{{vars.workspace_dir}}":          ws,
		"{{input.reviewed_sha}}":          "",
		"{{vars.forge_publish_url}}":      srv.URL + "/api/v1/forge/publish-review",
		"{{vars.forge_publish_token}}":    "run-token",
		"{{vars.pr_review_mode}}":         "inline",
		"{{input.effective_review_mode}}": "mono",
		"{{input.pr_url}}":                "https://github.com/acme/widgets/pull/7",
		"{{input.findings}}":              string(encoded),
		"{{input.questions}}":             "",
		"{{input.claude_findings}}":       "[]",
		"{{input.gpt_findings}}":          "[]",
		"{{vars.gate_enabled}}":           "true",
		"{{vars.gate_severity}}":          "high",
		"{{vars.gate_context}}":           "revi/review",
	} {
		if !strings.Contains(body, ref) {
			t.Fatalf("%s is no longer referenced by publish_review — the test wires nothing", ref)
		}
		body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
	}
	out, err := exec.Command("sh", "-c", body).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	if len(got.Comments) != len(findings) {
		t.Fatalf("posted %d comment(s) for %d finding(s)", len(got.Comments), len(findings))
	}

	for i, f := range findings {
		want := goFindingID(f["file"].(string), f["title"].(string))
		body, _ := got.Comments[i]["body"].(string)
		if !strings.HasPrefix(body, want+" ") {
			t.Errorf("finding %d: comment body starts %q, want the engine id %q — the two derivations have drifted",
				i, firstLine(body), want)
		}
	}
	// The summary must show an id too: in `summary` mode there are no inline
	// comments to read one from, and the operator needs one to arbitrate by.
	if id := goFindingID("pkg/db.go", "SQL   injection in the\tuser lookup"); !strings.Contains(got.Summary, id) {
		t.Errorf("review summary never shows a finding id (%s), so nothing tells the operator how to skip one:\n%s", id, got.Summary)
	}

	// A frozen vector: catches a change made identically on BOTH sides, which
	// would keep them agreeing while silently renaming every existing finding.
	if id := goFindingID("pkg/db.go", "SQL injection in the user lookup"); id != "R5ee591" {
		t.Errorf("the finding-id derivation changed (%s): every id already posted on an open PR, and every ledger entry keyed on one, is now orphaned. Change it only deliberately.", id)
	}
}

// goFindingID mirrors pkg/server/webhooks_handoff.go findingID.
func goFindingID(file, title string) string {
	t := strings.ToLower(strings.Join(strings.Fields(title), " "))
	if r := []rune(t); len(r) > 80 {
		t = string(r[:80])
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(file) + "\n" + t))
	return "R" + hex.EncodeToString(sum[:])[:6]
}
