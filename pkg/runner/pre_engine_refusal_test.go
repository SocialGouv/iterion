package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/botsource"
	dslast "github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func TestEveryPreEngineRefusalRecordsItsVerdict(t *testing.T) {
	for _, name := range []string{"workflow_ir", "workflow", "budget", "credentials", "repository_pin", "workspace", "mcp", "executor", "bundle", "engine_requirement", "usage_cap", "launch_inputs", "engine_success", "engine_pause"} {
		t.Run(name, func(t *testing.T) {
			ctx := store.WithIdentity(context.Background(), "refusal-team", "refusal-owner")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "refusal"
			if _, err = st.CreateRun(ctx, id, "main", nil); err != nil {
				t.Fatal(err)
			}
			if err = st.UpdateRunStatus(ctx, id, store.RunStatusQueued, ""); err != nil {
				t.Fatal(err)
			}
			work := synctestWorkDir(t)
			r := &Runner{cfg: Config{Store: st, WorkDir: work, Logger: iterlog.Nop()}}
			src := "workflow main:\n  entry: done\n"
			if name == "workspace" {
				src = "secrets:\n  forge_token:\n    sinks: [prompt, egress]\n\n" + src
			}
			if name == "usage_cap" {
				src = "workflow main:\n  entry: act\n  act -> done\nagent act:\n  backend: \"claude_code\"\n  model: \"claude-sonnet-4-5\"\n  system: sys\nprompt sys:\n  no paid execution\n"
			}
			parsed := parsertest.Parse("main.bot", src)
			body, err := dslast.MarshalFile(parsed.File)
			if err != nil {
				t.Fatal(err)
			}
			msg := &queue.RunMessage{RunID: id, TenantID: "refusal-team", OwnerID: "refusal-owner", WorkflowName: "main", IRCompiled: body, PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano)}
			want := ""
			code := store.FailureExecutionFailed
			status := store.RunStatusQueued
			switch name {
			case "workflow_ir":
				msg.IRCompiled = []byte("invalid json")
				want = "decode IR"
				code = store.FailureIRUnloadable
				status = store.RunStatusFailedResumable
			case "workflow":
				msg.IRCompiled = nil
				want = "neither IRCompiled nor IRRef"
			case "budget":
				msg.Budget = &queue.BudgetOverrides{MaxDuration: "invalid-duration"}
				want = "budget override"
			case "credentials":
				msg.SecretsRef = "missing-ref"
				want = "RunSecrets not wired"
			case "repository_pin":
				msg.RepoSHAExpected = "0123456789abcdef"
				want = "no repository to clone"
			case "mcp":
				if e := os.WriteFile(filepath.Join(work, ".mcp.json"), []byte("invalid json"), 0600); e != nil {
					t.Fatal(e)
				}
				want = "resolve MCP servers"
			case "workspace":
				msg.RepoURL = "https://example.invalid/no-clone.git"
				ctx = secrets.WithCredentials(ctx, secrets.Credentials{Generic: map[string]string{"forge_token": "synthetic-token"}})
				want = "withhold"
				code = store.FailureCapabilityUnsupported
				status = store.RunStatusFailedResumable
			case "executor":
				msg.Vars = map[string]any{"unserializable": func() {}}
				want = "unsupported type"
			case "bundle":
				msg.BotBundle = &queue.BotBundleRef{TenantID: "refusal-team", Slug: "missing", Version: 1}
				want = "no bot-source store wired"
			case "engine_requirement":
				pinEngineBuild(t, "v3.112.7")
				mem := botsource.NewMemoryStore()
				r.cfg.BotSources = mem
				created, e := mem.Create(store.WithTenant(ctx, botsource.PlatformTenantID), botsource.BotSource{TenantID: botsource.PlatformTenantID, Slug: "needy", Files: map[string]string{botsource.MainBotFile: "workflow main:\n  entry: done\n", "manifest.yaml": "name: needy\nversion: 1.6.0\nrequires:\n  iterion: \">= 3.112.14\"\n"}})
				if e != nil {
					t.Fatal(e)
				}
				msg.BotID = "needy"
				msg.BotBundle = &queue.BotBundleRef{TenantID: botsource.PlatformTenantID, Slug: "needy", Version: created.Version}
				want = "requires"
				code = store.FailureBotRequiresNewerEngine
				status = store.RunStatusFailed
			case "usage_cap":
				caps := usagecap.NewMemStore()
				r.cfg.UsageCaps = caps
				r.cfg.UsageCapSource = usagecap.StaticPolicy(capTestPolicy())
				if e := caps.Record(ctx, usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, ""), usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.92, Status: usagecap.StatusWarning, ResetsAt: time.Now().UTC().Add(time.Hour), ObservedAt: time.Now().UTC()}); e != nil {
					t.Fatal(e)
				}
				want = "not started"
				code = store.FailureUsageLimitBlocked
				status = store.RunStatusFailedResumable
			case "launch_inputs":
				msg.Vars = map[string]any{"unknown": "x"}
				want = "names no var"
			case "engine_pause":
				msg.IRCompiled = parkingRunMessage(t, id).IRCompiled
			}
			cause := r.executeRun(ctx, msg, nil, nil)
			doc, e := st.LoadRun(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			events, e := st.LoadEvents(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			var verdicts []store.Event
			for _, event := range events {
				if event.Type == store.EventRunFailed || event.Type == store.EventRunPreEngineRefused {
					verdicts = append(verdicts, *event)
				}
			}
			if name == "engine_success" || name == "engine_pause" {
				if name == "engine_success" && cause != nil {
					t.Fatal(cause)
				}
				if name == "engine_pause" && !errors.Is(cause, runtime.ErrRunPaused) {
					t.Fatalf("engine pause=%v", cause)
				}
				if doc.PreEngineRefusal != nil || len(verdicts) != 0 {
					t.Fatal("engine return was recorded as pre-engine refusal")
				}
				return
			}
			if cause == nil || !strings.Contains(cause.Error(), want) {
				t.Fatalf("wrong pipeline branch: %v want %q", cause, want)
			}
			if doc.Status != status || doc.PreEngineRefusal == nil || doc.PreEngineRefusal.Phase != name || doc.PreEngineRefusal.Code != code || doc.Error == "" || len(verdicts) != 1 {
				t.Fatalf("MISSING_VERDICT %s: status=%s diagnostic=%+v events=%v", name, doc.Status, doc.PreEngineRefusal, verdicts)
			}
			eventType := store.EventRunFailed
			if status.IsQueued() {
				eventType = store.EventRunPreEngineRefused
			}
			if verdicts[0].Type != eventType || verdicts[0].Data["code"] != string(code) || verdicts[0].Data["phase"] != name {
				t.Fatalf("false event: %+v", verdicts[0])
			}
			if status.IsQueued() && !isNakAction(classifyExecResult(cause, id).action) {
				t.Fatal("generic refusal changed delivery classification")
			}
			if !status.IsQueued() && doc.FailureCode != code {
				t.Fatalf("failure code=%s", doc.FailureCode)
			}
			if name == "workspace" && !strings.Contains(doc.Error, "grant") {
				t.Fatal("clone remedy missing from durable error")
			}
		})
	}
}

