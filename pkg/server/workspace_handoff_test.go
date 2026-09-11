package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestWorkspaceHandoffCompletionReturnsDurablyToExactSource(t *testing.T) {
	ctx := context.Background()
	registry := &projects.Config{
		Version: 1,
		RecentProjects: []projects.Project{
			{ID: "source", Name: "Source", Dir: t.TempDir(), StoreDir: t.TempDir()},
			{ID: "target", Name: "Target", Dir: t.TempDir(), StoreDir: t.TempDir()},
		},
		CurrentProjectID: "source",
	}
	newHost := func() *WorkspaceHost {
		host, err := NewWorkspaceHost(registry, func(project projects.Project) (*Server, error) {
			return New(Config{
				WorkDir: project.Dir, StoreDir: project.StoreDir,
				DisableAuth: true, SkipProjectRegistration: true,
				RecoveryPassive: true,
			}, iterlog.Nop()), nil
		}, iterlog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		return host
	}
	shutdown := func(host *WorkspaceHost) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.Shutdown(shutdownCtx); err != nil {
			t.Fatal(err)
		}
	}
	host := newHost()
	t.Cleanup(func() {
		if host != nil {
			shutdown(host)
		}
	})

	seedChatRun := func(projectID, runID, clientID, conversationID string) {
		runStore := host.runtimes[projectID].server.runs.RunStore()
		run, err := runStore.CreateRun(ctx, runID, "copilot", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.Source = &store.RunSource{
			Kind: store.RunSourceKindStudioChat, ClientID: clientID,
			ConversationID: conversationID,
		}
		if err := runStore.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	seedChatRun("source", "source-run", "source-client", "source-conversation")
	seedChatRun("target", "target-run", "target-client", "target-conversation")
	seedChatRun("target", "other-run", "other-client", "other-conversation")

	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Origin", "http://iterion.test")
		req.Host = "iterion.test"
		recorder := httptest.NewRecorder()
		host.ServeHTTP(recorder, req)
		return recorder
	}

	created := request(http.MethodPost, "/x/source/api/workspace/handoffs",
		`{"destination_project":"target","summary":"update the shared bot","source_run_id":"source-run"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var creation struct {
		Ticket    string `json:"ticket"`
		HandoffID string `json:"handoff_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &creation); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(registry.RecentProjects[0].StoreDir, workspaceHandoffDirName, creation.HandoffID+".json")
	persisted, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), creation.Ticket) || !strings.Contains(string(persisted), `"source_run_id": "source-run"`) {
		t.Fatalf("durable record leaked its bearer ticket or lost source binding: %s", persisted)
	}

	redeemed := request(http.MethodPost, "/x/target/api/workspace/handoffs/redeem",
		`{"ticket":"`+creation.Ticket+`","destination_client_id":"target-client","destination_conversation_id":"target-conversation"}`)
	if redeemed.Code != http.StatusOK || !strings.Contains(redeemed.Body.String(), creation.HandoffID) {
		t.Fatalf("redeem = %d: %s", redeemed.Code, redeemed.Body.String())
	}

	wrongBind := request(http.MethodPost, "/x/target/api/workspace/handoffs/bind",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"other-run"}`)
	if wrongBind.Code != http.StatusConflict {
		t.Fatalf("wrong bind = %d: %s", wrongBind.Code, wrongBind.Body.String())
	}
	bound := request(http.MethodPost, "/x/target/api/workspace/handoffs/bind",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run"}`)
	if bound.Code != http.StatusOK {
		t.Fatalf("bind = %d: %s", bound.Code, bound.Body.String())
	}

	receipt := `{"handoff_id":"` + creation.HandoffID + `","destination_run_id":"target-run","status":"completed","summary":"shared bot updated and verified","changed_files":["bots/shared/main.bot"],"commit_sha":"` + strings.Repeat("a", 40) + `","branch":"fix/shared-bot","pr_url":"https://github.com/SocialGouv/iterion/pull/1125"}`
	completed := request(http.MethodPost, "/x/target/api/workspace/handoffs/complete", receipt)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", completed.Code, completed.Body.String())
	}
	var result struct {
		ReceiptID  string `json:"receipt_id"`
		Idempotent bool   `json:"idempotent"`
	}
	if err := json.Unmarshal(completed.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ReceiptID == "" || result.Idempotent {
		t.Fatalf("unexpected first completion: %+v", result)
	}
	repeated := request(http.MethodPost, "/x/target/api/workspace/handoffs/complete", receipt)
	if repeated.Code != http.StatusOK || !strings.Contains(repeated.Body.String(), `"idempotent":true`) {
		t.Fatalf("idempotent complete = %d: %s", repeated.Code, repeated.Body.String())
	}
	conflict := request(http.MethodPost, "/x/target/api/workspace/handoffs/complete",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run","status":"failed","summary":"different terminal result"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting complete = %d: %s", conflict.Code, conflict.Body.String())
	}

	// The outcome must remain pending across a complete workspace restart.
	// A recorded source event on the next process then closes delivery without
	// executing the target action a second time.
	shutdown(host)
	host = nil
	restarted := newHost()
	host = restarted
	reloaded := host.handoffs[creation.HandoffID]
	if reloaded == nil || reloaded.SourceConversationID != "source-conversation" ||
		reloaded.DestinationRunID != "target-run" || reloaded.Outcome == nil ||
		!reloaded.DeliveredAt.IsZero() {
		t.Fatalf("pending durable handoff was not restored: %+v", reloaded)
	}

	_, err = host.runtimes["source"].server.runs.RunStore().AppendEvent(ctx, "source-run", store.Event{
		Type: store.EventHumanAnswersRecorded,
		Data: map[string]any{"answers": map[string]any{"host_event": map[string]any{
			"kind": "workspace-handoff-completed", "receipt_id": result.ReceiptID,
			"authority": "iterion-host", "operator_authorized": false,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host.deliverHandoff(ctx, creation.HandoffID)
	if host.handoffs[creation.HandoffID].DeliveredAt.IsZero() {
		t.Fatal("source receipt was not reconciled as delivered")
	}
}

func TestValidateWorkspaceHandoffReceiptRejectsUnsafeArtifacts(t *testing.T) {
	for _, receipt := range []workspaceHandoffReceipt{
		{Status: "completed", Summary: "ok", ChangedFiles: []string{"../secret"}},
		{Status: "completed", Summary: "ok", PRURL: "https://evil.test/repo/pull/1"},
		{Status: "completed", Summary: "ok", PRURL: "https://github.com/repo/issues/1"},
		{Status: "completed", Summary: "ok", CommitSHA: "abc123"},
	} {
		candidate := receipt
		if err := validateWorkspaceHandoffReceipt(&candidate); err == nil {
			t.Fatalf("unsafe receipt accepted: %+v", receipt)
		}
	}
}
