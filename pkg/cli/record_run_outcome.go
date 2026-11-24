package cli

import (
	"strings"
	"time"
)

// FormatRunOutcome renders the outcome tuple a record carries for the last
// run it launched — `last_run_status` / `last_run_error_code` /
// `last_run_at`, stamped by the run-outcome spine (#1501) onto a board card,
// a trigger subscription and a cloud schedule alike.
//
// One formatter for every CLI surface of the class, so the same record cannot
// read "failed" in one command and "failure" in the next: the status is the
// wire value the store holds (a store.RunStatus string), the code is the
// stable machine-readable classifier when the run carries one, and the
// instant is the ordering key the stamp used. An empty status means no
// terminal outcome has been recorded yet and renders as the empty string —
// the caller prints nothing rather than a dash that would read as a verdict.
func FormatRunOutcome(status, errCode string, at *time.Time) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}
	parts := []string{status}
	if code := strings.TrimSpace(errCode); code != "" {
		parts = append(parts, code)
	}
	if at != nil && !at.IsZero() {
		parts = append(parts, FormatTime(at.UTC()))
	}
	return strings.Join(parts, " · ")
}
