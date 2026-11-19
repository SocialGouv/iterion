package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The carrier-E witnesses (plan §1 D4): an action that continues an
// execution or reads other runs carries the admitted origin into its service
// (runview.ActionOrigin). The service verifies each run it loads by ID and
// acts on, binds only its named store-only writes, and the execution it
// starts keeps no binding.

const (
	expectResumeSource = "workflow resume_probe:\n  entry: done\n"
	expectHumanSource  = "schema ans:\n  ok: bool\n\nhuman ask:\n  output: ans\n\nworkflow answer_probe:\n  entry: ask\n  ask -> done\n"
	expectRewindSource = "agent draft:\n  model: \"claude-opus-4-7\"\n\nagent polish:\n  model: \"claude-opus-4-7\"\n\n" +
		"workflow rewind_probe:\n  entry: draft\n  draft -> polish\n  polish -> done\n"
	expectChatSource = "schema chat_out:\n  message: string\n  host_event: json\n\n" +
		"human chat:\n  output: chat_out\n  interaction: human_or_host\n\n" +
		"workflow host_probe:\n  entry: chat\n  chat -> done\n"
	expectChatManifest = "name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n      host_event_field: host_event\n"
	// expectAssistantID is the assistant run the watch and mission rows
	// link to the target; it is never replaced.
	expectAssistantID = "asst"
)

// expectBot is one workflow file of the fixture with its compiled hash.
type expectBot struct{ path, source, hash string }

// expectFixture is the production chain over the gate store (gateFixture)
// with the workflow files every row's run is seeded from, and the row's run
// born A under actionRunID.
type expectFixture struct {
	*gateFixture
	row   expectRow
	bots  map[string]expectBot
	birth string
}

// expectRow is one carrier-E row of plan §0.2: its request, how its run is
// seeded (A and its successor B alike, unless successor names B's shape),
// where the replacement lands — after the last load the row's own guard
// must pass — and HEAD's status for the run it expects.
type expectRow struct {
	n         int
	name      string
	path      string
	body      string
	seed      func(ctx context.Context, fx *expectFixture, marker string) error
	successor func(ctx context.Context, fx *expectFixture, marker string) error
	arm       func(fx *expectFixture, replace func() error)
	ok        int
}

// afterLoad replaces the run right after the nth run load from the request's
// first (the gate's read is the first).
func afterLoad(n int32) func(fx *expectFixture, replace func() error) {
	return func(fx *expectFixture, replace func() error) { fx.store.afterLoadN(n, replace) }
}

func expectRows() []expectRow {
	return []expectRow{
		{n: 20, name: "answer-human", path: "/answer-human", body: `{"answers":{"ok":true}}`, seed: seedHumanPaused, arm: afterLoad(2), ok: http.StatusAccepted},
		{n: 21, name: "answer-interaction", path: "/interactions/q1/answer", body: `{"answer":"blue"}`, seed: seedAsyncPending, arm: afterLoad(1), ok: http.StatusOK},
		{n: 22, name: "resume", path: "/resume", body: `{}`, seed: seedCancelledAtDone, arm: afterLoad(2), ok: http.StatusAccepted},
		{n: 22, name: "resume-running-orphan-successor", path: "/resume", body: `{}`, seed: seedCancelledAtDone, successor: seedRunningOrphan, arm: afterLoad(2), ok: http.StatusAccepted},
		{n: 23, name: "rewind", path: "/rewind", body: `{"node_id":"polish","keep_files":true}`, seed: seedRewindable, arm: afterLoad(2), ok: http.StatusOK},
		{n: 24, name: "fork", path: "/fork", body: `{"node_id":"review"}`, seed: seedForkable, arm: afterLoad(1), ok: http.StatusCreated},
		{n: 25, name: "host-event", path: "/host-event", body: `{"kind":"action-completed","event":{"done":true}}`, seed: seedChatPaused, arm: afterLoad(2), ok: http.StatusOK},
		{n: 26, name: "assistant-watch", path: "/assistant-watches", body: `{"assistant_run_id":"asst","mode":"diagnose"}`, seed: seedWatchTarget, arm: afterLoad(1), ok: http.StatusOK},
		{n: 28, name: "assistant-mission", path: "/assistant-missions", body: `{"assistant_run_id":"asst"}`, seed: seedWatchTarget, arm: afterLoad(1), ok: http.StatusOK},
	}
}

func expectRowByName(t *testing.T, name string) expectRow {
	t.Helper()
	for _, row := range expectRows() {
		if row.name == name {
			return row
		}
	}
	t.Fatalf("no expect row %q", name)
	return expectRow{}
}

