package bots

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A finding the audit sets aside — covered by a scanner exclusion the base
// declares, or found only in a source tree nothing installs (#3817: two
// "introduced" advisories located only in a nixpkgs source-tree Cargo.lock
// held a bump that removed 490) — holds nothing, and is never silent. Whether
// the model classifies a given finding that way is graded prose, frozen by
// TestDepUpdateGuardAuditPolicyIsTheFrozenOne and provable only live. What is
// deterministic is the reporting half: the audit has a typed field for the
// set-aside findings, every edge that publishes an audited verdict carries
// it, and post_feedback renders every entry on that verdict.

// TestDepUpdateGuardExcludedFindingsReachEveryAuditedVerdict pins the wiring:
// the audit's output schema declares the field its prompt asks for, and every
// edge into post_feedback that carries the audit's summary also carries its
// excluded findings — a verdict whose comment says what the audit read but not
// what it set aside drops those findings in silence.
//
// Mutations: drop `excluded_findings` from `audit_output` (the schema
// assertion reddens); drop its mapping from one audited edge into
// post_feedback (the edge assertion reddens, naming that edge).
func TestDepUpdateGuardExcludedFindingsReachEveryAuditedVerdict(t *testing.T) {
	cr := ir.Compile(parseBotUnit(depUpdateGuardBot).File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	declared := func(schema, field string, want ir.FieldType) {
		s, ok := cr.Workflow.Schemas[schema]
		if !ok {
			t.Fatalf("schema %s is not declared", schema)
		}
		for _, f := range s.Fields {
			if f.Name == field {
				if f.Type != want {
					t.Errorf("%s.%s is %v, want %v", schema, field, f.Type, want)
				}
				return
			}
		}
		t.Errorf("%s declares no %s field: the audit has nowhere to put a finding it sets aside", schema, field)
	}
	declared("audit_output", "excluded_findings", ir.FieldTypeStringArray)
	declared("feedback_input", "excluded_findings", ir.FieldTypeString)
	if p := cr.Workflow.Prompts["security_audit_system"]; p == nil || !strings.Contains(p.Body, "`excluded_findings`") {
		t.Errorf("the security_audit prompt never names `excluded_findings`, the field the schema gives it")
	}

	audited := 0
	for _, e := range cr.Workflow.Edges {
		if e.To != "post_feedback" {
			continue
		}
		mapped := map[string]string{}
		for _, m := range e.With {
			mapped[m.Key] = strings.Trim(m.Raw, `"`)
		}
		if !strings.Contains(mapped["audit_summary"], "outputs.security_audit.") {
			continue
		}
		audited++
		if got, want := mapped["excluded_findings"], "{{outputs.security_audit.excluded_findings}}"; got != want {
			t.Errorf("%s -> post_feedback (when %s) publishes an audited verdict without the audit's excluded findings: excluded_findings = %q, want %q",
				e.From, e.Condition, got, want)
		}
	}
	// hold_security, needs_decision, hold_delivery_permission, hold_unstable,
	// hold_lost_alignment, committed, clean: every audited verdict.
	if audited != 7 {
		t.Errorf("found %d audited edges into post_feedback, want 7 — a new verdict edge owes the audit's excluded findings too", audited)
	}
}

// TestDepUpdateGuardRendersEveryExcludedFinding drives the shipped
// post_feedback: each excluded finding, its location and its reason reach the
// published comment, on a hold and on a green verdict, whether the engine hands
// the list whole or as its text; an empty list renders no section.
//
// Mutation: drop the line that appends the excluded findings to the security
// section (every non-empty case reddens on its missing entry).
func TestDepUpdateGuardRendersEveryExcludedFinding(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	entries := []string{
		"GHSA-xxxx-hash hashbrown@0.15.0 — /nix/store/abc-nixpkgs/nixpkgs/pkgs/by-name/bb/bbox/Cargo.lock — skip-dirs /nix/store/*-nixpkgs, .github/workflows/trivy.yml:166 (base)",
		"GHSA-yyyy-zip zip@2.2.0 — /nix/store/abc-nixpkgs/nixpkgs/pkgs/by-name/bb/bbox/Cargo.lock — nixpkgs source recipes, nothing installed from them",
	}
	list, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	asText, err := json.Marshal(string(list))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, verdict, value string
		want                 []string
	}{
		{"a hold, the list whole", "hold_security", string(list), entries},
		{"a green verdict, the list whole", "clean", string(list), entries},
		{"a committed verdict, the list as text", "committed", string(asText), entries},
		{"an empty list renders no section", "hold_security", `[]`, nil},
		{"an empty string renders no section", "clean", `""`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := runFeedback(t, &feedbackForge{t: t, head: strings.Repeat("ab", 20), base: "main"}, map[string]string{
				"{{input.verdict}}":           fmt.Sprintf("%q", tc.verdict),
				"{{input.excluded_findings}}": tc.value,
			})
			body, _ := payload["summary"].(string)
			if body == "" {
				t.Fatalf("published no comment body: %v", payload)
			}
			const heading = "Not held — excluded from the verdict"
			if len(tc.want) == 0 {
				if strings.Contains(body, heading) {
					t.Errorf("an empty exclusion list rendered a section:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, heading) {
				t.Errorf("the comment does not say what the audit set aside:\n%s", body)
			}
			for _, e := range tc.want {
				if !strings.Contains(body, "- "+e) {
					t.Errorf("the comment drops the excluded finding %q:\n%s", e, body)
				}
			}
		})
	}
}
