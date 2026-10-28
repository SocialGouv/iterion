// The local resume lanes (in-process, detached subprocess) judge the
// mono_family pin their run FROZE at launch against THIS host's credentials
// — the same reading their launch takes, at the one other moment that can
// still refuse (#1221). The cloud lane judges its own inside SubmitResume,
// against the bundle it re-seals, which is why the judgement sits BELOW the
// publisher branch here: a host detection report describes this server's
// environment, never a queued run's sealed credentials.
//
// Mutants: the RefuseFrozenPin call removed → the refusal half reds; the
// call moved ABOVE the publisher branch → the cloud-lane half reds (a
// queued run would be judged against the wrong credential set); the
// judgement made unconditional → the served half reds.

package runview

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// monoFamilyBotSource declares the opt-in var the topology reads, so the
// judgement applies to it at all.
const monoFamilyBotSource = `
vars:
  review_mode: string = "mono"
  mono_family: string = "claude"

workflow operator_resume:
  entry: done
`

// isolateHostFamilies pins what detect.Detect can see to exactly the
// provider keys named: a scratch HOME and empty credential homes so no
// forfait, codex login or OAuth bridge is found, and every family-bearing
// variable cleared before the wanted ones are set. Without it this witness
// would read the developer's own keys and be green or red by accident.
// Values are literals written here; no host value is ever read or printed.
func isolateHostFamilies(t *testing.T, available ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ITERION_BACKEND_PREFERENCE", "")
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
		"ZAI_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL",
	} {
		t.Setenv(name, "")
	}
	for _, name := range available {
		t.Setenv(name, "sk-witness-not-a-real-key")
	}
}

// seedFrozenPinRun parks a run whose inputs carry the family the launch
// resolved, the way a launch leaves them on the document.
func seedFrozenPinRun(t *testing.T, svc *Service, runID, workflowHash, family string) map[string]any {
	t.Helper()
	inputs := map[string]any{"review_mode": "mono", "mono_family": family}
	if _, err := svc.store.CreateRun(context.Background(), runID, "operator_resume", inputs); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkflowHash = workflowHash
	if err := svc.store.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := svc.store.SaveCheckpoint(context.Background(), runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	return maps.Clone(r.Inputs)
}

func frozenPinService(t *testing.T, opts ...ServiceOption) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte(monoFamilyBotSource), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	svc, err := NewService(dir, append([]ServiceOption{WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir())}, opts...)...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, hash, err := CompileWorkflowWithHash(botPath, "")
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	return svc, botPath, hash
}

// A local resume whose frozen family this host no longer serves is refused,
// the run does not move, and its inputs stay byte-identical: the judgement
// reads them, it never rewrites them.
func TestResume_LocalLaneRefusesAPinThisHostCannotServe(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	isolateHostFamilies(t, "ANTHROPIC_API_KEY")
	svc, botPath, hash := frozenPinService(t)
	const runID = "run-local-dead-pin"
	frozen := seedFrozenPinRun(t, svc, runID, hash, "gpt")

	ctx := context.Background()
	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	if err == nil {
		t.Fatal("a local resume pinned to a family this host cannot serve started — it dies at its first review node instead")
	}
	if res != nil {
		t.Error("the refused resume reported a started run")
	}
	for _, want := range []string{"refusing to resume", `"gpt"`, "claude", "launch a fresh run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	r, lerr := svc.store.LoadRun(ctx, runID)
	if lerr != nil {
		t.Fatalf("LoadRun after the refusal: %v", lerr)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Errorf("the refused resume moved the run to %s, want paused_operator", r.Status)
	}
	if !reflect.DeepEqual(r.Inputs, frozen) {
		t.Errorf("the refused resume rewrote the frozen inputs: %+v, want %+v", r.Inputs, frozen)
	}
}

// The same lane resumes normally when a credential still serves the frozen
// family: this refuses a dead configuration, not resumes.
func TestResume_LocalLaneAcceptsAPinThisHostServes(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	isolateHostFamilies(t, "ANTHROPIC_API_KEY")
	svc, botPath, hash := frozenPinService(t)
	const runID = "run-local-served-pin"
	seedFrozenPinRun(t, svc, runID, hash, "claude")

	ctx := context.Background()
	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	if err != nil || res == nil {
		t.Fatalf("a local resume whose frozen family this host serves: %v, want it started", err)
	}
	select {
	case <-res.Done:
	case <-time.After(20 * time.Second):
		t.Fatal("the accepted resume never ended")
	}
}

// A QUEUED run is not judged here. Its credentials are the ones its bundle
// sealed, and this process's detection report describes the server's
// environment instead — so the cloud lane judges its own inside
// SubmitResume, and a dead pin reaches it rather than being refused against
// the wrong credential set. This is what pins the judgement BELOW the
// publisher branch: moved above it, a cloud resume on a host with no
// credential at all would be refused for a family its bundle may well
// serve.
func TestResume_TheCloudLaneIsNotJudgedAgainstThisHost(t *testing.T) {
	isolateHostFamilies(t, "ANTHROPIC_API_KEY")
	publisher := &operatorResumePublisher{}
	svc, botPath, hash := frozenPinService(t, WithLaunchPublisher(publisher))
	const runID = "run-queued-dead-pin"
	seedFrozenPinRun(t, svc, runID, hash, "gpt")

	if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath}); err != nil {
		t.Fatalf("a queued resume judged against this host's credentials: %v — the bundle's set is the run's, and SubmitResume reads it", err)
	}
	if publisher.resumeCalls != 1 {
		t.Fatalf("SubmitResume calls = %d, want 1 — the cloud lane must receive the resume and judge it there", publisher.resumeCalls)
	}
}
