package cli_test

import (
	"context"
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
			fmt.Fprint(w, `{"runs":[` +
				`{"id":"r1","workflow_name":"review_pr","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"},` +
				`{"id":"r2","workflow_name":"review_pr","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:31:00Z"},` +
				`{"id":"r3","workflow_name":"other","status":"failed_resumable","failure_code":"","created_at":"2026-10-06T11:32:00Z"}` +
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
			fmt.Fprint(w, `{"runs":[` +
				`{"id":"r1","workflow_name":"a","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:30:00Z"},` +
				`{"id":"r2","workflow_name":"b","status":"failed_resumable","failure_code":"USAGE_LIMIT_BLOCKED","created_at":"2026-10-06T11:31:00Z"}` +
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
