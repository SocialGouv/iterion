package native_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// restGrantSentinel is the bearer planted on the card. A surface printing it
// names itself in the failure message.
const restGrantSentinel = "sentinel-forge-grant-2036-rest"

// plantCardCarryingAGrant is the card the read mask exists for: one written
// BEFORE the store-boundary drop, which still holds a live publish bearer in
// its bot args. It is planted the only way such a card can exist now — in the
// store's files, behind the write boundary — and the returned store has read it
// into its index.
func plantCardCarryingAGrant(t *testing.T) (*httptest.Server, *native.Store, string) {
	t.Helper()
	root := t.TempDir()
	seed, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	iss, err := seed.Create(native.Issue{Title: "a card from before the drop", State: "ready", BotArgs: map[string]string{"loop_cap": "5"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, "issues", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("issue files = %v (err %v), want exactly one", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read card: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	doc["bot_args"] = map[string]string{"loop_cap": "5", store.ForgePublishTokenVar: restGrantSentinel}
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if err := os.WriteFile(files[0], patched, 0o600); err != nil {
		t.Fatalf("write card: %v", err)
	}
	s, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	held, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if held.BotArgs[store.ForgePublishTokenVar] != restGrantSentinel {
		t.Fatalf("the planted card does not carry the grant (%v) — the fixture, not the mask, is broken", held.BotArgs)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux, "")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s, iss.ID
}

// TestTheRESTSurfaceNeverHandsOutACardsPublishGrant — GET one card and GET the
// list, the two shapes the board serves.
func TestTheRESTSurfaceNeverHandsOutACardsPublishGrant(t *testing.T) {
	srv, _, id := plantCardCarryingAGrant(t)

	for _, route := range []struct {
		what string
		url  string
	}{
		{"GET /issues/{id}", srv.URL + "/issues/" + id},
		{"GET /issues", srv.URL + "/issues"},
	} {
		body := getBody(t, route.url)
		if strings.Contains(body, restGrantSentinel) {
			t.Errorf("%s hands out the bearer: %s", route.what, body)
		}
		if !strings.Contains(body, store.RedactedLaunchVar) {
			t.Errorf("%s says nothing about the grant the card holds (no mask): %s", route.what, body)
		}
		if !strings.Contains(body, "loop_cap") {
			t.Errorf("%s lost the card's own bot args: %s", route.what, body)
		}
	}
}

// TestTheRESTWriteBoundaryDropsAGrantAClientSent — the public break the
// change ships: a POST (and a PATCH) carrying grant vars gets a card without
// them, on the response AND in the store.
func TestTheRESTWriteBoundaryDropsAGrantAClientSent(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()

	post := bytes.NewBufferString(`{"title":"ship X","state":"ready","bot_args":{"loop_cap":"5","forge_publish_token":"` + restGrantSentinel + `","forge_publish_url":"https://attacker.example/publish"}}`)
	r, err := http.Post(srv.URL+"/issues", "application/json", post)
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
	for _, name := range store.ServerMintedLaunchVars {
		if got, ok := created.BotArgs[name]; ok {
			t.Errorf("POST response carries %s = %q", name, got)
		}
	}
	held, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, ok := held.BotArgs[store.ForgePublishTokenVar]; ok {
		t.Errorf("the stored card carries the bearer: %q", got)
	}
	if held.BotArgs["loop_cap"] != "5" {
		t.Errorf("the client's own arg was lost: %v", held.BotArgs)
	}

	patch := bytes.NewBufferString(`{"bot_args":{"loop_cap":"9","forge_publish_token":"` + restGrantSentinel + `"}}`)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/issues/"+created.ID, patch)
	req.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	patchedBody, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	if strings.Contains(string(patchedBody), restGrantSentinel) {
		t.Errorf("PATCH response carries the bearer: %s", patchedBody)
	}
	after, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after patch: %v", err)
	}
	if got, ok := after.BotArgs[store.ForgePublishTokenVar]; ok {
		t.Errorf("the patched card carries the bearer: %q", got)
	}
	if after.BotArgs["loop_cap"] != "9" {
		t.Errorf("the patch's own arg was lost: %v", after.BotArgs)
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, r.StatusCode)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}