// createExpectRun creates id named marker from bot, lets edit shape it, and
// persists it.
func (fx *expectFixture) createExpectRun(ctx context.Context, id, marker, bot string, edit func(*store.Run)) error {
	b := fx.bots[bot]
	if _, err := fx.native.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: marker, Format: store.LegacyValueFormat}); err != nil {
		return err
	}
	r, err := fx.native.LoadRun(ctx, id)
	if err != nil {
		return err
	}
	r.Name = marker
	r.FilePath, r.WorkflowSource, r.WorkflowHash = b.path, b.source, b.hash
	if edit != nil {
		edit(r)
	}
	return fx.native.SaveRun(ctx, r)
}

func emptyCheckpoint(node, interaction string, questions map[string]any) *store.Checkpoint {
	return &store.Checkpoint{
		NodeID: node, InteractionID: interaction, InteractionQuestions: questions,
		Outputs: map[string]map[string]any{}, LoopCounters: map[string]int{}, ArtifactVersions: map[string]int{}, Vars: map[string]any{},
	}
}

// seedCancelledAtDone: cancelled on a checkpoint, what an operator resume
// re-enters in-process.
func seedCancelledAtDone(ctx context.Context, fx *expectFixture, marker string) error {
	if err := fx.createExpectRun(ctx, actionRunID, marker, "resume", func(r *store.Run) { r.Status = store.RunStatusCancelled }); err != nil {
		return err
	}
	return fx.native.SaveCheckpoint(ctx, actionRunID, &store.Checkpoint{NodeID: "done"})
}

// seedRunningOrphan: running with no execution in this process — the
// successor a resume's targeted reconcile would move.
func seedRunningOrphan(ctx context.Context, fx *expectFixture, marker string) error {
	return fx.createExpectRun(ctx, actionRunID, marker, "resume", func(r *store.Run) { r.Status = store.RunStatusRunning })
}

// seedHumanPaused: paused on a human gate with its interaction.
func seedHumanPaused(ctx context.Context, fx *expectFixture, marker string) error {
	if err := fx.createExpectRun(ctx, actionRunID, marker, "human", nil); err != nil {
		return err
	}
	iid := actionRunID + "_ask"
	if err := fx.native.WriteInteraction(ctx, &store.Interaction{ID: iid, RunID: actionRunID, NodeID: "ask", RequestedAt: time.Now().UTC(), Questions: map[string]any{}}); err != nil {
		return err
	}
	return fx.native.PauseRun(ctx, actionRunID, emptyCheckpoint("ask", iid, map[string]any{}))
}

// seedAsyncPending: paused on the human gate with one pending async
// question, q1, the gate not awaiting it: answering records the answer and
// delivers it to the node's inbox, and resumes nothing.
//
// An await-paused run (the gate awaiting q1) is not the fixture: the
// in-process auto-resume an answer would drive hands Resume no answers, and
// validateResumable refuses a paused_waiting_human run without answers
// before anything else, so that resume never runs.
func seedAsyncPending(ctx context.Context, fx *expectFixture, marker string) error {
	if err := seedHumanPaused(ctx, fx, marker); err != nil {
		return err
	}
	return fx.native.WriteInteraction(ctx, &store.Interaction{ID: "q1", RunID: actionRunID, NodeID: "ask", Kind: store.InteractionKindAsync, RequestedAt: time.Now().UTC()})
}

// seedRewindable: failed after its second node.
func seedRewindable(ctx context.Context, fx *expectFixture, marker string) error {
	return fx.createExpectRun(ctx, actionRunID, marker, "rewind", func(r *store.Run) {
		r.Status = store.RunStatusFailedResumable
		r.Checkpoint = &store.Checkpoint{NodeID: "polish", Outputs: map[string]map[string]any{"draft": {"v": 1}, "polish": {"v": 1}}}
	})
}

// seedForkable: cancelled with a turn checkpoint at its agent node.
func seedForkable(ctx context.Context, fx *expectFixture, marker string) error {
	if err := fx.createExpectRun(ctx, actionRunID, marker, "resume", func(r *store.Run) {
		r.Status = store.RunStatusCancelled
		r.Checkpoint = &store.Checkpoint{NodeID: "review"}
	}); err != nil {
		return err
	}
	return fx.native.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: actionRunID, NodeID: "review", TurnIndex: 0, Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "review/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"` + marker + `"}]}]`),
		WrittenAt:   time.Now().UTC(),
	})
}

