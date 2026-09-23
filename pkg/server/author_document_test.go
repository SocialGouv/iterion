package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch endpoint refuses an author document by the name the caller
// asked for, with a stable code, before bot resolution rewrites the path
// and before an inline source is materialised: no cache file, no run.
func TestLaunchRefusesAnAuthorDocumentBeforeAnyEffect(t *testing.T) {
	srv, hs := newTestServer(t)
	if err := os.WriteFile(filepath.Join(srv.cfg.WorkDir, "draft.bot.yaml"), []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"file_path":"draft.bot.yaml"}`,
		`{"file_path":"draft.bot.yaml","source":"dsl: 2\n"}`,
	} {
		resp, err := http.Post(hs.URL+"/api/runs", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400 (%v)", body, resp.StatusCode, got)
		}
		if got["error_code"] != "author_document" {
			t.Fatalf("%s: code %q, want author_document (%v)", body, got["error_code"], got)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(srv.cfg.StoreDir, "inline-sources")); err == nil && len(entries) > 0 {
		t.Fatalf("a refused launch materialised an inline source: %d file(s)", len(entries))
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a refused launch left run docs behind: %v", ids)
	}
}

// Resume and the WebSocket answer reach the same source resolver as the
// launch: a draft named there is refused before it is materialised, with the
// same stable code on the HTTP resume, and the run it was meant for is left
// as it was.
func TestResumeRefusesAnAuthorDocumentBeforeAnyEffect(t *testing.T) {
	srv, hs := newTestServer(t)
	if err := os.WriteFile(filepath.Join(srv.cfg.WorkDir, "draft.bot.yaml"), []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.resolveWorkflowPath("draft.bot.yaml", ""); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("resolveWorkflowPath: %v, want ErrAuthorDocument", err)
	}
	if _, err := srv.resolveWorkflowPath("draft.bot.yaml", "dsl: 2\n"); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("resolveWorkflowPath with an inline source: %v, want ErrAuthorDocument", err)
	}
	seedRun(t, srv, "run-p", "wf", store.RunStatusFailedResumable)
	resp, err := http.Post(hs.URL+"/api/runs/run-p/resume", "application/json", bytes.NewReader([]byte(`{"file_path":"draft.bot.yaml"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || got["error_code"] != "author_document" {
		t.Fatalf("resume: status %d error_code %q, want 400 author_document (%v)", resp.StatusCode, got["error_code"], got)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(context.Background(), "run-p")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("a refused resume changed the run's status to %s", run.Status)
	}
	if entries, err := os.ReadDir(filepath.Join(srv.cfg.StoreDir, "inline-sources")); err == nil && len(entries) > 0 {
		t.Fatalf("a refused resume materialised an inline source: %d file(s)", len(entries))
	}
}

// The WebSocket answer reaches the same source resolver as launch and
// resume: a draft named there is refused with the same stable code in the
// error envelope, before anything is resumed, and the run is left as it was.
func TestWebSocketAnswerRefusesAnAuthorDocument(t *testing.T) {
	srv, hs := newTestServer(t)
	if err := os.WriteFile(filepath.Join(srv.cfg.WorkDir, "draft.bot.yaml"), []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRun(t, srv, "run-ws", "wf", store.RunStatusFailedResumable)
	c := dialRunWS(t, hs, "run-ws")
	payload, err := json.Marshal(wsAnswerRequest{FilePath: "draft.bot.yaml", Answers: map[string]any{"q": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeAnswer, AckID: "a1", Payload: payload})
	env := readEnvelope(t, c, wsTypeError)
	var p wsErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if p.Code != "author_document" {
		t.Fatalf("answer with a draft: code %q, want author_document (%s)", p.Code, p.Message)
	}
	if env.AckID != "a1" {
		t.Fatalf("AckID = %q, want a1", env.AckID)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(context.Background(), "run-ws")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("a refused answer changed the run's status to %s", run.Status)
	}
}

// The launch handler checks the identity the caller asked for BEFORE bot
// resolution rewrites it: with a bot id beside it (or, in cloud, a
// catalog-shaped path, which the inference reads without looking at the
// file name), resolution would replace the draft's path with the catalog
// bot's .bot and launch the truth under the draft's name — the resolver
// behind the handler never sees the draft, so only this door can refuse it.
func TestLaunchRefusesAnAuthorDocumentBeforeBotResolution(t *testing.T) {
	srv, hs := newTestServer(t)
	botDir := filepath.Join(srv.cfg.WorkDir, "bots", "drafted")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "main.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	// A manifest beside main.bot is what makes the directory a bundle the
	// registry discovers under its name — without it, the bot id would not
	// resolve and the draft path would fall through to the resolver, whose
	// own refusal is not this door's.
	if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"), []byte("name: drafted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(hs.URL+"/api/runs", "application/json", bytes.NewReader([]byte(`{"bot_id":"drafted","file_path":"bots/drafted/main.bot.yaml"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (%v)", resp.StatusCode, got)
	}
	if got["error_code"] != "author_document" {
		t.Fatalf("error_code %q, want author_document (%v)", got["error_code"], got)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("the draft's name launched the catalog bot: %v", ids)
	}
}
