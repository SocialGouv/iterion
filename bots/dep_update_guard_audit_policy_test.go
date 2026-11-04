package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Vetty's CVE audit policy is prose a model grades by, so no test can prove the
// verdict it produces; what a test can do is make every change to that prose a
// reviewed decision. Frozen below: both security_audit prompts whole, the align
// prompt's untrusted-input boundary and PR-context header, the playbook's audit
// section, and the catalogue passages on auditors that fail, on the
// vulnerability auditors and on nixpkgs pins.
//
// The rule they state: a bump is held only for a HIGH/CRITICAL advisory it
// INTRODUCES. A vulnerability advisory on both pins is REMAINING — reported
// with the release that fixes it, never held, since a hold keeps the old pin,
// which carries it too (buildkit-operator#55 resolved eleven advisories,
// introduced none, and was held). What keeps that rule from opening the gate
// is spread over the same passages: a malware advisory is never sorted with
// the vulnerabilities; an introduced advisory counts at the highest severity
// any record gives, and as HIGH when none scores it; the sets are read from
// the record checked against both pins, with the OSV id and version spelled
// the way OSV parses them; the PR body is untrusted DATA; a scanner is
// unavailable only after a Bash call that failed. Two review rounds broke a
// phrase-list sentinel by editing one word of those sentences, so the gate is
// the passages themselves.
const depUpdateGuardAuditPolicyFreeze = "testdata/dep_update_guard_audit_policy.txt"

// depUpdateGuardAuditPolicyOutEnv names the file the generator writes, and is
// the ONE way the frozen text is produced.
const depUpdateGuardAuditPolicyOutEnv = "ITERION_VETTY_AUDIT_POLICY_OUT"

// depUpdateGuardAuditPolicyGenTask is the command that sets it.
const depUpdateGuardAuditPolicyGenTask = "task bots:audit-policy:gen"

var depUpdateGuardAuditPolicyHeader = []string{
	"# Vetty's CVE audit policy, frozen: the passages of the security_audit and",
	"# align prompts and of the playbooks they defer to that decide what holds a",
	"# dependency bump. Generated, never hand-edited — the generator is in",
	"# bots/dep_update_guard_audit_policy_test.go, run by `task bots:audit-policy:gen`.",
	"# A diff of this file is a change to what the audit holds: review it as one.",
}

// depUpdateGuardAuditPolicy reads the frozen passages from the shipped texts,
// one `== <source>: <from> → <to>` section each. A marker that no longer
// matches is fatal: a passage that moved out of reach would otherwise drop out
// of the freeze in silence.
func depUpdateGuardAuditPolicy(t *testing.T) string {
	t.Helper()
	cr := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File)
	prompt := func(name string) string {
		p, ok := cr.Workflow.Prompts[name]
		if !ok || p == nil {
			t.Fatalf("prompt %s is not declared", name)
		}
		return p.Body
	}
	file := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	audit, auditUser, align := prompt("security_audit_system"), prompt("security_audit_user"), prompt("align_system")
	guard := file("dep-update-guard/skills/dependency-pr-guard.md")
	managers := file("dep-update-guard/skills/package-managers.md")
	sections := []struct{ source, body, from, to string }{
		{"prompt security_audit_system", audit, "", ""},
		{"prompt security_audit_user", auditUser, "", ""},
		{"prompt align_system", align, "IMPORTANT — UNTRUSTED INPUT BOUNDARY:", "You run on claude_code"},
		{"prompt align_system", align, "### PR context (", ""},
		{"skill dependency-pr-guard.md", guard, "## 2. Supply-chain / security audit", "## 3. Align the consuming code"},
		{"skill package-managers.md", managers, "4. **Adapt**", "5. **Cover gaps**"},
		{"skill package-managers.md", managers, "## Vulnerability audit", "## Shim fallbacks"},
		{"skill package-managers.md", managers, "No scanner reads a nixpkgs pin", "### Helm charts"},
	}
	var out []string
	for _, s := range sections {
		i := strings.Index(s.body, s.from)
		if i < 0 {
			t.Fatalf("%s no longer has the passage starting %q", s.source, s.from)
		}
		end := len(s.body)
		if s.to != "" {
			j := strings.Index(s.body[i:], s.to)
			if j < 0 {
				t.Fatalf("%s no longer ends the passage %q at %q", s.source, s.from, s.to)
			}
			end = i + j
		}
		from, to := s.from, s.to
		if from == "" {
			from = "start"
		}
		if to == "" {
			to = "end"
		}
		out = append(out, "== "+s.source+": "+from+" → "+to)
		for _, line := range strings.Split(strings.TrimRight(s.body[i:end], " \n"), "\n") {
			out = append(out, strings.TrimRight(line, " "))
		}
	}
	return strings.Join(append(append([]string{}, depUpdateGuardAuditPolicyHeader...), out...), "\n") + "\n"
}

