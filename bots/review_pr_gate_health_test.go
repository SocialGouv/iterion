package bots

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// A required status that never lands blocks a pull request for good, and the
// bundle already paid for that once (Revi 0.5.6). Since 0.9.7 pins the verdict
// to the revision it judged, the server REFUSES a stale pin — so "the gate did
// not land" is now a routine outcome, and the run must say so instead of
// reporting the publish as healthy.
//
// The subtle half is WHICH exits have to say it. `gate_requested` defaults to
// false, so every emit() that forgets it makes this guard blind on its own
// path — and the paths that forget are exactly the failure ones, where the gate
// most certainly did not land. On a clean review (0 findings) the older
// `total > 0 && posted == 0` predicate is false too, so nothing at all fires.
//
// Mutations that must redden this: drop `gate_requested=gate_enabled` from any
// of the three failure exits in publish_review, or drop `gate_missing` from
// publish_health's `degraded`.
func TestReviewPRPublishHealthSeesALostGateOnEveryExit(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "review-pr/main.bot", "publish_health")

	// The node reads its inputs through env assignments on the command head, so
	// the values are substituted into the command and the whole thing is run by
	// sh -c — the same way it runs in production. Asserting on the template text
	// would prove nothing about what the shell and python actually see.
	run := func(t *testing.T, over map[string]string) (bool, string) {
		t.Helper()
		vals := map[string]string{
			"total_findings": "0", "comments_posted": "0", "published": "false",
			"review_url": "", "skipped_reason": "",
			"gate_requested": "false", "gate_posted": "false", "gate_skipped_reason": "",
		}
		for k, v := range over {
			vals[k] = v
		}
		rendered := cmdTemplate
		for k, v := range vals {
			rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", "'"+v+"'")
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
		}
		out, err := exec.Command("sh", "-c", rendered).Output()
		if err != nil {
			t.Fatalf("publish_health failed: %v (out %q)", err, out)
		}
		if len(out) == 0 {
			t.Fatal("publish_health produced EMPTY output — the command body is truncated by a stray metacharacter")
		}
		var res struct {
			Degraded bool   `json:"degraded"`
			Banner   string `json:"banner"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not publish_health_output JSON: %v (%q)", err, out)
		}
		return res.Degraded, res.Banner
	}

	// The case the older predicate cannot see: a CLEAN review (nothing to post)
	// whose gate did not land. Both counts are zero, so only the gate half can
	// fire — and if it does not, the run reads as healthy.
	t.Run("a clean review whose gate was requested and never landed is degraded", func(t *testing.T) {
		degraded, banner := run(t, map[string]string{
			"gate_requested": "true", "gate_posted": "false",
			"gate_skipped_reason": "forge publish endpoint unreachable",
		})
		if !degraded {
			t.Errorf("a required check that never landed must not read as a healthy run, got banner %q", banner)
		}
		if !strings.Contains(banner, "MERGE GATE NOT POSTED") {
			t.Errorf("the banner must name what is missing, got %q", banner)
		}
		if !strings.Contains(banner, "unreachable") {
			t.Errorf("the banner must carry the REASON it was given, got %q", banner)
		}
	})

	// The negatives, so the predicate is not simply always-true.
	t.Run("a gate that landed is not degraded", func(t *testing.T) {
		degraded, banner := run(t, map[string]string{"gate_requested": "true", "gate_posted": "true"})
		if degraded {
			t.Errorf("a posted gate must not be reported lost: %q", banner)
		}
	})
	t.Run("no gate requested is not degraded", func(t *testing.T) {
		degraded, banner := run(t, map[string]string{"gate_requested": "false", "gate_posted": "false"})
		if degraded {
			t.Errorf("an advisory-only deployment has no gate to lose: %q", banner)
		}
	})

	// The half the test above CANNOT see, and the one the finding was about.
	// Driving publish_health directly proves its predicate; it proves nothing
	// about whether publish_review's FAILURE exits fill gate_requested at all.
	// Measured: dropping the flag from the unreachable-endpoint exit left the
	// predicate test green. So this drives publish_review itself, on the exit
	// where the gate most certainly did not land, and reads what it emits.
	t.Run("publish_review says a gate was expected on its failure exits", func(t *testing.T) {
		pub := toolCommand(t, "review-pr/main.bot", "publish_review")
		// An address nothing listens on: the endpoint-unreachable exit.
		vals := map[string]string{
			"{{vars.forge_publish_url}}":   "'http://127.0.0.1:9/api/v1/forge/publish-review'",
			"{{vars.forge_publish_token}}": "'run-token'",
			"{{vars.gate_enabled}}":        "'true'",
			"{{input.pr_url}}":             "'https://github.com/acme/widgets/pull/7'",
			"{{input.findings}}":           "'[]'",
		}
		rendered := pub
		for ref, v := range vals {
			rendered = strings.ReplaceAll(rendered, ref, v)
		}
		// Everything else is irrelevant to this path; blank it so the shell is happy.
		for strings.Contains(rendered, "{{") {
			r := firstRef(rendered)
			rendered = strings.ReplaceAll(rendered, r, "''")
		}
		out, err := exec.Command("sh", "-c", rendered).Output()
		if err != nil {
			t.Fatalf("publish_review failed: %v (out %q)", err, out)
		}
		var res struct {
			Published     bool   `json:"published"`
			GateRequested bool   `json:"gate_requested"`
			Skipped       string `json:"skipped_reason"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
		}
		if res.Published {
			t.Fatalf("an unreachable endpoint must not report a publish: %+v", res)
		}
		if !res.GateRequested {
			t.Errorf("the publish failed with the gate enabled, so gate_requested MUST be true — otherwise publish_health is blind on exactly this path (skipped=%q)", res.Skipped)
		}
	})

	// The inline-anchor guard the node had before must keep working: this test
	// would otherwise pass while having replaced one predicate with the other.
	t.Run("findings that landed no comment is still degraded", func(t *testing.T) {
		degraded, banner := run(t, map[string]string{"total_findings": "4", "comments_posted": "0"})
		if !degraded {
			t.Errorf("4 findings and 0 comments must stay degraded: %q", banner)
		}
		if !strings.Contains(banner, "FORGE PUBLISH FAILED") {
			t.Errorf("the anchor guard keeps its own banner, got %q", banner)
		}
	})
}
