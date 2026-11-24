package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// parityScenario is one request on a run seeded healthy: its route, body
// and the run's shape.
type parityScenario struct {
	name, method, sub, body string
	status                  store.RunStatus
	workdir, queued, child  bool
}

func parityScenarios() []parityScenario {
	fin, paused, running, fr := store.RunStatusFinished, store.RunStatusPausedWaitingHuman, store.RunStatusRunning, store.RunStatusFailedResumable
	return []parityScenario{
		{name: "rename", method: "POST", sub: "/rename", body: `{"name":"renamed"}`, status: fin},
		{name: "tags", method: "PUT", sub: "/tags", body: `{"tags":["a","b"]}`, status: fin},
		{name: "notes", method: "POST", sub: "/notes", body: `{"body":"hello"}`, status: fin},
		{name: "watch-add", method: "POST", sub: "/watch/ISS-1", body: `{}`, status: paused},
		{name: "watch-del", method: "DELETE", sub: "/watch/ISS-1", status: paused},
		{name: "queue", method: "POST", sub: "/queue-message", body: `{"text":"hi"}`, status: paused},
		{name: "queue-terminal", method: "POST", sub: "/queue-message", body: `{"text":"hi"}`, status: fin},
		{name: "cancel-queued", method: "DELETE", sub: "/queue-message/m1", status: paused, queued: true},
		{name: "cancel-queued-missing", method: "DELETE", sub: "/queue-message/nope", status: paused},
		{name: "cancel-paused", method: "POST", sub: "/cancel", body: `{}`, status: paused},
		{name: "cancel-failed-resumable", method: "POST", sub: "/cancel", body: `{}`, status: fr},
		{name: "cancel-finished", method: "POST", sub: "/cancel", body: `{}`, status: fin},
		{name: "cancel-running", method: "POST", sub: "/cancel", body: `{}`, status: running},
		{name: "pause", method: "POST", sub: "/pause", body: `{}`, status: running},
		{name: "bump-loop", method: "POST", sub: "/bump-loop", body: `{"loop_name":"l","delta":1}`, status: running},
		{name: "raise-budget", method: "POST", sub: "/raise-budget", body: `{"budget":{"max_cost_usd":1}}`, status: running},
		{name: "merge", method: "POST", sub: "/merge", body: `{}`, status: fin},
		{name: "commit-finalize", method: "POST", sub: "/commit-and-finalize", body: `{}`, status: fin},
		{name: "conflict-resolve", method: "POST", sub: "/merge/conflicts/resolve", body: `{"path":"a.txt","content":"x"}`, status: fin},
		{name: "conflict-agent", method: "POST", sub: "/merge/conflicts/resolve-with-agent", body: `{}`, status: fin},
		{name: "conflict-finalize", method: "POST", sub: "/merge/conflicts/finalize", body: `{}`, status: fin},
		{name: "conflict-abort", method: "POST", sub: "/merge/conflicts/abort", body: `{}`, status: fin},
		{name: "files-put", method: "PUT", sub: "/files/content", body: `{"path":"a.txt","content":"x"}`, status: fin, workdir: true},
		{name: "browser-attach", method: "POST", sub: "/browser/attach", body: `{}`, status: running},
		{name: "delete-finished", method: "DELETE", sub: "", status: fin},
		{name: "delete-running", method: "DELETE", sub: "", status: running},
		{name: "delete-with-a-child", method: "DELETE", sub: "", status: fin, child: true},
		{name: "answer-human", method: "POST", sub: "/answer-human", body: `{"answers":{"a":"b"}}`, status: paused},
		{name: "interaction-answer", method: "POST", sub: "/interactions/i1/answer", body: `{"answer":"x"}`, status: paused},
		{name: "resume-failed-resumable", method: "POST", sub: "/resume", body: `{}`, status: fr},
		{name: "resume-finished", method: "POST", sub: "/resume", body: `{}`, status: fin},
		{name: "rewind", method: "POST", sub: "/rewind", body: `{}`, status: fin},
		{name: "fork", method: "POST", sub: "/fork", body: `{}`, status: fin},
		{name: "host-event", method: "POST", sub: "/host-event", body: `{"kind":"x"}`, status: paused},
		{name: "assistant-watch", method: "POST", sub: "/assistant-watches", body: `{"assistant_run_id":"other-run"}`, status: running, child: true},
		{name: "assistant-watch-stop", method: "DELETE", sub: "/assistant-watches/w1", status: fin},
		{name: "assistant-mission", method: "POST", sub: "/assistant-missions", body: `{}`, status: fin},
		{name: "assistant-mission-stop", method: "POST", sub: "/assistant-missions/m1/stop", body: `{}`, status: fin},
		{name: "get-run", method: "GET", sub: "", status: fin},
		{name: "get-events", method: "GET", sub: "/events", status: fin},
		{name: "get-log", method: "GET", sub: "/log", status: fin},
		{name: "get-conflicts", method: "GET", sub: "/merge/conflicts", status: fin},
		{name: "get-files", method: "GET", sub: "/files", status: fin},
		{name: "get-children", method: "GET", sub: "/children", status: fin, child: true},
		{name: "get-diagnostic", method: "GET", sub: "/diagnostic", status: fin},
	}
}