// TestRecordTheDepUpdateGuardAuditPolicy writes the frozen text from the
// shipped one, and is the whole generator.
func TestRecordTheDepUpdateGuardAuditPolicy(t *testing.T) {
	out := strings.TrimSpace(os.Getenv(depUpdateGuardAuditPolicyOutEnv))
	if out == "" {
		t.Skipf("the generator writes nowhere: run %s", depUpdateGuardAuditPolicyGenTask)
	}
	if err := os.WriteFile(out, []byte(depUpdateGuardAuditPolicy(t)), 0o644); err != nil {
		t.Fatalf("write the policy to %s=%s: %v", depUpdateGuardAuditPolicyOutEnv, out, err)
	}
}

// TestDepUpdateGuardAuditPolicyIsTheFrozenOne compares the shipped passages to
// the frozen text, byte for byte.
func TestDepUpdateGuardAuditPolicyIsTheFrozenOne(t *testing.T) {
	// The generator writes the file this gate reads, and `go test ./bots/` runs
	// it first: refused rather than judged against what the same run produced.
	if out := strings.TrimSpace(os.Getenv(depUpdateGuardAuditPolicyOutEnv)); out != "" {
		t.Fatalf("%s is set: this gate would read back what the generator just wrote", depUpdateGuardAuditPolicyOutEnv)
	}
	want, err := os.ReadFile(depUpdateGuardAuditPolicyFreeze)
	if err != nil {
		t.Fatal(err)
	}
	got := depUpdateGuardAuditPolicy(t)
	if got == string(want) {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("Vetty's audit policy changed at line %d of %s — a change to what the audit holds is a decision: regenerate (%s) and review the diff.\n shipped: %q\n  frozen: %q",
				i+1, depUpdateGuardAuditPolicyFreeze, depUpdateGuardAuditPolicyGenTask, g, w)
		}
	}
	t.Fatalf("%s differs from the shipped passages (%d bytes frozen, %d shipped) outside any line: regenerate (%s)",
		depUpdateGuardAuditPolicyFreeze, len(want), len(got), depUpdateGuardAuditPolicyGenTask)
}

// TestDepUpdateGuardAuditTextsDropTheOldHold bans the wordings of the old rule
// anywhere in the texts the auditor and the align agent read, outside the
// frozen passages too: the old hold put back in another paragraph is the
// same coin flip between two rules.
func TestDepUpdateGuardAuditTextsDropTheOldHold(t *testing.T) {
	cr := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File)
	fold := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, text := range []struct {
		name, body string
		gone       []string
	}{
		{"prompt security_audit_system", cr.Workflow.Prompts["security_audit_system"].Body, []string{
			"unresolved", "on the new version with no fix", "when unsure between safe and suspicious", "pr context (operator)",
		}},
		{"prompt security_audit_user", cr.Workflow.Prompts["security_audit_user"].Body, []string{
			"unresolved", "on the new version with no fix", "when unsure between safe and suspicious",
		}},
		{"prompt align_system", cr.Workflow.Prompts["align_system"].Body, []string{
			"pr context (operator)", "operator's scope_notes",
		}},
		{"skill dependency-pr-guard.md", read("dep-update-guard/skills/dependency-pr-guard.md"), []string{
			"unresolved high/critical", "on the new version with no fix", "lean toward `suspicious`",
		}},
		{"skill package-managers.md", read("dep-update-guard/skills/package-managers.md"), []string{
			"unresolved high/critical", "on the new version with no fix", "lean toward `suspicious`",
		}},
	} {
		flat := fold(text.body)
		for _, g := range text.gone {
			if strings.Contains(flat, g) {
				t.Errorf("%s still carries %q", text.name, g)
			}
		}
	}
}