// seedChatRun pauses id on the chat bot's host-event-capable gate, owned by
// the identity the DisableAuth chain stamps ("dev"): the operator a watch
// and a mission are armed for.
func seedChatRun(ctx context.Context, fx *expectFixture, id, marker string) error {
	if err := fx.createExpectRun(ctx, id, marker, "chat", func(r *store.Run) { r.BotID, r.OwnerID = "chatbot", "dev" }); err != nil {
		return err
	}
	iid := id + "_chat"
	if err := fx.native.WriteInteraction(ctx, &store.Interaction{ID: iid, RunID: id, NodeID: "chat", RequestedAt: time.Now().UTC(), Questions: map[string]any{}}); err != nil {
		return err
	}
	return fx.native.PauseRun(ctx, id, emptyCheckpoint("chat", iid, map[string]any{}))
}

func seedChatPaused(ctx context.Context, fx *expectFixture, marker string) error {
	return seedChatRun(ctx, fx, actionRunID, marker)
}

// seedWatchTarget: a failed target; the assistant run is the fixture's.
func seedWatchTarget(ctx context.Context, fx *expectFixture, marker string) error {
	return fx.createExpectRun(ctx, actionRunID, marker, "resume", func(r *store.Run) { r.Status = store.RunStatusFailed })
}

// replaceExpectRun ends, deletes and prunes the run, then seeds its
// successor — the only way a new birth takes an existing ID.
func (fx *expectFixture) replaceExpectRun(ctx context.Context) error {
	r, err := fx.native.LoadRun(ctx, actionRunID)
	if err != nil {
		return err
	}
	if !r.Status.IsTerminal() {
		if err := fx.native.UpdateRunStatus(ctx, actionRunID, store.RunStatusFinished, ""); err != nil {
			return err
		}
	}
	if err := fx.native.DeleteRun(ctx, actionRunID); err != nil {
		return err
	}
	if _, err := fx.native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	successor := fx.row.successor
	if successor == nil {
		successor = fx.row.seed
	}
	return successor(ctx, fx, "B")
}

func newExpectFixture(t *testing.T, row expectRow) *expectFixture {
	t.Helper()
	noLLMCredential(t)
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	fx := &expectFixture{gateFixture: newGateFixture(t, true), row: row, bots: map[string]expectBot{}}
	work := fx.srv.cfg.WorkDir
	botsRoot := filepath.Join(work, "bots")
	chatDir := filepath.Join(botsRoot, "chatbot")
	mustWS(t, os.MkdirAll(chatDir, 0o755))
	mustWS(t, os.WriteFile(filepath.Join(chatDir, "manifest.yaml"), []byte(expectChatManifest), 0o600))
	fx.srv.cfg.Bots.Paths = []string{botsRoot}
	for name, b := range map[string]expectBot{
		"resume": {path: filepath.Join(work, "resume_probe.bot"), source: expectResumeSource},
		"human":  {path: filepath.Join(work, "answer_probe.bot"), source: expectHumanSource},
		"rewind": {path: filepath.Join(work, "rewind_probe.bot"), source: expectRewindSource},
		"chat":   {path: filepath.Join(chatDir, "main.bot"), source: expectChatSource},
	} {
		mustWS(t, os.WriteFile(b.path, []byte(b.source), 0o644))
		_, hash, err := runview.CompileWorkflowFromSource(b.path, b.source, "", "")
		mustWS(t, err)
		b.hash = hash
		fx.bots[name] = b
	}
	ctx := t.Context()
	if row.name == "assistant-watch" || row.name == "assistant-mission" {
		mustWS(t, seedChatRun(ctx, fx, expectAssistantID, "assistant"))
	}
	mustWS(t, row.seed(ctx, fx, "A"))
	fx.birth = birthOf(t, fx.native, actionRunID)
	if row.name == "assistant-mission" {
		// A mission binds the one active watch of its target: armed for A,
		// unchecked. It is keyed by the target's ID, so a successor recreated
		// under that ID finds it too.
		w := expectDo(t, fx.hs.URL, expectRowByName(t, "assistant-watch"), "")
		if w.status != http.StatusOK {
			t.Fatalf("fixture: arm the mission's watch: %d %s", w.status, w.body)
		}
	}
	return fx
}

func expectDo(t *testing.T, base string, row expectRow, birth string) gateReply {
	t.Helper()
	return actionDo(t, base, actionRow{method: http.MethodPost, path: row.path, body: row.body}, birth)
}