// parityModes are the ways a request can reach a healthy run: no
// expectation; any expectation — the run's birth or a stale one — under
// ITERION_RUN_BIRTH_CHECK=0; the run's birth; `historical` on a run with no
// birth, in the header or the query.
var parityModes = []string{"none", "off-current", "off-stale", "on-current", "on-historical", "on-historical-query"}

var (
	parityBirthRE = regexp.MustCompile(`[A-Z2-7]{26}`)
	parityTimeRE  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+(Z|[+-]\d{2}:\d{2})`)
	parityUUIDRE  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	parityHexRE   = regexp.MustCompile(`\b[0-9a-f]{12,40}\b`)
	parityIDRE    = regexp.MustCompile(`"(id|message_id)":"[^"]*"`)
)

// parityNormal strips what differs between two fixtures by construction —
// their directories, births, instants and generated identifiers — so the
// answers of two modes compare whole. A run's empty birth (the historical
// modes) reads as a birth.
func parityNormal(s string, dirs ...string) string {
	for _, d := range dirs {
		s = strings.ReplaceAll(s, d, "<DIR>")
	}
	s = parityBirthRE.ReplaceAllString(s, "<BIRTH>")
	s = strings.ReplaceAll(s, `"birth_id":""`, `"birth_id":"<BIRTH>"`)
	s = parityTimeRE.ReplaceAllString(s, "<T>")
	s = parityUUIDRE.ReplaceAllString(s, "<UUID>")
	s = parityHexRE.ReplaceAllString(s, "<HEX>")
	s = parityIDRE.ReplaceAllString(s, `"$1":"<ID>"`)
	return strings.ReplaceAll(s, "\n", " ")
}

// paritySeed creates the scenario's run under id, healthy: its status, a
// workspace file, a queued message, a child run, as the scenario names.
// It returns the directories the fixture made for it.
func paritySeed(t *testing.T, fx *gateFixture, sc parityScenario, id string) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := fx.native.CreateRun(ctx, id, "wf", map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	mustWS(t, fx.native.UpdateRunStatus(ctx, id, sc.status, ""))
	dirs := []string{fx.srv.cfg.WorkDir, fx.native.Root()}
	if sc.workdir {
		r, err := fx.native.LoadRun(ctx, id)
		mustWS(t, err)
		wd := filepath.Join(t.TempDir(), "wd")
		mustWS(t, os.MkdirAll(wd, 0o755))
		mustWS(t, os.WriteFile(filepath.Join(wd, "a.txt"), []byte("old"), 0o644))
		r.WorkDir = wd
		mustWS(t, fx.native.SaveRun(ctx, r))
		dirs = append(dirs, wd)
	}
	if sc.queued {
		mustWS(t, fx.native.AppendQueuedMessage(ctx, id, store.QueuedUserMessage{ID: "m1", Text: "x"}))
	}
	if sc.child {
		if _, err := fx.native.CreateRun(ctx, "other-run", "wf", nil); err != nil {
			t.Fatal(err)
		}
		c, err := fx.native.LoadRun(ctx, "other-run")
		mustWS(t, err)
		c.ParentRunID = id
		mustWS(t, fx.native.SaveRun(ctx, c))
		mustWS(t, fx.native.UpdateRunStatus(ctx, "other-run", store.RunStatusFinished, ""))
	}
	return dirs
}

