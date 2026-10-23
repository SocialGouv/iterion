package parser

import (
	"fmt"
	"strings"
	"testing"
)

func diag(sev Severity, msg string) Diagnostic {
	return Diagnostic{Code: DiagExpectedToken, Severity: sev, Message: msg}
}

func coded(code DiagCode, sev Severity, msg string) Diagnostic {
	return Diagnostic{Code: code, Severity: sev, Message: msg}
}

// A report keeps the first MaxReported of each code, errors and warnings
// apart: a flood of one code leaves room for every other code, and its last
// one kept says how many more there were.
func TestReportKeepsTheFirstOfEachCode(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	var r Report
	for i := 0; i < MaxReported+5; i++ {
		r.Add(coded(DiagExpectedToken, SeverityWarning, "w"))
	}
	if !r.Add(coded(DiagExpectedToken, SeverityError, "e")) {
		t.Fatal("an error of a code whose warnings went past the cap was not kept")
	}
	for i := 0; i < MaxReported+1; i++ {
		r.Add(coded(DiagUnknownProperty, SeverityError, "u"))
	}
	if !r.Add(coded(DiagDirectiveInProfile, SeverityError, "d")) {
		t.Fatal("an error of another code, after a flood of one code, was not kept")
	}
	if got := r.Count(SeverityWarning); got != MaxReported+5 {
		t.Errorf("Count(warning) = %d, want %d: a count past the cap is still exact", got, MaxReported+5)
	}
	if got := r.Count(SeverityError); got != MaxReported+3 {
		t.Errorf("Count(error) = %d, want %d", got, MaxReported+3)
	}
	ds := r.Diagnostics()
	if len(ds) != 2*MaxReported+2 {
		t.Fatalf("kept %d, want %d", len(ds), 2*MaxReported+2)
	}
	if !strings.HasSuffix(ds[MaxReported-1].Message, "— and 5 more E002 warnings not reported: a reading reports 1000 of each code and 2000 of each severity, every code's first kept") {
		t.Errorf("the last warning kept says %q", ds[MaxReported-1].Message)
	}
	if ds[MaxReported].Message != "e" {
		t.Errorf("an error under the cap is told as it was added: %q", ds[MaxReported].Message)
	}
	if !strings.HasSuffix(ds[2*MaxReported].Message, "— and 1 more E012 error not reported: a reading reports 1000 of each code and 2000 of each severity, every code's first kept") {
		t.Errorf("the last E012 kept says %q", ds[2*MaxReported].Message)
	}
	if ds[2*MaxReported+1].Message != "d" {
		t.Errorf("the other code's error is told as it was added: %q", ds[2*MaxReported+1].Message)
	}
}

// Diagnostics leaves the report as it was: asked twice, it says the count
// once each time.
func TestReportDiagnosticsIsRepeatable(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	var r Report
	for i := 0; i < MaxReported+2; i++ {
		r.Add(diag(SeverityError, "e"))
	}
	first, second := r.Diagnostics(), r.Diagnostics()
	if first[MaxReported-1].Message != second[MaxReported-1].Message || strings.Count(second[MaxReported-1].Message, "more E002 errors") != 1 {
		t.Errorf("the count is said once per call: %q then %q", first[MaxReported-1].Message, second[MaxReported-1].Message)
	}
}

// Latest is the diagnostic the latest Add kept — nil when that one was only
// counted, so a site completing its own refusal never rewrites another's.
func TestReportLatestIsTheOneJustKept(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	var r Report
	if r.Latest() != nil {
		t.Fatal("an empty report has a latest diagnostic")
	}
	r.Add(diag(SeverityError, "first"))
	if d := r.Latest(); d == nil || d.Message != "first" {
		t.Fatalf("Latest after one Add = %v", d)
	}
	for i := 1; i < MaxReported; i++ {
		r.Add(diag(SeverityError, "e"))
	}
	r.Add(diag(SeverityError, "counted"))
	if d := r.Latest(); d != nil {
		t.Errorf("Latest after an Add past the cap = %q, want nil", d.Message)
	}
	r.Add(diag(SeverityWarning, "w"))
	if d := r.Latest(); d == nil || d.Message != "w" {
		t.Errorf("Latest after a kept warning = %v", d)
	}
}

// An empty report has no diagnostics at all — nil, as a reading that found
// nothing always returned.
func TestEmptyReportIsNil(t *testing.T) {
	var r Report
	if r.Diagnostics() != nil {
		t.Error("an empty report returns a non-nil slice")
	}
}

// The operator who removes the read budget removes the cap with it: every
// diagnostic is reported, and none says more were left out.
func TestNoReadBudgetReportsEverything(t *testing.T) {
	t.Setenv(readBudgetEnv, "0")
	var r Report
	for i := 0; i < MaxReported+7; i++ {
		r.Add(diag(SeverityError, "e"))
	}
	ds := r.Diagnostics()
	if len(ds) != MaxReported+7 {
		t.Fatalf("with no budget the report kept %d of %d", len(ds), MaxReported+7)
	}
	for _, d := range ds {
		if d.Message != "e" {
			t.Fatalf("with no budget a diagnostic says %q", d.Message)
		}
	}
}

// A reading keeps at most MaxReportedOfASeverity errors in all, whatever
// their codes: three codes of 1500 errors each keep 1000, 1000 and — past
// the 2000 — the third code's first alone, which says how many more of it
// there were, a decisive code never hidden behind the others. The count
// stays exact, and with no read budget nothing is capped.
func TestAReadingKeepsTwoThousandOfASeverity(t *testing.T) {
	t.Setenv(ReadBudgetEnv, "")
	var r Report
	for _, code := range []DiagCode{"E002", "E012", "E042"} {
		for i := 0; i < 1500; i++ {
			r.Add(Diagnostic{Code: code, Severity: SeverityError, Message: string(code)})
		}
	}
	for i := 0; i < 3; i++ {
		r.Add(Diagnostic{Code: "E002", Severity: SeverityWarning, Message: "w"})
	}
	kept := map[DiagCode]int{}
	var last Diagnostic
	for _, d := range r.Diagnostics() {
		if d.Severity == SeverityError {
			kept[d.Code]++
			if d.Code == "E042" {
				last = d
			}
		}
	}
	if kept["E002"] != MaxReported || kept["E012"] != MaxReported || kept["E042"] != 1 {
		t.Fatalf("kept %v, want 1000, 1000 and the third code's first", kept)
	}
	if !strings.HasSuffix(last.Message, "— and 1499 more E042 errors not reported: a reading reports 1000 of each code and 2000 of each severity, every code's first kept") {
		t.Errorf("the third code's one kept says %q", last.Message)
	}
	if got := r.Count(SeverityError); got != 4500 {
		t.Errorf("Count(error) = %d, want 4500", got)
	}
	if n := len(r.Diagnostics()) - 2001; n != 3 {
		t.Errorf("the warnings kept: %d, want 3 — a severity's cap is its own", n)
	}
	t.Setenv(ReadBudgetEnv, "0")
	var none Report
	for i := 0; i < 2500; i++ {
		none.Add(Diagnostic{Code: DiagCode(fmt.Sprintf("E%d", 100+i%3)), Severity: SeverityError})
	}
	if got := len(none.Diagnostics()); got != 2500 {
		t.Errorf("with no budget the report keeps %d of 2500", got)
	}
}
