package ir

import (
	"strings"
	"testing"
)

const timeoutFromEnvSrc = `
prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  system: sys
  user: usr
  timeout: "%s"

workflow test:
  entry: a1
  a1 -> done
`

// A diagnostic about a field `${…}` expanded names the field as the author
// wrote it and never the value the expansion read: that value may be any
// variable of the environment the compiler runs in — a server's, for
// POST /api/validate.
func TestATimeoutDiagnosticNeverQuotesTheExpansion(t *testing.T) {
	const value = "expansion-echo-probe-value"
	t.Setenv("PROBE_TIMEOUT_TEXT", value)
	r := compileFile(t, strings.Replace(timeoutFromEnvSrc, "%s", "${PROBE_TIMEOUT_TEXT}", 1))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidNodeTimeout {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatal("no C122 for a timeout that expands to a non-duration")
	}
	if strings.Contains(msg, value) {
		t.Fatalf("the diagnostic quotes the expanded value: %q", msg)
	}
	if !strings.Contains(msg, "${PROBE_TIMEOUT_TEXT}") {
		t.Errorf("the diagnostic does not name the field as written: %q", msg)
	}

	// A literal the author typed keeps the parser's own reason.
	r = compileFile(t, strings.Replace(timeoutFromEnvSrc, "%s", "2h3Om", 1))
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidNodeTimeout && !strings.Contains(d.Message, "time: ") {
			t.Errorf("a literal's diagnostic lost the parser's reason: %q", d.Message)
		}
	}
}

// A launch's budget override is refused without quoting what its expansion read.
func TestABudgetOverrideErrorNeverQuotesTheExpansion(t *testing.T) {
	const value = "budget-echo-probe-value"
	t.Setenv("PROBE_BUDGET_TEXT", value)
	err := BudgetOverrides{MaxDuration: "${PROBE_BUDGET_TEXT}"}.Validate()
	if err == nil {
		t.Fatal("a max_duration expanding to a non-duration was accepted")
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("the error quotes the expanded value: %v", err)
	}
	if err := (BudgetOverrides{MaxDuration: "2h3Om"}).Validate(); err == nil || !strings.Contains(err.Error(), "time: ") {
		t.Errorf("a literal's error lost the parser's reason: %v", err)
	}
}
