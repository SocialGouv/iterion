package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The manual half of the forfait-reset recovery (#2247): a usage-blocked
// sweep reads exactly failed_resumable + USAGE_LIMIT_BLOCKED, resumes each
// run through the normal resume endpoint, and one failed resume does not
// stop the batch. Mutation: make the sweep send a different status filter
// (or drop the failure_code guard) and these tests redden.
func TestRemoteRunsResumeUsageBlocked_ResumesEachBlockedRun(t *testing.T) {
	var resumes []string
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/runs" && r.Method == http.MethodGet:
			if r.URL.Query().Get("status") != "failed_resumable" {
				t.Errorf("list status = %q, want failed_resumable", r.URL.Query().Get("status"))
			}
			fmt.Fprint(w, `{"runs":[`+
				`{"id":"r1","workflow_name":"review_pr","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"},`+
				`{"id":"r2","workflow_name":"review_pr","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:31:00Z"},`+
				`{"id":"r3","workflow_name":"other","status":"failed_resumable","failure_code":"","created_at":"2026-10-06T11:32:00Z"}`+
				`]}`)
		case strings.HasPrefix(r.URL.Path, "/api/runs/") && strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPost:
			resumes = append(resumes, r.URL.Path)
			fmt.Fprint(w, `{"status":"queued"}`)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsResumeUsageBlocked(context.Background(), c, p, cli.RemoteRunsListOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(resumes) != 2 {
		t.Fatalf("resumes = %v, want exactly r1 and r2", resumes)
	}
	out := buf.String()
	if !strings.Contains(out, "r1") || !strings.Contains(out, "r2") {
		t.Errorf("the resumed ids are not named:\n%s", out)
	}
	if strings.Contains(out, "r3") {
		t.Errorf("a run without the usage-blocked failure code was resumed:\n%s", out)
	}
	if !strings.Contains(out, "2 usage-blocked run(s) processed") {
		t.Errorf("the summary is wrong:\n%s", out)
	}
}

// A resume failure on ONE run must not stop the batch: the sweep still
// reaches the runs after it, and the failure is named.
func TestRemoteRunsResumeUsageBlocked_OneFailureKeepsTheBatch(t *testing.T) {
	var resumes []string
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/runs" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"runs":[`+
				`{"id":"r1","workflow_name":"a","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"},`+
				`{"id":"r2","workflow_name":"b","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:31:00Z"}`+
				`]}`)
		case strings.HasPrefix(r.URL.Path, "/api/runs/") && strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPost:
			resumes = append(resumes, r.URL.Path)
			if strings.Contains(r.URL.Path, "r1") {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"error":"run moved under the operator"}`)
				return
			}
			fmt.Fprint(w, `{"status":"queued"}`)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	}))
	p, _ := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsResumeUsageBlocked(context.Background(), c, p, cli.RemoteRunsListOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(resumes) != 2 {
		t.Fatalf("resumes = %v, want both runs reached despite r1's failure", resumes)
	}
}

// Round-1 adversarial fixes: the sweep names the failure reason in human
// mode, fails the command when not one run resumed, stays a single JSON
// document (the inner resumes' own bodies must not leak into stdout), and
// reaches the whole population (no limit param sent).
func TestRemoteRunsResumeUsageBlocked_Round1Findings(t *testing.T) {
	var sawQuery string
	var resumes int
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/runs" && r.Method == http.MethodGet:
			sawQuery = r.URL.RawQuery
			fmt.Fprint(w, `{"runs":[{"id":"r1","workflow_name":"a","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPost:
			resumes++
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"run moved under the operator"}`)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	}))

	// H4: the failure REASON is named in human mode, and a sweep that
	// resumed nothing FAILS the command.
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsResumeUsageBlocked(context.Background(), c, p, cli.RemoteRunsListOptions{}); err == nil {
		t.Fatal("a sweep that resumed nothing must fail the command")
	}
	if !strings.Contains(buf.String(), "run moved under the operator") {
		t.Errorf("the failure reason is not named:\n%s", buf.String())
	}
	// H1: no limit param — the sweep reaches the whole population.
	if strings.Contains(sawQuery, "limit=") {
		t.Errorf("the sweep capped its own scan: %s", sawQuery)
	}

	// H3: JSON mode emits ONE document — the inner resume bodies stay out.
	p2, buf2 := remotePrinter(cli.OutputJSON)
	if err := cli.RemoteRunsResumeUsageBlocked(context.Background(), c, p2, cli.RemoteRunsListOptions{}); err == nil {
		t.Fatal("the JSON sweep must fail the command like the human one")
	}
	doc := buf2.String()
	if !json.Valid([]byte(doc)) {
		t.Errorf("JSON mode emitted a broken document:\n%s", doc)
	}
	if strings.Count(doc, `"processed"`) != 1 {
		t.Errorf("the envelope is not the single document:\n%s", doc)
	}
}

// H5: the failure-code filter holds in JSON mode too — a script asking for
// USAGE_LIMIT_BLOCKED must not be handed DLQ_PARKED rows the operator's
// table would have hidden.
func TestRemoteRunsList_FailureCodeFilterHoldsInJSON(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"runs":[`+
			`{"id":"blocked","workflow_name":"a","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"},`+
			`{"id":"parked","workflow_name":"b","status":"failed_resumable","failure_code":"DLQ_PARKED","created_at":"2026-10-06T11:31:00Z"}`+
			`]}`)
	}))
	p, buf := remotePrinter(cli.OutputJSON)
	if err := cli.RemoteRunsList(context.Background(), c, p, cli.RemoteRunsListOptions{FailureCode: "USAGE_LIMIT_BLOCKED"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	if !json.Valid([]byte(out)) {
		t.Fatalf("JSON mode emitted a broken document:\n%s", out)
	}
	if strings.Contains(out, "DLQ_PARKED") || strings.Contains(out, "parked") {
		t.Errorf("the JSON filter is fail-open:\n%s", out)
	}
	if !strings.Contains(out, "blocked") {
		t.Errorf("the filtered run is missing:\n%s", out)
	}
}
