package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestReviewPRMergeFloorRestoresDroppedFindings drives the real publish_review
// command through the merge floor: converge can return a READABLE finding set
// that still lost findings it had no contract to drop (observed on
// deepseek-v4.1-flash: three medium findings in, `[]` out, a PR told nothing
// was found). Dedupe, threshold and cap are the only drops converge may make,
// so each case below is one of them seen from the publish side — the floor
// must restore exactly what none of the three explains, and nothing else.
func TestReviewPRMergeFloorRestoresDroppedFindings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	f := func(file string, line int, sev, title string) map[string]any {
		return map[string]any{"file": file, "line": line, "severity": sev, "title": title,
			"detail": "d", "category": "maintainability", "confidence": "high"}
	}
	enc := func(fs ...map[string]any) string {
		raw, err := json.Marshal(append([]map[string]any{}, fs...))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	skillPath := f(".claude/skills/write-unit-tests/SKILL.md", 42, "medium", "Front unit-test path does not exist")
	portalCode := f(".claude/product.md", 9, "medium", "Wrong portal code PDH instead of PHD")
	commands := f(".claude/orchestration.md", 84, "medium", "Documented commands are not shipped")
	high := f("a.go", 3, "high", "Lost update")
	low := f("b.go", 7, "low", "Naming")

	for _, tc := range []struct {
		name                  string
		merged, claude, gpt   string
		threshold, cap        string
		wantTitles            []string
		wantRestored          int
		wantBlocking          int
		wantNoteHas, wantSays string
		extra                 map[string]string
	}{
		{
			name:   "an emptied merged set gets every reviewer finding back (the deepseek incident)",
			merged: "[]", claude: enc(skillPath, portalCode, commands), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Documented commands are not shipped", "Front unit-test path does not exist", "Wrong portal code PDH instead of PHD"},
			wantRestored: 3, wantNoteHas: "3 findings the merge step dropped, restored from the reviewers",
			wantSays: "Fusion incomplète : 3 findings omis par l’étape de fusion, restaurés depuis les reviewers.",
		},
		{
			name:   "a partial merged set gets only the missing findings back",
			merged: enc(portalCode), claude: enc(skillPath, portalCode, commands), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Documented commands are not shipped", "Front unit-test path does not exist", "Wrong portal code PDH instead of PHD"},
			wantRestored: 2, wantNoteHas: "2 findings the merge step dropped",
		},
		{
			name:      "a cross-family pair converge deduplicated stays one finding",
			merged:    enc(f("a.go", 12, "high", "Lost update on save")),
			claude:    enc(f("a.go", 12, "high", "Lost update on save")),
			gpt:       enc(f("a.go", 14, "high", "Concurrent saves overwrite newer values")),
			threshold: "medium", cap: "15",
			wantTitles: []string{"Lost update on save"}, wantBlocking: 1,
		},
		{
			name:   "a finding kept under a moved anchor is still covered by its id",
			merged: enc(f("a.go", 90, "high", "Lost update")), claude: enc(high), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles: []string{"Lost update"}, wantBlocking: 1,
		},
		{
			name:   "a finding below the threshold is a legitimate drop",
			merged: "[]", claude: enc(low), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles: nil,
		},
		{
			name:   "a full cap is a legitimate drop",
			merged: enc(skillPath, portalCode), claude: enc(skillPath, portalCode, commands), gpt: "[]",
			threshold: "medium", cap: "2",
			wantTitles: []string{"Front unit-test path does not exist", "Wrong portal code PDH instead of PHD"},
		},
		{
			name: "the cap's room goes to the most severe findings first",
			// The high finding comes LAST, so reviewer order alone would spend
			// the room on the two medium ones.
			merged: "[]", claude: enc(skillPath, portalCode, high), gpt: "[]",
			threshold: "medium", cap: "2",
			wantTitles:   []string{"Front unit-test path does not exist", "Lost update"},
			wantRestored: 2, wantBlocking: 1,
		},
		{
			name:   "a finding both families reported is restored once",
			merged: "[]", claude: enc(high), gpt: enc(f("a.go", 3, "high", "Lost update")),
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Lost update"},
			wantRestored: 1, wantBlocking: 1, wantNoteHas: "1 finding the merge step dropped",
			wantSays: "Fusion incomplète : 1 finding omis par l’étape de fusion, restauré depuis les reviewers.",
		},
		{
			// A dedupe keeps the HIGHER severity, so a merged finding less
			// severe than the dropped one cannot be its absorbed twin.
			name:   "a less severe neighbour does not cover a dropped critical",
			merged: enc(f("a.go", 10, "medium", "Naming drift")),
			claude: enc(f("a.go", 10, "medium", "Naming drift"), f("a.go", 12, "critical", "SQL injection")), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Naming drift", "SQL injection"},
			wantRestored: 1, wantBlocking: 1,
		},
		{
			name: "a wide less severe span does not cover a dropped critical inside it",
			merged: enc(map[string]any{"file": "a.go", "line": 1, "line_end": 300, "severity": "medium",
				"title": "Naming drift", "detail": "d"}),
			claude: enc(f("a.go", 150, "critical", "SQL injection")), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Naming drift", "SQL injection"},
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// The floor compares anchors the way the comments are anchored:
			// a rooted rendering of the same file is the same file.
			name:      "a twin anchored on a rooted path of the same file stays covered",
			merged:    enc(f("pkg/a.go", 12, "high", "Lost update on save")),
			claude:    enc(f("pkg/a.go", 12, "high", "Lost update on save")),
			gpt:       enc(f("/pkg/a.go", 13, "high", "Concurrent saves overwrite newer values")),
			threshold: "medium", cap: "15",
			wantTitles: []string{"Lost update on save"}, wantBlocking: 1,
		},
		{
			// Prose items are not findings, so they hold no room under the cap.
			name:   "non-finding items in the merged list do not use the cap",
			merged: `["See structured findings array.","See structured findings array."]`,
			claude: enc(high), gpt: "[]",
			threshold: "medium", cap: "2",
			wantTitles:   []string{"Lost update"},
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// A JSON number past float range decodes to infinity: no int.
			name:   "an out-of-range line number does not kill the publish",
			merged: "[]",
			claude: `[{"file":"a.go","line":1e400,"line_end":1e400,"severity":"high","title":"Lost update","detail":"d"}]`,
			gpt:    "[]", threshold: "medium", cap: "15",
			// No usable line: the finding is published unanchored, in the
			// summary, not as an inline comment.
			wantTitles:   nil,
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// The fallback clause alone outruns the 140 characters a forge
			// status keeps; the restoration must survive the cut.
			name:   "the restoration survives the status cut beside a fallback note",
			merged: "[]", claude: enc(high), gpt: "[]",
			threshold: "medium", cap: "15",
			extra: map[string]string{"input.ai_converge_fallback": "true",
				"input.ai_converge_model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
			wantTitles:   []string{"Lost update"},
			wantRestored: 1, wantBlocking: 1, wantNoteHas: "1 finding the merge step dropped, restored from the reviewers",
		},
		{
			name:   "an unreadable threshold or cap restores at every severity, uncapped",
			merged: "[]", claude: enc(low, high, skillPath), gpt: "[]",
			threshold: "", cap: "",
			wantTitles:   []string{"Front unit-test path does not exist", "Lost update", "Naming"},
			wantRestored: 3, wantBlocking: 1,
		},
		{
			// The id cover and the slack recognise a twin converge
			// deduplicated; they must not fire between two findings the
			// SAME reviewer wrote separately (round 2, R2-1).
			name:   "two distinct findings from one reviewer two lines apart are both restored",
			merged: "[]",
			claude: enc(f("a.go", 10, "high", "SQL injection in query builder"),
				f("a.go", 12, "high", "Unchecked error from Close")),
			gpt:       "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"SQL injection in query builder", "Unchecked error from Close"},
			wantRestored: 2, wantBlocking: 2,
		},
		{
			// Same title, distant lines, one reviewer: the id ignores the
			// line, but these are two separate reports, not a twin.
			name:   "same title from one reviewer at distant lines is restored twice",
			merged: "[]",
			claude: enc(f("a.go", 10, "high", "Unchecked error from Close"),
				f("a.go", 200, "high", "Unchecked error from Close")),
			gpt:       "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Unchecked error from Close", "Unchecked error from Close"},
			wantRestored: 2, wantBlocking: 2,
		},
		{
			// A byte-identical repeat inside one reviewer's own array is
			// one finding, restored once.
			name:   "an exact repeat inside one reviewer's array is restored once",
			merged: "[]",
			claude: enc(f("a.go", 10, "high", "Unchecked error from Close"),
				f("a.go", 10, "high", "Unchecked error from Close")),
			gpt:       "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Unchecked error from Close"},
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// The OTHER family's report can still stand for a dropped
			// twin: cross-family cover survives an emptied merged set.
			name:      "a cross-family twin with an emptied merged set is restored once",
			merged:    "[]",
			claude:    enc(f("a.go", 12, "high", "Lost update on save")),
			gpt:       enc(f("a.go", 14, "high", "Concurrent saves overwrite newer values")),
			threshold: "medium", cap: "15",
			wantTitles: []string{"Lost update on save"}, wantRestored: 1, wantBlocking: 1,
		},
		{
			// Round 2, R2-2: a lone surrogate in a reviewer finding crashes
			// the strict encode() in finding_id and kills the whole publish
			// (the gate never lands). The shell hands the env var through
			// verbatim, so the JSON escape decodes to a real surrogate.
			name:      "a lone surrogate in a reviewer finding title does not kill the publish",
			merged:    "[]",
			claude:    `[{"file":"a.go","line":3,"severity":"high","title":"Emoji ` + "\\ud83d" + ` truncated","detail":"d"}]`,
			gpt:       "[]",
			threshold: "medium", cap: "15",
			wantTitles:   nil,
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// Round 2, R2-3: a restored finding on an anomaly path is
			// reclassified by the strip below the floor; the restoration
			// count must follow what is actually published.
			name:   "a restored finding moved to the anomaly section is not counted as restored",
			merged: "[]", claude: enc(high), gpt: "[]",
			threshold: "medium", cap: "15",
			extra:      map[string]string{"input.workspace_anomaly": "?? a.go"},
			wantTitles: nil,
		},
		{
			// Round 2, R2-3: prose items in the merged list are not
			// findings; the header and the gate total must not count them.
			name:   "prose items in the merged list are not counted as findings",
			merged: `["See structured findings array.","See structured findings array."]`,
			claude: enc(high), gpt: "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Lost update"},
			wantRestored: 1, wantBlocking: 1, wantNoteHas: "blocking of 1 finding(s)",
		},
		{
			// Revi Rb997dc (high): a finding on an anomaly path is
			// reclassified, never published as a PR finding, so it must not
			// consume the restoration cap -- otherwise it eats the slot and
			// an eligible finding on a clean file is dropped.
			name:   "an anomaly-path finding does not consume the restoration cap",
			merged: "[]",
			claude: enc(f("a.go", 5, "critical", "SQL injection in query builder"),
				f("b.go", 9, "high", "Unchecked error from Close")),
			gpt:       "[]",
			threshold: "medium", cap: "1",
			extra:      map[string]string{"input.workspace_anomaly": "?? a.go"},
			wantTitles: []string{"Unchecked error from Close"},
			// only the b.go finding is published as a PR finding; the a.go
			// one is reclassified, so the restoration count names one.
			wantRestored: 1, wantBlocking: 1,
		},
		{
			// Revi Rb17e04 (medium): two findings from one reviewer at the
			// same anchor with the same title but different details are two
			// reports, not a repeat -- only a byte-exact repeat collapses.
			name:   "same anchor and title but different detail restores both",
			merged: "[]",
			claude: enc(map[string]any{"file": "a.go", "line": 10, "severity": "high",
				"title": "Unchecked error from Close", "detail": "rows.Close() is not checked on the read path"},
				map[string]any{"file": "a.go", "line": 10, "severity": "high",
					"title": "Unchecked error from Close", "detail": "stmt.Close() is not checked on the write path"}),
			gpt:       "[]",
			threshold: "medium", cap: "15",
			wantTitles:   []string{"Unchecked error from Close", "Unchecked error from Close"},
			wantRestored: 2, wantBlocking: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Summary  string           `json:"summary"`
				Comments []map[string]any `json:"comments"`
				Gate     struct {
					Note     string `json:"note"`
					Blocking int    `json:"blocking_count"`
				} `json:"gate"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode: %v", err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "comments_posted": len(got.Comments)})
			}))
			defer srv.Close()
			refs := map[string]string{
				"vars.forge_publish_url": srv.URL, "vars.forge_publish_token": "test",
				"input.pr_url": "https://github.com/acme/repo/pull/1", "input.effective_review_mode": "mono",
				"vars.gate_enabled": "true", "vars.gate_severity": "high",
				"input.scope_files": "3", "input.review_scope": "code:a.go",
				"input.findings": tc.merged, "input.claude_findings": tc.claude, "input.gpt_findings": tc.gpt,
				"input.severity_threshold": tc.threshold, "input.max_findings": tc.cap,
			}
			for k, v := range tc.extra {
				refs[k] = v
			}
			body := toolCommand(t, "review-pr/main.bot", "publish_review")
			for _, ref := range []string{"input.severity_threshold", "input.max_findings", "input.claude_findings", "input.gpt_findings"} {
				if !strings.Contains(body, "{{"+ref+"}}") {
					t.Fatalf("publish_review no longer reads {{%s}} — the floor is fed nothing", ref)
				}
			}
			command := regexp.MustCompile(`\{\{([^}]+)\}\}`).ReplaceAllStringFunc(body, func(ref string) string {
				return "'" + strings.ReplaceAll(refs[ref[2:len(ref)-2]], "'", `'\''`) + "'"
			})
			if out, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
				t.Fatalf("publisher: %v\n%s", err, out)
			}

			var titles []string
			for _, c := range got.Comments {
				body, _ := c["body"].(string)
				for _, want := range []string{"Front unit-test path does not exist", "Wrong portal code PDH instead of PHD",
					"Documented commands are not shipped", "Lost update on save", "Concurrent saves overwrite newer values",
					"SQL injection in query builder", "Unchecked error from Close",
					"Lost update", "SQL injection", "Naming drift", "Naming"} {
					if strings.Contains(body, want) {
						titles = append(titles, want)
						break
					}
				}
			}
			sort.Strings(titles)
			if strings.Join(titles, " | ") != strings.Join(tc.wantTitles, " | ") {
				t.Errorf("published findings = %q, want %q\nsummary: %s", titles, tc.wantTitles, got.Summary)
			}
			if got.Gate.Blocking != tc.wantBlocking {
				t.Errorf("gate blocking_count = %d, want %d (gate_severity high)", got.Gate.Blocking, tc.wantBlocking)
			}
			restoredNote := strings.Contains(got.Gate.Note, "the merge step dropped")
			if (tc.wantRestored > 0) != restoredNote {
				t.Errorf("gate note %q: restored=%d must be named exactly when the floor restored something", got.Gate.Note, tc.wantRestored)
			}
			// The forge keeps 140 characters of a status description
			// (pkg/forge/status.go), so that is the part a reader sees.
			shown := got.Gate.Note
			if len(shown) > 140 {
				shown = shown[:140]
			}
			if tc.wantNoteHas != "" && !strings.Contains(shown, tc.wantNoteHas) {
				t.Errorf("gate note as the forge shows it %q does not say %q (full: %q)", shown, tc.wantNoteHas, got.Gate.Note)
			}
			if (tc.wantRestored > 0) != strings.Contains(got.Summary, "Fusion incomplète") {
				t.Errorf("summary must name the restoration exactly when there is one (restored=%d): %s", tc.wantRestored, got.Summary)
			}
			if tc.wantSays != "" && !strings.Contains(got.Summary, tc.wantSays) {
				t.Errorf("summary does not say %q: %s", tc.wantSays, got.Summary)
			}
		})
	}
}