// entityPicture is what an action could create beside the addressed run:
// another run (a fork's child), a watch, a mission.
func (fx *expectFixture) entityPicture() (string, error) {
	root := fx.srv.cfg.StoreDir
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("run " + e.Name() + "\n")
	}
	for _, name := range []string{"assistant-run-watches.json", "assistant-missions.json"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		switch {
		case err == nil:
			sum := sha256.Sum256(raw)
			b.WriteString(name + " " + hex.EncodeToString(sum[:]) + "\n")
		case errors.Is(err, fs.ErrNotExist):
			b.WriteString(name + " absent\n")
		default:
			return "", err
		}
	}
	return b.String(), nil
}

// expectPicture is the successor's run directory and the entities beside
// it at the end of the replacement hook.
type expectPicture struct {
	mu       sync.Mutex
	birth    string
	run      map[string]string
	entities string
}

func (fx *expectFixture) armExpectReplacement(t *testing.T) *expectPicture {
	t.Helper()
	pic := &expectPicture{}
	runDir := filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID)
	fx.row.arm(fx, func() error {
		ctx := context.Background()
		if err := fx.replaceExpectRun(ctx); err != nil {
			return err
		}
		b, err := fx.native.LoadRun(ctx, actionRunID)
		if err != nil {
			return err
		}
		run, err := treeDigest(runDir)
		if err != nil {
			return err
		}
		ent, err := fx.entityPicture()
		if err != nil {
			return err
		}
		pic.mu.Lock()
		pic.birth, pic.run, pic.entities = b.BirthID, run, ent
		pic.mu.Unlock()
		fx.store.probe.reset()
		return nil
	})
	return pic
}

// settle waits until no execution of this service holds the run, so the
// test's directories are not removed under a resumed run.
func (fx *expectFixture) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for fx.srv.runs.Active(actionRunID) {
		if time.Now().After(deadline) {
			t.Fatal("the run's execution did not end within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// W7: every carrier-E row, admitted on A's birth, with A replaced by an
// actionable B after the last load the row's own guard must pass. The
// action is refused 409 run_origin_lost naming the run; B's run directory
// (status, checkpoint, events, interactions, inbox, log) is as the
// replacement left it, and nothing was created beside it — no fork child,
// no watch, no mission. The orphan row's B is running: it stays running,
// never reconciled.
func TestExpectedMutationsRefuseASuccessor(t *testing.T) {
	for _, row := range expectRows() {
		t.Run(fmt.Sprintf("%02d-%s", row.n, row.name), func(t *testing.T) {
			fx := newExpectFixture(t, row)
			pic := fx.armExpectReplacement(t)
			got := expectDo(t, fx.hs.URL, row, fx.birth)
			fx.settle(t)
			mustWS(t, fx.store.failure())
			pic.mu.Lock()
			defer pic.mu.Unlock()
			if pic.birth == "" || pic.birth == fx.birth {
				t.Fatalf("the replacement did not run: successor birth %q, original %q (reply %d %s)", pic.birth, fx.birth, got.status, got.body)
			}
			wantOriginLost(t, row.name, got, actionRunID)
			if d := diffDigests(pic.run, mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))); len(d) != 0 {
				t.Fatalf("the successor's run directory changed:\n%s", strings.Join(d, "\n"))
			}
			ent, err := fx.entityPicture()
			mustWS(t, err)
			if ent != pic.entities {
				t.Fatalf("the action created something beside the successor:\nbefore:\n%s\nafter:\n%s", pic.entities, ent)
			}
		})
	}
}

// W3 (E rows): at the run's current birth every row answers HEAD's status,
// and no store access is left bound behind the response: an execution an
// E row starts never inherits the binding.
func TestExpectedMutationsServeTheAdmittedRun(t *testing.T) {
	for _, row := range expectRows() {
		if row.successor != nil {
			continue
		}
		t.Run(fmt.Sprintf("%02d-%s", row.n, row.name), func(t *testing.T) {
			fx := newExpectFixture(t, row)
			hs := withAfterFlag(t, fx.gateFixture)
			fx.store.probe.reset()
			got := expectDo(t, hs.URL, row, fx.birth)
			fx.settle(t)
			if got.status != row.ok {
				t.Fatalf("%s at the current birth: %d %s, want %d", row.name, got.status, got.body, row.ok)
			}
			calls, _ := fx.store.probe.recorded()
			for _, c := range calls {
				if c.bound && c.after {
					t.Fatalf("%s left a bound access behind the response: %+v", row.name, c)
				}
			}
		})
	}
}

// W4 (E rows): an unchecked request — no expectation, or one under
// ITERION_RUN_BIRTH_CHECK=0 — calls its service as HEAD does: HEAD's status
// and not one bound store access, before or after the response.
func TestUncheckedExpectedMutationsStayUnbound(t *testing.T) {
	for _, off := range []bool{false, true} {
		for _, row := range expectRows() {
			if row.successor != nil {
				continue
			}
			t.Run(fmt.Sprintf("check-off=%t/%02d-%s", off, row.n, row.name), func(t *testing.T) {
				fx := newExpectFixture(t, row)
				birth := ""
				if off {
					t.Setenv(runBirthCheckEnv, "0")
					birth = fx.birth
				}
				fx.store.probe.reset()
				got := expectDo(t, fx.hs.URL, row, birth)
				fx.settle(t)
				if got.status != row.ok {
					t.Fatalf("%s unchecked: %d %s, want %d", row.name, got.status, got.body, row.ok)
				}
				calls, _ := fx.store.probe.recorded()
				for _, c := range calls {
					if c.bound {
						t.Fatalf("%s unchecked made a bound access: %+v", row.name, c)
					}
				}
			})
		}
	}
}

// W5 (E rows): a request the gate admitted, served by a store that does not
// endorse a bound observation, is refused 503 scope_unavailable before the
// service runs: the run is as seeded.
func TestExpectedActionRefusesAStoreThatDropsItsContext(t *testing.T) {
	for _, name := range []string{"answer-human", "answer-interaction", "resume", "rewind", "fork", "host-event"} {
		t.Run(name, func(t *testing.T) {
			row := expectRowByName(t, name)
			fx := newExpectFixture(t, row)
			fx.srv.runs.Stop(t.Context())
			fx.srv.runs = newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(nonEndorsingStore{fx.native}))
			runDir := filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID)
			before := mustTreeDigest(t, runDir)
			got := expectDo(t, fx.hs.URL, row, fx.birth)
			fx.settle(t)
			if got.status != http.StatusServiceUnavailable || got.code != errCodeScopeUnavailable {
				t.Fatalf("%s over a non-endorsing store: %d %s, want 503 %s", name, got.status, got.body, errCodeScopeUnavailable)
			}
			if d := diffDigests(before, mustTreeDigest(t, runDir)); len(d) != 0 {
				t.Fatalf("%s acted though refused:\n%s", name, strings.Join(d, "\n"))
			}
		})
	}
}