type preEngineTestStore struct {
	store.RunStore
	result         store.PreEngineRefusalResult
	recordErr      error
	eventErr       error
	writes, events int
}

func (s *preEngineTestStore) RecordPreEngineRefusal(ctx context.Context, _ string, q store.PreEngineRefusalRequest) (store.PreEngineRefusalResult, error) {
	s.writes++
	if err := ctx.Err(); err != nil {
		return store.PreEngineRefusalResult{}, err
	}
	return s.result, s.recordErr
}
func (s *preEngineTestStore) AppendEvent(context.Context, string, store.Event) (*store.Event, error) {
	s.events++
	return nil, s.eventErr
}

type preEngineLegacyStore struct{ store.RunStore }

func TestPreEngineRecorderKeepsCauseAndEmitsOnlyAfterWrite(t *testing.T) {
	cause := errors.New("original")
	for _, name := range []string{"CAS_refused", "store_error", "event_error", "malformed_publication", "cancelled_parent", "missing_capability"} {
		t.Run(name, func(t *testing.T) {
			st := &preEngineTestStore{result: store.PreEngineRefusalResult{Changed: true, Status: store.RunStatusQueued}}
			msg := &queue.RunMessage{RunID: "run"}
			ctx := context.Background()
			expected := 1
			switch name {
			case "CAS_refused":
				st.result.Changed = false
				expected = 0
			case "store_error":
				st.recordErr = errors.New("store broken")
				expected = 0
			case "event_error":
				st.eventErr = errors.New("event broken")
			case "malformed_publication":
				msg.PublishedAtRFC = "not a date"
				expected = 0
			case "cancelled_parent":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing_capability":
				expected = 0
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			if name == "missing_capability" {
				r.cfg.Store = preEngineLegacyStore{RunStore: st}
			}
			if got := r.refuseBeforeEngine(ctx, msg, "probe", cause); got != cause {
				t.Fatal("original error replaced")
			}
			if st.events != expected {
				t.Fatalf("FALSE_EVENT %s: events=%d want%d", name, st.events, expected)
			}
			if (name == "malformed_publication" || name == "missing_capability") && st.writes != 0 {
				t.Fatal("invalid/capability-less recorder wrote")
			}
		})
	}
}
