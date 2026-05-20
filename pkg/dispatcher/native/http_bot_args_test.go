package native_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func TestHTTPCreate_BotAndArgs(t *testing.T) {
	srv, _ := newServerWithStore(t)
	defer srv.Close()

	body := bytes.NewBufferString(`{
	  "title": "ship X",
	  "state": "ready",
	  "bot": "feature_dev",
	  "bot_args": {"workspace_dir": "/tmp", "loop_cap": "5"}
	}`)
	r, err := http.Post(srv.URL+"/issues", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", r.StatusCode)
	}
	var created native.Issue
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if created.Bot != "feature_dev" {
		t.Errorf("Bot = %q", created.Bot)
	}
	if created.BotArgs["loop_cap"] != "5" {
		t.Errorf("BotArgs[loop_cap] = %q", created.BotArgs["loop_cap"])
	}
}

func TestHTTPPatch_ReplacesBotArgs(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()
	iss, _ := s.Create(native.Issue{
		Title:   "x",
		State:   "ready",
		BotArgs: map[string]string{"a": "1", "b": "2"},
	})

	patch := bytes.NewBufferString(`{"bot_args": {"a": "9", "c": "3"}}`)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/issues/"+iss.ID, patch)
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	var upd native.Issue
	_ = json.NewDecoder(r.Body).Decode(&upd)
	r.Body.Close()
	if upd.BotArgs["a"] != "9" {
		t.Errorf("a = %q", upd.BotArgs["a"])
	}
	if _, ok := upd.BotArgs["b"]; ok {
		t.Errorf("PATCH should have dropped b: %v", upd.BotArgs)
	}
	if upd.BotArgs["c"] != "3" {
		t.Errorf("c = %q", upd.BotArgs["c"])
	}
}