// W7u: a resume with an attachment, A replaced after the preflight's load
// and before the promotion. The promotion is bound to A: it refuses, the
// answer is 409 run_origin_lost, B's attachments are untouched and the
// staged upload is kept for the retry.
func TestResumeUploadPromotionBoundToTheAdmittedRun(t *testing.T) {
	row := expectRowByName(t, "answer-human")
	row.path, row.body = "/resume", `{"answers":{"ok":true},"attachments":["upload-1"]}`
	fx := newExpectFixture(t, row)
	fx.srv.cfg.MaxUploadsPerRun = 5
	ctx := t.Context()
	body := []byte("staged bytes\n")
	sum := sha256.Sum256(body)
	mustWS(t, fx.native.StageUpload(ctx, store.StagedUpload{
		UploadID: "upload-1", OriginalFilename: "note.txt", MIME: "text/plain",
		Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(),
	}, bytes.NewReader(body)))
	// The gate's load, the handler's, then the preflight's: the successor
	// lands before the promotion.
	row.arm = afterLoad(3)
	fx.row = row
	pic := fx.armExpectReplacement(t)
	got := expectDo(t, fx.hs.URL, row, fx.birth)
	fx.settle(t)
	mustWS(t, fx.store.failure())
	pic.mu.Lock()
	defer pic.mu.Unlock()
	if pic.birth == "" || pic.birth == fx.birth {
		t.Fatalf("the replacement did not run (reply %d %s)", got.status, got.body)
	}
	wantOriginLost(t, "resume with an upload", got, actionRunID)
	if d := diffDigests(pic.run, mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))); len(d) != 0 {
		t.Fatalf("the successor's run directory changed:\n%s", strings.Join(d, "\n"))
	}
	if _, rc, err := fx.native.OpenStagedUpload(ctx, "upload-1"); err != nil {
		t.Fatalf("the staged upload was consumed by a refused promotion: %v", err)
	} else {
		_ = rc.Close()
	}
}
