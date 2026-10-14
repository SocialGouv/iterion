package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
			"{{vars.forge_publish_token}}": `"run-token"`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
			"{{vars.gate_enabled}}":        boolLit(gateEnabled),
			"{{vars.gate_context}}":        `"` + gateContext + `"`,
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
		got, _ := run(t, "clean", "iterion/review", true, "")
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
		got, _ := run(t, "hold_lost_alignment", "iterion/review", true, "")
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
			"{{vars.forge_publish_token}}": `"run-token"`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
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
			"{{vars.forge_publish_token}}": `""`,
			"{{run.url}}":                  runURLLit(runPage, runPageLit),
			"{{run.id}}":                   `"` + runID + `"`,
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
