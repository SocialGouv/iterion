package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scratchRefusalBody is the server's answer to a resume that would lose the
// scratch over an edited source: a message longer than the old 300-byte cut,
// and the hint the server writes after it.
func scratchRefusalBody(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"error":            "resume: [SCRATCH_NOT_PORTABLE] run 01KAZ9Q2V3W4X5Y6Z7A8B9C0DE: its teardown could not bank the files it left under ${PROJECT_SCRATCH_DIR} in a sandbox that is gone (the scratch compresses past the 256 MiB cap): resumed, its next nodes would find the scratch empty; the workflow source has also changed since the run started",
		"error_code":       "scratch_not_portable",
		"hint":             "relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss) to continue as it stands; the source changed too: add --force to accept that",
		"also_needs_force": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAPIError_saysWhatTheServerSaid: a JSON error body is read field by
// field — the whole message, its code, the hint the server writes after it,
// and the facts an operator acts on — and any other body keeps its first
// line.
func TestAPIError_saysWhatTheServerSaid(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	for _, tc := range []struct {
		name string
		body string
		want []string
		not  []string
	}{
		{"a scratch refusal", scratchRefusalBody(t), []string{
			"HTTP 400 POST /api/runs/r1/resume: resume: [SCRATCH_NOT_PORTABLE]",
			"the workflow source has also changed since the run started",
			"(error_code: scratch_not_portable; also needs --force)",
			"— hint: relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss)",
		}, []string{`"hint"`, "…"}},
		{"a queue outage", `{"error":"resume: the queue is unavailable","error_code":"queue_unavailable","retryable":true}`,
			[]string{"resume: the queue is unavailable (error_code: queue_unavailable; retryable)"}, nil},
		{"a launch denial", `{"error":"monthly_run_quota","detail":"monthly run quota (5) exhausted","reset_at":"2026-10-01T00:00:00Z"}`,
			[]string{"monthly_run_quota — monthly run quota (5) exhausted (resets at 2026-10-01T00:00:00Z)"}, nil},
		{"a refusal with its words in message", `{"error":"open_blockers","message":"cannot mark ready: open blockers #1","open_blockers":["1"]}`,
			[]string{"open_blockers — cannot mark ready: open blockers #1"}, nil},
		{"a huge message keeps its hint", fmt.Sprintf(`{"error":%q,"hint":"do this"}`, huge),
			[]string{"xxx…", "— hint: do this"}, []string{huge}},
		{"plain text", "forbidden: not a member\nsecond line", []string{"HTTP 400 POST /api/runs/r1/resume: forbidden: not a member"}, []string{"second line"}},
		{"JSON naming no message", `{"status":"nope"}`, []string{`: {"status":"nope"}`}, nil},
		{"an empty body", "", []string{": (empty body)"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&cli.APIError{Status: 400, Method: "POST", Path: "/api/runs/r1/resume", Body: tc.body}).Error()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("Error() = %q\nwant it to contain %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("Error() = %q\nwant it without %q", got, n)
				}
			}
		})
	}
}