// parityState is what the request left of the run: its lifecycle fields and
// the entries of its directory.
func parityState(fx *gateFixture, id string, dirs []string) string {
	r, err := fx.native.LoadRun(context.Background(), id)
	if err != nil {
		return "load-error " + parityNormal(err.Error(), dirs...)
	}
	entries, _ := os.ReadDir(filepath.Join(fx.native.Root(), "runs", id))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return fmt.Sprintf("status=%s name=%q merge=%s watched=%v err=%q files=%v", r.Status, r.Name, r.MergeStatus, r.WatchedIssueIDs, parityNormal(r.Error, dirs...), names)
}

// parityDo sends the scenario's request in mode and returns its answer and
// the run's state after it, normalized.
func parityDo(t *testing.T, sc parityScenario, mode string) string {
	t.Helper()
	switch mode {
	case "off-current", "off-stale":
		t.Setenv(runBirthCheckEnv, "0")
	default:
		t.Setenv(runBirthCheckEnv, "")
	}
	fx := newGateFixture(t, true)
	const id = "parity-run"
	dirs := paritySeed(t, fx, sc, id)
	birth := ""
	switch mode {
	case "off-current", "on-current":
		birth = birthOf(t, fx.native, id)
	case "off-stale":
		birth = "STALEBIRTH0"
	case "on-historical", "on-historical-query":
		clearBirth(t, fx.native.Root(), id)
		birth = expectedBirthHistorical
	}
	target := fx.hs.URL + "/api/runs/" + id + sc.sub
	if mode == "on-historical-query" {
		target += "?" + expectedBirthParam + "=" + birth
	}
	var body io.Reader
	if sc.body != "" {
		body = bytes.NewBufferString(sc.body)
	}
	req, err := http.NewRequest(sc.method, target, body)
	mustWS(t, err)
	if sc.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if birth != "" && mode != "on-historical-query" {
		req.Header.Set(expectedBirthHeader, birth)
	}
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var env struct {
		Code string `json:"error_code"`
	}
	_ = json.Unmarshal(raw, &env)
	return fmt.Sprintf("%d|%s|%s|%s", resp.StatusCode, env.Code, parityNormal(string(raw), dirs...), parityState(fx, id, dirs))
}

// W4p: on a healthy run, every run route answers alike — the same status,
// error code, body and run state — whether its request carries no
// expectation, the run's birth, `historical` on a run with no birth, or any
// expectation under ITERION_RUN_BIRTH_CHECK=0. An expectation that holds
// changes nothing of what the action does: no healthy run is answered lost,
// no checked path diverges from the legacy one.
func TestRunRoutesServeAHealthyRunAlikeInEveryExpectationMode(t *testing.T) {
	for _, sc := range parityScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, mode := range parityModes {
				t.Run(mode, func(t *testing.T) { got[mode] = parityDo(t, sc, mode) })
			}
			for _, mode := range parityModes[1:] {
				if got[mode] != got[parityModes[0]] {
					t.Fatalf("%s in mode %s answered\n  %s\nwithout an expectation\n  %s", sc.name, mode, got[mode], got[parityModes[0]])
				}
			}
		})
	}
}
