package bots

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runPostFeedback runs the post_feedback script the way the engine runs it:
// the publish grant in the interpreter's ENVIRONMENT, under the name the body
// reads, never inlined into the script body (#2033). An empty grant is the
// local-CLI case — the variable is not set at all.
func runPostFeedback(t *testing.T, path, grant string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("python3", path)
	cmd.Env = os.Environ()
	if grant != "" {
		cmd.Env = append(cmd.Env, secretguard.PlaceholderForName(store.ForgePublishTokenVar)+"="+grant)
	}
	return cmd.Output()
}

// The bot names the engine's variable literally — a .bot cannot compute it.
// If the placeholder convention moved, post_feedback would read an unset
// variable and publish nothing, reporting "no grant" on a run that had one:
// this is the pin that refuses the drift.
func TestDepUpdateGuardReadsTheGrantUnderTheEnginesName(t *testing.T) {
	script := toolScript(t, "dep-update-guard/main.bot", "post_feedback")
	name := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	if !strings.Contains(script, `os.environ.get("`+name+`"`) {
		t.Errorf("post_feedback does not read the grant under %s: the publish would go unauthenticated", name)
	}
	if strings.Contains(script, "{{vars."+store.ForgePublishTokenVar+"}}") {
		t.Error("post_feedback inlines the grant into the script body, which is written to a temp file")
	}
}

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
		// AuditedSHA pins the verdict to the revision this run read. The server
		// refuses a pin that is no longer the head instead of retargeting the
		// status, so a payload that drops it goes quietly back to certifying
		// whatever revision arrived last.
		AuditedSHA string `json:"audited_sha"`
	}
	type published struct {
		PRURL   string `json:"pr_url"`
		Summary string `json:"summary"`
		Gate    *gate  `json:"gate"`
	}

	// pinOverride, when non-empty, replaces the audited_sha fixture — the one
	// input on the committed path that an AGENT produces, so the one that can
	// arrive in a shape the server refuses.
	// The run page the engine resolves into the comment's signature. A
	// subtest blanks it to exercise a deployment with no public address.
	runPage := "https://iterion.example/studio/runs/01a0dep"
	// When set, the EXACT python literal substituted for {{run.url}}: what an
	// engine predating the member renders in a script body is the JSON
	// `null`, which no quoted value can stand in for.
	runPageLit := ""
	runID := "01a0dep"
	// align is the "Code alignment" body a node maps in: empty on every
	// verdict but the delivery hold, whose edge sends the probe's reason
	// through align_summary.
	run := func(t *testing.T, verdict, gateContext string, gateEnabled bool, align string, pinOverride ...string) (published, map[string]any) {
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
			"{{input.align_summary}}":      strconv.Quote(align),
			"{{input.validate_summary}}":   `""`,
			"{{input.escalation}}":         `""`,
			"{{input.commit_summary}}":     `""`,
			"{{input.audited_sha}}":        `"feedfacefeed"`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `"` + srv.URL + `/api/v1/forge/publish-review"`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
			"{{vars.gate_enabled}}":        boolLit(gateEnabled),
			"{{vars.gate_context}}":        `"` + gateContext + `"`,
			"{{input.origin_sha}}":         `""`,
			"{{input.origin_at}}":          `""`,
			"{{vars.workspace_dir}}":       `""`,
			"{{vars.base_ref}}":            `"main"`,
			"{{input.head_sha}}":           `""`,
			"{{input.head_id}}":            `""`,
			"{{vars.continuity}}":          "True",
			"{{vars.carry_max_age_h}}":     "72",
		} {
			if ref == "{{input.audited_sha}}" && len(pinOverride) > 0 {
				val = strconv.Quote(pinOverride[0])
			}
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
		out, err := runPostFeedback(t, f.Name(), "run-token")
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
		got, _ := run(t, "clean", "iterion/review", true, "")
		if got.Gate == nil {
			t.Fatal("no gate payload")
		}
		if strings.Contains(got.Gate.Note, "alignment committed") {
			t.Errorf("check claims a commit on the no-alignment verdict: %q", got.Gate.Note)
		}
		// "nothing left to align", not "no alignment needed": the same verdict
		// closes the pass the alignment's own push relaunches, where the head
		// already carries an alignment (buildkit-operator#50).
		if !strings.Contains(got.Gate.Note, "nothing left to align") {
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
		got, _ := run(t, "hold_lost_alignment", "iterion/review", true, "")
		if got.Gate == nil {
			t.Fatal("no gate payload")
		}
		if got.Gate.BlockingCount == 0 {
			t.Error("a bump whose alignment is missing must not be mergeable")
		}
		if strings.Contains(got.Gate.Note, "nothing left to align") {
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

	// Every comment a bot posts names the run that wrote it: the reader's way
	// to the steps, the models and the cost behind the verdict, and the way
	// into the platform for someone meeting it on their own forge. A
	// deployment that publishes no public address leaves the id alone — a
	// root-relative link posted on a forge resolves against the FORGE.
	t.Run("the comment links the run that wrote it", func(t *testing.T) {
		pub, _ := run(t, "clean", "iterion/review", true, "")
		want := "run [`" + runID + "`](" + runPage + ")"
		if !strings.Contains(pub.Summary, want) {
			t.Errorf("the comment does not link its run (%s):\n%s", want, pub.Summary)
		}

		restore := runPage
		runPage = ""
		defer func() { runPage = restore }()
		pub, _ = run(t, "clean", "iterion/review", true, "")
		// The product reference stays (a constant, not this deployment's
		// address); only the RUN link must be gone.
		if strings.Contains(pub.Summary, "run [") {
			t.Errorf("with no public address the signature must not link the run:\n%s", pub.Summary)
		}
		if !strings.Contains(pub.Summary, "run `"+runID+"`") {
			t.Errorf("with no public address the signature must still name the run id:\n%s", pub.Summary)
		}
	})

	// A run id is CALLER-CHOSEN at launch and screened only for path
	// traversal (store.SanitizePathComponent accepts `]`, `(`, `)` and a
	// backtick), so an id can hold markdown. Pasted raw into the footer's
	// label it closes the link early and publishes an attacker's host under
	// this bot's signature, on someone else's repository. WHATWG resolves a
	// slash-free `https:evil.example`, so no path separator is needed.
	t.Run("a run id holding markdown never becomes a link", func(t *testing.T) {
		restore := runID
		runID = "x](https:evil.example)"
		defer func() { runID = restore }()
		pub, _ := run(t, "clean", "iterion/review", true, "")
		if strings.Contains(pub.Summary, "evil.example)") || strings.Contains(pub.Summary, "](https:evil") {
			t.Errorf("the id was pasted into the footer and published a foreign link:\n%s", pub.Summary)
		}
		if !strings.Contains(pub.Summary, "[this run](") {
			t.Errorf("the run link must survive with a fixed label when the id is unusable:\n%s", pub.Summary)
		}
		if !strings.Contains(pub.Summary, runPage) {
			t.Errorf("the footer must still point at this run's own page:\n%s", pub.Summary)
		}
	})

	// An engine that predates the run.url member leaves the reference
	// UNRESOLVED, and a script body renders a missing ref as the JSON `null`
	// literal — C153 is a warning, so the bundle still admits there. The
	// footer must degrade to the id, never publish "None" as an address.
	t.Run("an older engine degrades to the id, never a null address", func(t *testing.T) {
		runPageLit = "null"
		defer func() { runPageLit = "" }()
		pub, _ := run(t, "clean", "iterion/review", true, "")
		if strings.Contains(pub.Summary, "None") || strings.Contains(pub.Summary, "run [") {
			t.Errorf("the unresolved reference was published as an address:\n%s", pub.Summary)
		}
		if !strings.Contains(pub.Summary, "run `"+runID+"`") {
			t.Errorf("the footer must still name the run id:\n%s", pub.Summary)
		}
	})

	// On the committed path the pin is read off an AGENT's report, and this bot
	// has already been burned by the model answering with the abbreviated form
	// `git commit` prints. The server REFUSES a pin it cannot read, and that
	// refusal costs the required check — so a malformed answer must degrade to
	// an UNPINNED post that says why, never to no status at all.
	t.Run("a pin the server could not read degrades to unpinned, and says so", func(t *testing.T) {
		for _, bad := range []string{
			"a11ced0 (HEAD -> renovate/go-mods)", "HEAD", "none", "a11ced", "", "  ",
		} {
			t.Run(strconv.Quote(bad), func(t *testing.T) {
				pub, _ := run(t, "committed", "iterion/review", true, "", bad)
				if pub.Gate == nil {
					t.Fatal("the gate must still be requested — a bad pin costs the pin, not the check")
				}
				if pub.Gate.AuditedSHA != "" {
					t.Fatalf("a pin the server would refuse must not be sent, got %q", pub.Gate.AuditedSHA)
				}
				// Empty is the ordinary "nothing to pin" case (no commit on this
				// path); only a value that was PRESENT and wrong needs saying.
				if strings.TrimSpace(bad) != "" && !strings.Contains(pub.Gate.Note, "UNPINNED") {
					t.Errorf("a pin that was present and unusable must be reported, got note %q", pub.Gate.Note)
				}
			})
		}
	})

	// #1595: the delivery App cannot push .github/workflows/**, so an Actions
	// bump whose alignment edits the workflow used to 403 at push and arrive
	// as `hold_lost_alignment` — "the code alignment for this bump is missing
	// from the branch". The operator then looks for work the bot lost instead
	// of granting a permission. This verdict's whole job is to be readable:
	// the cause, and whose move it is.
	t.Run("hold_delivery_permission: names the permission, not a lost alignment", func(t *testing.T) {
		got, _ := run(t, "hold_delivery_permission", "iterion/review", true, "")
		if got.Gate == nil {
			t.Fatal("no gate payload")
		}
		if got.Gate.BlockingCount == 0 {
			t.Error("a bump whose alignment cannot be delivered must not be mergeable")
		}
		if strings.Contains(got.Gate.Note, "unrecognised verdict") {
			t.Errorf("the gate does not know this verdict, so the check says nothing useful: %q", got.Gate.Note)
		}
		for _, want := range []string{"workflow", "permission"} {
			if !strings.Contains(got.Gate.Note, want) {
				t.Errorf("the check's note must name %q — a reviewer reads nothing else on the PR: %q", want, got.Gate.Note)
			}
		}
		if strings.Contains(got.Gate.Note, "missing from the branch") {
			t.Errorf("the check still reports a permission refusal as a lost alignment: %q", got.Gate.Note)
		}
		for _, want := range []string{".github/workflows/", "Nothing was committed", "refresh its token"} {
			if !strings.Contains(got.Summary, want) {
				t.Errorf("the comment must carry %q — it is the operator's whole next move:\n%s", want, got.Summary)
			}
		}
		if strings.Contains(got.Summary, "ℹ️ Vetty review") {
			t.Errorf("the comment falls back to the unknown-verdict badge:\n%s", got.Summary)
		}
	})

	// TWO causes reach this verdict, and both are ANSWERED refusals — a
	// permission the App does not hold (the preflight's verdict) and a
	// classic token short of a scope (the forge API's scope list). Their
	// cures differ: a grant the operator performs on the App, or a reissued
	// token. A consumer text keyed on the VERDICT alone prescribes the grant
	// for both, which sends the operator of a PAT connection after work they
	// cannot perform — one cause reported as another, on a public pull
	// request, by the one feature whose whole job is to stop doing that.
	//
	// The third cause this menu used to carry — a preflight that did not
	// answer — can no longer reach a verdict at all: it ends the run as
	// FORGE_PREFLIGHT_UNAVAILABLE and publishes nothing. A relaunch cure
	// offered here would describe a hold that cannot happen, which is the
	// stale half of a menu nobody re-reads.
	t.Run("hold_delivery_permission: the comment carries BOTH answered cures, and no cure for a hold that cannot happen", func(t *testing.T) {
		const grant = "Delivery refused before the push: workflows:write is missing or unverified on the " +
			"runtime token; an operator must approve the App's delivery permissions, refresh its token and relaunch"
		got, _ := run(t, "hold_delivery_permission", "iterion/review", true, grant)
		for _, want := range []struct{ frag, why string }{
			{"the delivery permission for it was refused", "the badge says what happened: the credential was asked and refused"},
			{"refresh its token", "the grant cure, for a permission the App does not hold"},
			{"reissue it", "the scope cure, for a runtime token the forge API answered about"},
			{"applied by hand", "the manual path, which neither cure replaces"},
		} {
			if !strings.Contains(got.Summary, want.frag) {
				t.Errorf("the comment does not carry %q — %s:\n%s", want.frag, want.why, got.Summary)
			}
		}
		if strings.Contains(got.Summary, "once the service answers") {
			t.Errorf("the comment offers the relaunch cure of a preflight that did not answer — a cause that reaches no verdict, so the menu sends the operator after an outage nobody had:\n%s", got.Summary)
		}
	})

	// The third cause, as it arrives: the classic-token branch runs BEFORE the
	// installation-token question and refuses a PAT carrying no `workflow`
	// scope (forge.KindPAT is a first-class connection kind). Behind that
	// refusal there is no App and no delivery permission to approve, so a
	// comment offering only the grant and the relaunch sends that operator
	// after work they cannot perform.
	t.Run("hold_delivery_permission: a token short of a scope gets a cure it can perform", func(t *testing.T) {
		const scope = "Delivery refused before the push: workflows:write is missing or unverified on the " +
			"runtime token (classic scope: workflow); reissue the token with the workflow and repo scopes, " +
			"then relaunch"
		got, _ := run(t, "hold_delivery_permission", "iterion/review", true, scope)
		if !strings.Contains(got.Summary, scope) {
			t.Fatalf("the refusal the probe emitted does not reach the comment, so neither does its cure:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "reissue it") {
			t.Errorf("the comment's cure menu skips the cause this refusal names — a PAT connection has no App to grant anything on:\n%s", got.Summary)
		}
	})

	// align_summary renders under "## Code alignment", and the delivery hold
	// is the one verdict whose edge maps a REASON through it. Pinned as a
	// substring of the .bot source, that labelling proves the edge and not the
	// render: a comment that dropped or relocated align_summary would stay
	// green while the permission refusal read as a description of the
	// alignment — the wrong-cause diagnosis in another costume.
	t.Run("hold_delivery_permission: the probe's reason renders under Code alignment", func(t *testing.T) {
		const reason = "Delivery refused before the push: workflows:write is missing or unverified on the runtime token"
		const heading = "## Code alignment"
		got, _ := run(t, "hold_delivery_permission", "iterion/review", true, reason)
		at := strings.Index(got.Summary, heading)
		if at < 0 {
			t.Fatalf("no %q section rendered, so the probe's reason reaches the reader nowhere:\n%s", heading, got.Summary)
		}
		section := got.Summary[at+len(heading):]
		if end := strings.Index(section, "\n## "); end >= 0 {
			section = section[:end]
		}
		if !strings.Contains(section, reason) {
			t.Errorf("the probe's reason does not render under %q:\n%s", heading, got.Summary)
		}
	})

	t.Run("committed: says so, in the badge and the check", func(t *testing.T) {
		got, _ := run(t, "committed", "iterion/review", true, "")
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
		// A carry is green: the identical change's audit, carried onto this
		// head (by continuity, or by post_feedback's repin).
		{"carried", false},
		{"hold_security", true},
		{"hold_unstable", true},
		{"needs_decision", true},
		{"hold_lost_alignment", true},
		{"hold_delivery_permission", true},
		// Not a verdict the graph can produce today. If one ever appears, the
		// gate must refuse it rather than wave it through.
		{"probably_fine", true},
		{"", true},
	} {
		t.Run("verdict="+tc.verdict, func(t *testing.T) {
			pub, res := run(t, tc.verdict, "iterion/review", true, "")
			if pub.Gate == nil {
				t.Fatal("no gate in the publish payload")
			}
			// The pin has to REACH the endpoint, on every verdict — a hold is
			// as much a statement about a revision as a pass is, and the
			// server can only refuse a stale one it was given.
			if pub.Gate.AuditedSHA != "feedfacefeed" {
				t.Errorf("verdict %q: gate.audited_sha = %q, want the revision the run read — without it the status certifies whatever head the forge reports at publish time",
					tc.verdict, pub.Gate.AuditedSHA)
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

	// The class rule: every verdict a reader elsewhere treats as green is a
	// green gate here, or a carry would publish a red check.
	for _, green := range depUpdateGuardGreenVerdicts {
		t.Run("green verdict publishes a green gate/"+green, func(t *testing.T) {
			if pub, _ := run(t, green, "iterion/review", true, ""); pub.Gate == nil || pub.Gate.BlockingCount != 0 ||
				strings.Contains(pub.Gate.Note, "unrecognised verdict") {
				t.Errorf("verdict %q is green to arm_automerge but not to the gate: %+v", green, pub.Gate)
			}
		})
	}

	t.Run("unknown verdict says why it blocked", func(t *testing.T) {
		pub, _ := run(t, "probably_fine", "iterion/review", true, "")
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
			"{{input.audited_sha}}":        `"feedfacefeed"`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `"` + redir.URL + `/api/v1/forge/publish-review"`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
			"{{vars.gate_enabled}}":        `True`,
			"{{vars.gate_context}}":        `"iterion/review"`,
			"{{input.origin_sha}}":         `""`,
			"{{input.origin_at}}":          `""`,
			"{{vars.workspace_dir}}":       `""`,
			"{{vars.base_ref}}":            `"main"`,
			"{{input.head_sha}}":           `""`,
			"{{input.head_id}}":            `""`,
			"{{vars.continuity}}":          "True",
			"{{vars.carry_max_age_h}}":     "72",
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
		out, err := runPostFeedback(t, f.Name(), "run-token")
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
		pub, res := run(t, "committed", "iterion/review", false, "")
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
			"{{input.audited_sha}}":        `"feedfacefeed"`,
			"{{secrets.forge_token.path}}": `""`,
			"{{vars.forge_publish_url}}":   `""`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
			"{{vars.gate_enabled}}":        `True`,
			"{{vars.gate_context}}":        `"iterion/review"`,
			"{{input.origin_sha}}":         `""`,
			"{{input.origin_at}}":          `""`,
			"{{vars.workspace_dir}}":       `""`,
			"{{vars.base_ref}}":            `"main"`,
			"{{input.head_sha}}":           `""`,
			"{{input.head_id}}":            `""`,
			"{{vars.continuity}}":          "True",
			"{{vars.carry_max_age_h}}":     "72",
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
		out, err := runPostFeedback(t, f.Name(), "")
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
	pr := parseBotUnit(rel)
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

// runURLLit renders the python literal the engine substitutes for
// {{run.url}}: a quoted string normally, or the raw literal a caller pins —
// the JSON `null` an engine predating the member leaves in a script body.
func runURLLit(page, override string) string {
	if override != "" {
		return override
	}
	return `"` + page + `"`
}

// feedbackForge is the server's publish endpoint plus the GitHub reads
// post_feedback makes for a repin: the pull request, its reviews, and the
// combined status on a sha. gitRoot, when set, is where the script's
// explicit git URL points instead of https://github.com/.
type feedbackForge struct {
	t        *testing.T
	mu       sync.Mutex
	head     string // the head GET pulls/7 reports; "" answers 404
	base     string
	reviews  []map[string]any
	statuses map[string][]map[string]any
	gitRoot  string
	payload  map[string]any
	reads    []string
}

func (f *feedbackForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const prefix = "/repos/acme/widgets/"
	switch {
	case r.URL.Path == "/api/v1/forge/publish-review":
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			f.t.Errorf("decode publish payload: %v", err)
		}
		f.payload = p
		gate, _ := p["gate"].(map[string]any)
		state := "success"
		if n, _ := gate["blocking_count"].(float64); n > 0 {
			state = "failure"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"published": true, "review_url": "https://github.com/acme/widgets/pull/7#pullrequestreview-1",
			"gate_posted": gate != nil, "gate_state": state, "gate_sha": gate["audited_sha"],
		})
	case r.Method != http.MethodGet:
		f.t.Errorf("post_feedback wrote to the forge API: %s %s — it reads only", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.URL.Path == prefix+"pulls/7":
		f.reads = append(f.reads, r.URL.Path)
		if f.head == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": f.base}})
	case r.URL.Path == prefix+"pulls/7/reviews":
		f.reads = append(f.reads, r.URL.Path)
		reviews := f.reviews
		if reviews == nil {
			reviews = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(reviews)
	case strings.HasPrefix(r.URL.Path, prefix+"commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		f.reads = append(f.reads, r.URL.Path)
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"commits/"), "/status")
		st := f.statuses[sha]
		if st == nil {
			st = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "statuses": st, "total_count": len(st)})
	default:
		f.t.Errorf("post_feedback called %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// runFeedback drives post_feedback against a feedbackForge.
func runFeedback(t *testing.T, forge *feedbackForge, over map[string]string, env ...string) (map[string]any, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(forge)
	defer srv.Close()
	subs := map[string]string{
		"{{input.pr_url}}":             `"https://github.com/acme/widgets/pull/7"`,
		"{{input.verdict}}":            `"clean"`,
		"{{input.audit_summary}}":      `"audited"`,
		"{{input.cves}}":               `""`,
		"{{input.malware_signals}}":    `""`,
		"{{input.align_summary}}":      `""`,
		"{{input.validate_summary}}":   `""`,
		"{{input.escalation}}":         `""`,
		"{{input.commit_summary}}":     `""`,
		"{{input.audited_sha}}":        `""`,
		"{{input.origin_sha}}":         `""`,
		"{{input.origin_at}}":          `""`,
		"{{secrets.forge_token.path}}": `"ghs_test"`,
		"{{vars.forge_publish_url}}":   `"` + srv.URL + `/api/v1/forge/publish-review"`,
		"{{run.url}}":                  `"https://iterion.example/studio/runs/01a0dep"`,
		"{{run.id}}":                   `"01a0dep"`,
		"{{vars.gate_enabled}}":        `True`,
		"{{vars.gate_context}}":        `"vetty/deps"`,
		"{{vars.workspace_dir}}":       `""`,
		"{{vars.base_ref}}":            `"main"`,
		"{{input.head_sha}}":           `""`,
		"{{input.head_id}}":            `""`,
		"{{vars.continuity}}":          "True",
		"{{vars.carry_max_age_h}}":     "72",
	}
	for k, v := range over {
		subs[k] = v
	}
	body := toolScript(t, depUpdateGuardBot, "post_feedback")
	for ref, val := range subs {
		body = strings.ReplaceAll(body, ref, val)
	}
	for _, url := range []string{`"https://api.github.com"`, `"https://github.com/"`} {
		if strings.Count(body, url) != 1 {
			t.Fatalf("post_feedback names %s %d times, want once: the stub stands in for it", url, strings.Count(body, url))
		}
	}
	body = strings.ReplaceAll(body, `"https://api.github.com"`, `"`+srv.URL+`"`)
	if forge.gitRoot != "" {
		body = strings.ReplaceAll(body, `"https://github.com/"`, quoted(forge.gitRoot))
	}
	if strings.Contains(body, "{{") {
		t.Fatalf("unsubstituted ref: %s", firstRef(body))
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "feedback.py")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", path)
	cmd.Dir = dir
	cmd.Env = append(append(gittest.Env(), secretguard.PlaceholderForName(store.ForgePublishTokenVar)+"=run-token"), env...)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("post_feedback failed: %v (out %q)", err, raw)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("post_feedback output is not feedback_output JSON: %v (%q)", err, raw)
	}
	forge.mu.Lock()
	defer forge.mu.Unlock()
	if forge.payload == nil {
		t.Fatal("post_feedback published nothing")
	}
	return forge.payload, res
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var markerLine = regexp.MustCompile(`^<!-- vetty-gate/v1 sha=([0-9a-f]{40}) id=(pid1:[0-9a-f]{24}) verdict=([a-z_]+) origin=([0-9a-f]{40}) origin_at=([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z) -->$`)

// forgeOrigin is a bare repository standing in for the forge's git
// endpoint: <root>/acme/widgets.git, served as file://<root>/.
type forgeOrigin struct {
	root, dir string
}

func newForgeOrigin(t *testing.T) forgeOrigin {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "acme", "widgets.git")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-q", "--bare", dir)
	gittest.Run(t, dir, "config", "maintenance.auto", "false")
	gittest.Run(t, dir, "config", "gc.auto", "0")
	gittest.Run(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	return forgeOrigin{root: root, dir: dir}
}

// gitRoot is what the script's explicit forge URL is pointed at.
func (o forgeOrigin) gitRoot() string { return "file://" + o.root + "/" }

// TestDepUpdateGuardGateCarriesItsChangeIdentity pins the half of gate
// continuity post_feedback owns: the fact travels WITH the gate. The note
// ends with the change identity of the pinned revision, within GitHub's 140
// runes on every verdict; the review's last line is the marker continuity
// reads; a marker an agent writes into a section is defused; and the two
// copies of the identity contract are one. The identity of the head prepare
// read is continuity's — computed before any agent ran — and the identity of
// the alignment this run pushed is read from the forge, never the workspace.
//
// Mutations (main.bot `post_feedback`) and the case each reddens: put the
// stamp before the note, or drop it on holds (the per-verdict case); stamp a
// pin that is not a full commit id (the bad-pin case); drop the
// neutralisation (the planted-marker case); edit a flag in one copy of the
// identity block only (the parity case); stamp head_id on every pin, the
// pushed alignment included, or read the alignment's identity from the forge
// when continuity identified nothing (the alignment case).
func TestDepUpdateGuardGateCarriesItsChangeIdentity(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	t.Run("the identity contract has one text", func(t *testing.T) {
		if a, b := identityBlock(t, "continuity"), identityBlock(t, "post_feedback"); a != b {
			t.Errorf("the identity block differs between continuity and post_feedback: the id one stamps is not the id the other compares")
		}
	})

	ws := t.TempDir()
	gittest.InitRepo(t, ws)
	base := commitTree(t, ws, "HEAD", "base", map[string][]byte{"deps.yaml": depsYAML(nil)})
	gittest.Run(t, ws, "branch", "-f", "main", base)
	x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
	id, why := changeIdentity(t, ws, "main", "main", x)
	if id == "" {
		t.Fatalf("fixture has no identity: %s", why)
	}
	origin := strings.Repeat("0a", 20)
	originAt := "2026-11-10T08:00:00Z"
	// The head prepare read is x, and continuity identified it.
	inWS := map[string]string{
		"{{input.audited_sha}}": quoted(x), "{{input.head_sha}}": quoted(x),
		"{{input.head_id}}": quoted(id),
	}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range inWS {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	gateOf := func(p map[string]any) map[string]any {
		g, _ := p["gate"].(map[string]any)
		if g == nil {
			t.Fatalf("no gate in the payload: %v", p)
		}
		return g
	}
	lastLine := func(s string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return lines[len(lines)-1]
	}

	for _, verdict := range []string{"clean", "committed", "carried", "hold_security", "hold_unstable",
		"needs_decision", "hold_lost_alignment", "hold_delivery_permission", "probably_fine"} {
		t.Run("stamp and marker/"+verdict, func(t *testing.T) {
			over := map[string]string{"{{input.verdict}}": quoted(verdict)}
			wantOrigin := x
			if verdict == "carried" {
				over["{{input.origin_sha}}"], over["{{input.origin_at}}"] = quoted(origin), quoted(originAt)
				wantOrigin = origin
			}
			p, res := runFeedback(t, &feedbackForge{t: t}, with(over))
			note, _ := gateOf(p)["note"].(string)
			if !strings.HasSuffix(note, " · "+id) {
				t.Errorf("the note does not END with the change identity %s: %q", id, note)
			}
			if n := utf8.RuneCountInString(note); n > 140 || forge.TruncateStatusDescription(note) != note {
				t.Errorf("note + stamp is %d runes: the forge truncates it past 140 and the stamp is lost: %q", n, note)
			}
			summary, _ := p["summary"].(string)
			m := markerLine.FindStringSubmatch(lastLine(summary))
			if m == nil {
				t.Fatalf("the review's last line is not the gate marker:\n%s", summary)
			}
			wantVerdict := verdict
			if verdict == "probably_fine" {
				wantVerdict = "unknown"
			}
			if m[1] != x || m[2] != id || m[3] != wantVerdict || m[4] != wantOrigin {
				t.Errorf("marker = sha %s id %s verdict %s origin %s, want %s %s %s %s", m[1], m[2], m[3], m[4], x, id, wantVerdict, wantOrigin)
			}
			if verdict == "carried" && m[5] != originAt {
				t.Errorf("a carry restarted its origin's clock: origin_at %s, want %s", m[5], originAt)
			}
			if strings.Count(summary, "<!-- vetty-gate") != 1 {
				t.Errorf("the review carries %d markers, want exactly the last line", strings.Count(summary, "<!-- vetty-gate"))
			}
			if res["pinned_sha"] != x {
				t.Errorf("pinned_sha = %v, want the pin %s", res["pinned_sha"], x[:12])
			}
		})
	}

	// Only a FULL commit id is stamped: an abbreviated pin is still a pin,
	// but no identity is computed for it, and a pin that is no commit id
	// posts unpinned and certifies nothing.
	for _, pin := range []string{x[:12], "HEAD", ""} {
		t.Run("no stamp without a full commit id/"+strconv.Quote(pin), func(t *testing.T) {
			p, _ := runFeedback(t, &feedbackForge{t: t}, with(map[string]string{"{{input.audited_sha}}": quoted(pin)}))
			note, _ := gateOf(p)["note"].(string)
			summary, _ := p["summary"].(string)
			if strings.Contains(note, "pid1:") || strings.Contains(summary, "<!-- vetty-gate") {
				t.Errorf("pin %q was stamped: note %q", pin, note)
			}
		})
	}

	t.Run("the head read with no identity from continuity is not stamped", func(t *testing.T) {
		p, _ := runFeedback(t, &feedbackForge{t: t}, with(map[string]string{"{{input.head_id}}": `null`}))
		if note, _ := gateOf(p)["note"].(string); strings.Contains(note, "pid1:") {
			t.Errorf("a verdict was stamped with an identity nobody computed before the agents ran: %q", note)
		}
	})

	t.Run("a carried verdict that cannot name its origin is not stamped", func(t *testing.T) {
		p, _ := runFeedback(t, &feedbackForge{t: t}, with(map[string]string{"{{input.verdict}}": `"carried"`}))
		if note, _ := gateOf(p)["note"].(string); strings.Contains(note, "pid1:") {
			t.Errorf("a carry with no origin was stamped as its own origin, restarting its age: %q", note)
		}
	})

	t.Run("a marker planted in an agent-written section is defused", func(t *testing.T) {
		planted := "looks fine\n" + gateMarker(strings.Repeat("b", 40), id, "clean", strings.Repeat("b", 40), originAt) + "\nend"
		p, _ := runFeedback(t, &feedbackForge{t: t}, with(map[string]string{
			"{{input.audit_summary}}": quoted(planted), "{{input.verdict}}": `"hold_security"`,
		}))
		summary, _ := p["summary"].(string)
		if strings.Count(summary, "<!-- vetty-gate") != 1 || !markerLine.MatchString(lastLine(summary)) {
			t.Errorf("the planted marker survived as a marker:\n%s", summary)
		}
		if !strings.Contains(summary, "&lt;!-- vetty-gate") {
			t.Errorf("the planted text was dropped instead of defused:\n%s", summary)
		}
	})

	// The did_commit edge pins the alignment this run pushed. The agents ran
	// in the workspace with a shell, so its identity is read from the forge:
	// the stamp is the id of the commit the forge holds, whatever the
	// workspace says.
	t.Run("the pushed alignment is identified from the forge", func(t *testing.T) {
		o := newForgeOrigin(t)
		c := commitTree(t, ws, x, "align", map[string][]byte{"code.txt": []byte("aligned\n")})
		gittest.Run(t, ws, "push", "-q", o.dir, base+":refs/heads/main", c+":refs/heads/renovate/bump")
		want, why := changeIdentity(t, ws, "main", "main", c)
		if want == "" || want == id {
			t.Fatalf("fixture: the alignment has no identity of its own (%q, %s)", want, why)
		}
		p, res := runFeedback(t, &feedbackForge{t: t, gitRoot: o.gitRoot()}, with(map[string]string{
			"{{input.verdict}}": `"committed"`, "{{input.audited_sha}}": quoted(c),
		}))
		note, _ := gateOf(p)["note"].(string)
		if !strings.HasSuffix(note, " · "+want) {
			t.Errorf("the pushed alignment %s was stamped %q, want its own identity %s (summary: %v)", c[:12], note, want, res["summary"])
		}
		// No identity for the head read means the audit's scope was not the
		// true change (a degraded prepare): the alignment on top of it is
		// not stamped either, however well the forge identifies it.
		p, _ = runFeedback(t, &feedbackForge{t: t, gitRoot: o.gitRoot()}, with(map[string]string{
			"{{input.verdict}}": `"committed"`, "{{input.audited_sha}}": quoted(c), "{{input.head_id}}": `""`,
		}))
		if note, _ := gateOf(p)["note"].(string); strings.Contains(note, "pid1:") {
			t.Errorf("an alignment on a head continuity could not identify was stamped: %q", note)
		}
	})
}

// repinFixture: the forge as it stands when an audit of X finishes — the
// base moved, and the pull request's head with it. `same` is X rebased onto
// the new base, `other` another change on it. id is X's identity as
// continuity computed it before any agent ran.
type repinFixture struct {
	ws, x, same, other, base1 string
	id                        string
	origin                    forgeOrigin
}

func newRepinFixture(t *testing.T) repinFixture {
	t.Helper()
	seed := t.TempDir()
	gittest.InitRepo(t, seed)
	base0 := commitTree(t, seed, "HEAD", "base", map[string][]byte{"deps.yaml": depsYAML(nil), "other.txt": []byte("one\n")})
	x := commitTree(t, seed, base0, "bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
	base1 := commitTree(t, seed, base0, "base moves", map[string][]byte{"other.txt": []byte("two\n")})
	same := commitTree(t, seed, base1, "bump, rebased", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
	other := commitTree(t, seed, base1, "another bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v77"})})
	o := newForgeOrigin(t)
	gittest.Run(t, seed, "push", "-q", o.dir, base0+":refs/heads/main", x+":refs/heads/renovate/bump")
	ws := filepath.Join(t.TempDir(), "ws")
	gittest.Run(t, filepath.Dir(ws), "clone", "-q", "--no-tags", o.dir, ws)
	gittest.Run(t, ws, "config", "maintenance.auto", "false")
	gittest.Run(t, ws, "config", "gc.auto", "0")
	gittest.Run(t, ws, "switch", "-q", "--detach", x)
	id, why := changeIdentity(t, ws, "main", "main", x)
	if id == "" {
		t.Fatalf("fixture: X has no identity: %s", why)
	}
	// The forge moves on after the clone.
	gittest.Run(t, seed, "push", "-q", "--force", o.dir, base1+":refs/heads/main", same+":refs/heads/same", other+":refs/heads/other")
	return repinFixture{ws: ws, x: x, same: same, other: other, base1: base1, id: id, origin: o}
}

// repin runs post_feedback for an audit of X whose verdict is `verdict`,
// with the forge reporting `head`.
func (fx repinFixture) repin(t *testing.T, f *feedbackForge, verdict string, env ...string) (map[string]any, map[string]any) {
	t.Helper()
	return fx.repinWith(t, f, verdict, nil, env...)
}

// repinWith is repin with extra script substitutions (a var the operator pins).
func (fx repinFixture) repinWith(t *testing.T, f *feedbackForge, verdict string, over map[string]string, env ...string) (map[string]any, map[string]any) {
	t.Helper()
	f.t, f.base = t, "main"
	if f.gitRoot == "" {
		f.gitRoot = fx.origin.gitRoot()
	}
	subs := map[string]string{
		"{{input.audited_sha}}": quoted(fx.x), "{{input.head_sha}}": quoted(fx.x),
		"{{input.head_id}}": quoted(fx.id), "{{input.verdict}}": quoted(verdict),
		"{{vars.workspace_dir}}": quoted(fx.ws),
	}
	for k, v := range over {
		subs[k] = v
	}
	return runFeedback(t, f, subs, env...)
}

func pinOf(p map[string]any) string {
	g, _ := p["gate"].(map[string]any)
	s, _ := g["audited_sha"].(string)
	return s
}

// TestDepUpdateGuardRepinsAGreenOntoAPureRebase pins arm 1b: an audit that
// finishes after a rebase moved the head publishes `carried` on the new head
// when — and only when — the verdict is green, the new head carries the
// identical change as the FORGE holds it, and the new head's own gate
// history vetoes nothing. A hold is never moved.
//
// Mutations (main.bot `post_feedback`) and the case each reddens: drop the
// head_id == change_id comparison (the different-change case pins a head
// nobody audited); allow the repin for holds (the hold case moves a hold);
// skip the gate-history read (the hold-on-the-new-head and red-on-the-new-
// head cases paint over a verdict about that head); compute the new head's
// identity in the workspace instead of from the forge (the workspace case);
// let the scratch repository read the global git config (the url-rewrite
// case fetches a base the forge never had); parse only Bot-authored markers
// again (the User-authored hold on another head is painted over); drop the
// continuity, the carry_max_age_h or the age term of the switch (that
// switch's case is repinned).
func TestDepUpdateGuardRepinsAGreenOntoAPureRebase(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	t.Run("moved to a pure rebase: the verdict is carried onto the new head", func(t *testing.T) {
		fx := newRepinFixture(t)
		f := &feedbackForge{head: fx.same}
		p, res := fx.repin(t, f, "clean")
		if pinOf(p) != fx.same || res["verdict"] != "carried" || res["pinned_sha"] != fx.same {
			t.Fatalf("want the payload pinned to %s as carried, got pin %s verdict %v pinned_sha %v (%v)",
				fx.same[:12], pinOf(p), res["verdict"], res["pinned_sha"], res["summary"])
		}
		note, _ := p["gate"].(map[string]any)["note"].(string)
		if !strings.Contains(note, "carried from "+fx.x[:12]) {
			t.Errorf("the note does not name the commit actually audited: %q", note)
		}
		summary, _ := p["summary"].(string)
		lines := strings.Split(strings.TrimRight(summary, "\n"), "\n")
		m := markerLine.FindStringSubmatch(lines[len(lines)-1])
		if m == nil || m[1] != fx.same || m[2] != fx.id || m[3] != "carried" || m[4] != fx.x {
			t.Errorf("the marker must name the new head, the carried verdict and the audited origin, got %v", m)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !slices.Contains(f.reads, "/repos/acme/widgets/commits/"+fx.same+"/status") {
			t.Errorf("the repin never read the new head's own status (reads %v): a hold on it would be painted over", f.reads)
		}
	})

	t.Run("moved to a different change: pinned to the audited commit, as before", func(t *testing.T) {
		fx := newRepinFixture(t)
		p, res := fx.repin(t, &feedbackForge{head: fx.other}, "clean")
		if pinOf(p) != fx.x || res["verdict"] != "clean" {
			t.Fatalf("a different change was certified: pin %s verdict %v, want %s clean", pinOf(p), res["verdict"], fx.x[:12])
		}
	})

	// The operator's switches govern the repin exactly as they govern
	// continuity's carry: `continuity` off (the /vetty command, an operator
	// pin) or carry_max_age_h at 0 carries nothing, on either path. Mutation:
	// drop either term of the switch (that case is repinned).
	for _, sw := range []struct{ name, ref, val, says string }{
		{"continuity off", "{{vars.continuity}}", "False", "continuity is off"},
		{"carry_max_age_h=0", "{{vars.carry_max_age_h}}", "0", "carry_max_age_h is 0"},
	} {
		t.Run(sw.name+": a pure rebase is not repinned", func(t *testing.T) {
			fx := newRepinFixture(t)
			f := &feedbackForge{head: fx.same}
			p, res := fx.repinWith(t, f, "clean", map[string]string{sw.ref: sw.val})
			if pinOf(p) != fx.x || res["verdict"] != "clean" {
				t.Fatalf("%s still carried the verdict onto %s: pin %s verdict %v", sw.name, fx.same[:12], pinOf(p), res["verdict"])
			}
			if summary, _ := res["summary"].(string); !strings.Contains(summary, sw.says) {
				t.Errorf("the run does not name the switch that stopped the repin: %q", summary)
			}
		})
	}

	// A carried verdict keeps its ORIGINAL audit's clock across a repin too.
	// Mutation: drop the age term of the switch -> the stale carry moves.
	t.Run("a carried verdict whose original audit is past carry_max_age_h is not repinned", func(t *testing.T) {
		fx := newRepinFixture(t)
		old := time.Now().UTC().Add(-100 * time.Hour).Format("2006-01-02T15:04:05Z")
		p, res := fx.repinWith(t, &feedbackForge{head: fx.same}, "carried", map[string]string{
			"{{input.origin_sha}}": quoted(strings.Repeat("0a", 20)), "{{input.origin_at}}": quoted(old),
		})
		if pinOf(p) != fx.x || res["verdict"] != "carried" {
			t.Fatalf("a carry past its age was carried onto %s: pin %s verdict %v", fx.same[:12], pinOf(p), res["verdict"])
		}
		if summary, _ := res["summary"].(string); !strings.Contains(summary, "older than carry_max_age_h") {
			t.Errorf("the run does not say the original audit is too old: %q", summary)
		}
	})

	t.Run("a hold is never moved", func(t *testing.T) {
		fx := newRepinFixture(t)
		f := &feedbackForge{head: fx.same}
		p, res := fx.repin(t, f, "hold_unstable")
		if pinOf(p) != fx.x || res["verdict"] != "hold_unstable" {
			t.Fatalf("a hold was moved: pin %s verdict %v", pinOf(p), res["verdict"])
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.reads) != 0 {
			t.Errorf("a hold asked the forge for the head (%v): it has no repin to decide", f.reads)
		}
	})

	// The new head's own audit can finish FIRST with a hold. A repin
	// published after it would supersede it, so the repin reads the new
	// head's gate history and stands down — the reviews served in both
	// orders, the answer the same.
	stamp := func(fx repinFixture) string { return " · " + fx.id }
	for _, tc := range []struct {
		name     string
		reviews  func(fx repinFixture) []map[string]any
		statuses func(fx repinFixture) map[string][]map[string]any
	}{
		{
			name: "a hold on the new head, with its marker",
			reviews: func(fx repinFixture) []map[string]any {
				return []map[string]any{
					vettyReview(1, vettyLogin, gateMarker(fx.x, fx.id, "clean", fx.x, "2026-11-10T08:00:00Z")),
					vettyReview(2, vettyLogin, gateMarker(fx.same, fx.id, "hold_unstable", fx.same, "2026-11-10T09:00:00Z")),
				}
			},
			statuses: func(fx repinFixture) map[string][]map[string]any {
				return map[string][]map[string]any{fx.same: {gateStatus("failure", "build/tests not green after alignment"+stamp(fx), reviewURL(2))}}
			},
		},
		{
			// Vetty's own hold on ANOTHER head of the change, published as a
			// PAT connection's User: the repin, which cannot name the App,
			// vetoes on it exactly as continuity does.
			name: "a User-authored hold on another head of the same change",
			reviews: func(fx repinFixture) []map[string]any {
				y1 := strings.Repeat("1", 40)
				return []map[string]any{
					vettyReview(1, vettyLogin, gateMarker(fx.x, fx.id, "clean", fx.x, "2026-11-10T08:00:00Z")),
					userReview(2, "acme-pat-user", vettyID, gateMarker(y1, fx.id, "hold_unstable", y1, "2026-11-10T09:00:00Z")),
				}
			},
			statuses: func(fx repinFixture) map[string][]map[string]any {
				return map[string][]map[string]any{strings.Repeat("1", 40): {gateStatus("failure", "build/tests not green after alignment"+stamp(fx), reviewURL(2))}}
			},
		},
		{
			name:    "a red on the new head, with no marker",
			reviews: func(repinFixture) []map[string]any { return nil },
			statuses: func(fx repinFixture) map[string][]map[string]any {
				return map[string][]map[string]any{fx.same: {gateStatus("failure", "supply-chain risk: the bump was not applied"+stamp(fx), "")}}
			},
		},
	} {
		t.Run(tc.name+": not repinned", func(t *testing.T) {
			fx := newRepinFixture(t)
			for _, order := range []string{"served", "reversed"} {
				reviews := slices.Clone(tc.reviews(fx))
				if order == "reversed" {
					slices.Reverse(reviews)
				}
				p, res := fx.repin(t, &feedbackForge{head: fx.same, reviews: reviews, statuses: tc.statuses(fx)}, "clean")
				if pinOf(p) != fx.x || res["verdict"] != "clean" {
					t.Errorf("%s: a green was repinned over a verdict about the new head: pin %s verdict %v (%v)",
						order, pinOf(p), res["verdict"], res["summary"])
				}
				if summary, _ := res["summary"].(string); !strings.Contains(summary, "not repinned onto "+fx.same[:12]) {
					t.Errorf("%s: the run does not say why it did not repin: %q", order, summary)
				}
			}
		})
	}

	// The agents ran in the workspace with a shell and the forge token: its
	// refs, replace objects and config are theirs. A local base moved onto
	// an unaudited commit makes a head that stacks the same bump on it read
	// as the audited change there — and a replace ref makes any head read as
	// any other. The new head's identity is the forge's, never the
	// workspace's.
	t.Run("the workspace the agents ran in carries nothing: a moved local base, a replace ref", func(t *testing.T) {
		fx := newRepinFixture(t)
		seed := t.TempDir()
		gittest.InitRepo(t, seed)
		gittest.Run(t, seed, "fetch", "-q", fx.origin.dir, "refs/heads/main:refs/forge/main")
		unaudited := commitTree(t, seed, fx.base1, "unaudited", map[string][]byte{"other.txt": []byte("payload\n")})
		z := commitTree(t, seed, unaudited, "bump on it", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
		gittest.Run(t, seed, "push", "-q", fx.origin.dir, z+":refs/heads/z")
		gittest.Run(t, fx.ws, "fetch", "-q", "origin", "refs/heads/z:refs/agent/z", fx.same, fx.other)
		gittest.Run(t, fx.ws, "branch", "-f", "main", unaudited)
		gittest.Run(t, fx.ws, "replace", fx.other, fx.same)
		for _, head := range []string{z, fx.other} {
			p, res := fx.repin(t, &feedbackForge{head: head}, "clean")
			if pinOf(p) != fx.x || res["verdict"] != "clean" {
				t.Errorf("a head the workspace dressed as the audited change (%s) was certified: pin %s verdict %v",
					head[:12], pinOf(p), res["verdict"])
			}
		}
	})

	// A url rewrite in the global git config — a file the agents can write —
	// would fetch the base from a repository of their choosing: there, a
	// malicious base makes the new head look like the audited change.
	t.Run("a global url rewrite fetches nothing from elsewhere", func(t *testing.T) {
		fx := newRepinFixture(t)
		seed := t.TempDir()
		gittest.InitRepo(t, seed)
		gittest.Run(t, seed, "fetch", "-q", fx.origin.dir, "refs/heads/main:refs/forge/main")
		evilBase := commitTree(t, seed, fx.base1, "unaudited", map[string][]byte{"other.txt": []byte("payload\n")})
		z := commitTree(t, seed, evilBase, "bump on it", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
		gittest.Run(t, seed, "push", "-q", fx.origin.dir, z+":refs/heads/z")
		evil := newForgeOrigin(t)
		gittest.Run(t, seed, "push", "-q", evil.dir, evilBase+":refs/heads/main", z+":refs/heads/z")
		global := filepath.Join(t.TempDir(), "gitconfig")
		gittest.Run(t, seed, "config", "--file", global, "url."+evil.gitRoot()+".insteadOf", fx.origin.gitRoot())
		p, res := fx.repin(t, &feedbackForge{head: z}, "clean", "GIT_CONFIG_GLOBAL="+global)
		if pinOf(p) != fx.x || res["verdict"] != "clean" {
			t.Fatalf("a base fetched from a rewritten URL certified %s: pin %s verdict %v", z[:12], pinOf(p), res["verdict"])
		}
	})
}

// TestDepUpdateGuardRepinFetchCarriesTheTokenByEnvironment: the repin's
// fetch from the forge needs the token, and it reaches git through its
// ENVIRONMENT — never its argv, never the node's output. The forge's git
// endpoint is a TLS server that records what git sends; a git shim records
// every argv.
//
// Mutations (main.bot `post_feedback`): pass the header as `-c
// http.extraHeader=…` on the command line (the argv assertion reddens); drop
// the header (the server assertion reddens).
func TestDepUpdateGuardRepinFetchCarriesTheTokenByEnvironment(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	const token = "ghs_repinSecretToken1234567890"
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	fx := newRepinFixture(t)
	tokenPath := filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim, argv := t.TempDir(), filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argv + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range gittest.Env() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(name, "_PROXY") || name == "PATH" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"), "GIT_SSL_NO_VERIFY=1")

	f := &feedbackForge{t: t, head: fx.same, base: "main", gitRoot: srv.URL + "/"}
	srvAPI := httptest.NewServer(f)
	defer srvAPI.Close()
	body := toolScript(t, depUpdateGuardBot, "post_feedback")
	for ref, val := range map[string]string{
		"{{input.pr_url}}": `"https://github.com/acme/widgets/pull/7"`, "{{input.verdict}}": `"clean"`,
		"{{input.audit_summary}}": `""`, "{{input.cves}}": `""`, "{{input.malware_signals}}": `""`,
		"{{input.align_summary}}": `""`, "{{input.validate_summary}}": `""`, "{{input.escalation}}": `""`,
		"{{input.commit_summary}}": `""`, "{{input.audited_sha}}": quoted(fx.x), "{{input.origin_sha}}": `""`,
		"{{input.origin_at}}": `""`, "{{secrets.forge_token.path}}": quoted(tokenPath),
		"{{vars.forge_publish_url}}": quoted(srvAPI.URL + "/api/v1/forge/publish-review"),
		"{{run.url}}":                `""`, "{{run.id}}": `"01a0dep"`, "{{vars.gate_enabled}}": "True",
		"{{vars.gate_context}}": `"vetty/deps"`, "{{vars.base_ref}}": `"main"`,
		"{{input.head_sha}}": quoted(fx.x), "{{input.head_id}}": quoted(fx.id),
		"{{vars.continuity}}": "True", "{{vars.carry_max_age_h}}": "72",
	} {
		body = strings.ReplaceAll(body, ref, val)
	}
	body = strings.ReplaceAll(body, `"https://api.github.com"`, quoted(srvAPI.URL))
	body = strings.ReplaceAll(body, `"https://github.com/"`, quoted(f.gitRoot))
	if strings.Contains(body, "{{") {
		t.Fatalf("unsubstituted ref: %s", firstRef(body))
	}
	path := filepath.Join(t.TempDir(), "feedback.py")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", path)
	cmd.Env = append(env, secretguard.PlaceholderForName(store.ForgePublishTokenVar)+"=run-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("post_feedback failed: %v (out %q)", err, out)
	}
	if pinOf(f.payload) != fx.x {
		t.Errorf("a forge that refused every read let the verdict move: pin %s", pinOf(f.payload))
	}
	mu.Lock()
	seen := slices.Clone(auth)
	mu.Unlock()
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	if !slices.Contains(seen, "Basic "+encoded) {
		t.Errorf("the forge's git endpoint never received the token as an Authorization header (saw %q)", seen)
	}
	logged, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("the git shim recorded nothing: %v", err)
	}
	if !strings.Contains(string(logged), "fetch") {
		t.Fatalf("the shim did not see the repin's fetch:\n%s", logged)
	}
	if strings.Contains(string(logged), token) || strings.Contains(string(logged), encoded) {
		t.Errorf("the forge token reached git's argv:\n%s", logged)
	}
	if strings.Contains(string(out), token) || strings.Contains(string(out), encoded) {
		t.Errorf("the forge token reached the node's output: %s", out)
	}
}