// TestRemoteRunsResume_namesTheConsentAScratchRefusalNeeds: `iterion remote
// runs resume`, against the real server handler, prints the scratch's
// refusal with the consent that clears it, and that --force is needed too
// when the source changed.
func TestRemoteRunsResume_namesTheConsentAScratchRefusalNeeds(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	for _, tc := range []struct {
		name      string
		edited    bool
		needForce bool
		evs       []store.Event
	}{
		{"a scratch its teardown could not bank", false, false, []store.Event{
			{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap"}},
		}},
		{"a stale bank under an edited source", true, true, []store.Event{
			{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "bytes": 42}},
			{Type: store.EventRunResumed},
			{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": true, "bytes": 42}},
			{Type: store.EventNodeFinished, NodeID: "work", Data: map[string]any{"_in_sandbox": true, "_on_cycle": false}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			storeDir := filepath.Join(workDir, ".iterion")
			srv := server.New(server.Config{WorkDir: workDir, StoreDir: storeDir, SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
			hs := httptest.NewServer(srv.Handler())
			t.Cleanup(func() {
				hs.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			})
			source := "\nworkflow scratch_cli:\n  entry: done\n"
			botPath := filepath.Join(workDir, "scratch_cli.bot")
			if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			_, hash, err := runview.CompileWorkflowFromSource(botPath, source)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const runID = "01KAZ9Q2V3W4X5Y6Z7A8B9C0DE"
			if _, err := st.CreateRun(ctx, runID, "scratch_cli", nil); err != nil {
				t.Fatal(err)
			}
			run, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			run.Status = store.RunStatusFailedResumable
			run.FilePath = botPath
			run.WorkflowHash = hash
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			for _, ev := range tc.evs {
				if _, err := st.AppendEvent(ctx, runID, ev); err != nil {
					t.Fatal(err)
				}
			}
			if tc.edited {
				if err := os.WriteFile(botPath, []byte("\n## edited\n"+source), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: hs.URL})
			err = cli.RemoteRunsResume(ctx, c, &cli.Printer{W: io.Discard}, runID, cli.RemoteRunsResumeOptions{})
			var apiErr *cli.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("want the server's 400 refusal, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "--accept-scratch-loss") || !strings.Contains(msg, "error_code: scratch_not_portable") {
				t.Errorf("the refusal does not name its consent: %s", msg)
			}
			if got := strings.Contains(msg, "also needs --force"); got != tc.needForce {
				t.Errorf("says --force is needed too = %v, want %v: %s", got, tc.needForce, msg)
			}
		})
	}
}

// TestRemoteRunsEvents_showsAFailuresWordsAndRemedy: following a run prints
// what a failure says and what to do about it — run_failed and
// run_retry_skipped carry them as error and hint, not as message.
func TestRemoteRunsEvents_showsAFailuresWordsAndRemedy(t *testing.T) {
	hint := "the resume was refused before it claimed the run, which is back to failed_resumable; relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss) to continue as it stands; the source changed too: add --force to accept that"
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"seq": 1, "timestamp": time.Now().UTC(), "type": "run_retry_skipped", "data": map[string]any{
				"error": "[SCRATCH_NOT_PORTABLE] run r1: its teardown could not bank the scratch",
				"hint":  hint,
			}},
			{"seq": 2, "timestamp": time.Now().UTC(), "type": "node_started", "node_id": "n", "data": map[string]any{"message": "starting"}},
		}})
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteRunsEvents(context.Background(), c, p, "r1", 0, false, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"its teardown could not bank the scratch", "— hint: " + hint, "— starting"} {
		if !strings.Contains(out, want) {
			t.Errorf("events output:\n%s\nwant it to contain %q", out, want)
		}
	}
}

// TestRemoteErrorPaths_readTheBodyAsAPIErrorDoes: the remote commands that
// read an error response themselves say what the server said, hint
// included, as APIError does.
func TestRemoteErrorPaths_readTheBodyAsAPIErrorDoes(t *testing.T) {
	body := fmt.Sprintf(`{"error":%q,"hint":"ask an org admin"}`, strings.Repeat("refused ", 50))
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, body)
	})
	t.Run("login", func(t *testing.T) {
		c := remoteTestClient(t, refuse)
		_, err := c.LoginWithPassword(context.Background(), "a@b.c", "pw", "cli")
		if err == nil || !strings.Contains(err.Error(), "— hint: ask an org admin") {
			t.Fatalf("login refusal = %v, want the server's hint", err)
		}
	})
	t.Run("pool probe", func(t *testing.T) {
		c := remoteTestClient(t, refuse)
		p, _ := remotePrinter(cli.OutputHuman)
		err := cli.RemotePoolPolicy(context.Background(), c, p, "team-1", cli.PoolPolicy{})
		var apiErr *cli.APIError
		if !errors.As(err, &apiErr) || !strings.Contains(err.Error(), "— hint: ask an org admin") {
			t.Fatalf("pool probe refusal = %v, want an APIError with the server's hint", err)
		}
	})
}
