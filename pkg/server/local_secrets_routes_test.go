package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// /api/local/secrets is the desktop/local studio's whole secret surface: the
// Secrets view calls nothing else, and what it stores is what
// ResolveLocalCredentials later injects into a run. Two invariants make it
// worth an end-to-end test rather than a store-level one — the value must be
// SEALED on the way to disk (an operator's API key must never be readable in
// ~/.iterion/secrets.json), and no response may ever echo it back.
//
// Both are properties of the HANDLERS composed with the store, so the test
// drives the real routes on a server built by New() and takes the FILE on
// disk as the oracle: plaintext absent, name/last4 present.

type localSecretsE2E struct {
	hs         *httptest.Server
	globalPath string
}

func newLocalSecretsE2E(t *testing.T) *localSecretsE2E {
	t.Helper()
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "secrets.json")
	global, err := secrets.NewFileGenericSecretStore(globalPath)
	if err != nil {
		t.Fatalf("global store: %v", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	s := New(Config{
		WorkDir:                 t.TempDir(),
		Bind:                    "127.0.0.1",
		SkipProjectRegistration: true,
		GenericSecrets:          secrets.NewLayeredGenericSecretStore(global, nil),
		Sealer:                  sealer,
		// The local studio's own shape: one operator, trusted to their TTY
		// (pkg/cli/studio.go). These routes only exist in that mode.
		DisableAuth: true,
	}, iterlog.New(iterlog.LevelError, nil))

	// Guard the premise: without the layered store selected by the real
	// wiring, the routes below are not registered at all and every
	// assertion would be about a 404.
	if s.localSecretStore() == nil {
		t.Fatal("server did not wire the local layered secret store")
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return &localSecretsE2E{hs: hs, globalPath: globalPath}
}

func (l *localSecretsE2E) call(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, l.hs.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// onDisk is the sealed store file as raw text — the oracle for "was it
// actually persisted" and "was it actually sealed".
func (l *localSecretsE2E) onDisk(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(l.globalPath)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	return string(raw)
}

func (l *localSecretsE2E) list(t *testing.T) []localSecretView {
	t.Helper()
	status, body := l.call(t, http.MethodGet, "/api/local/secrets", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d body=%s", status, body)
	}
	var got struct {
		Secrets []localSecretView `json:"secrets"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	return got.Secrets
}

func TestLocalSecrets_CreateListDeleteSealsAndNeverEchoesTheValue(t *testing.T) {
	l := newLocalSecretsE2E(t)
	const plaintext = "sk-plaintext-must-never-be-readable-42"

	status, body := l.call(t, http.MethodPost, "/api/local/secrets",
		`{"name":"DEPLOY_TOKEN","secret":"`+plaintext+`","allowed_hosts":["api.example.com"]}`)
	if status != http.StatusOK {
		t.Fatalf("create = %d body=%s", status, body)
	}
	var created localSecretView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, body)
	}
	if created.ID == "" || created.Name != "DEPLOY_TOKEN" {
		t.Fatalf("create returned %+v", created)
	}
	if created.Scope != "global" {
		t.Fatalf("scope = %q, want global (no project layer is active)", created.Scope)
	}
	if strings.Contains(string(body), plaintext) {
		t.Fatal("create response echoed the plaintext secret")
	}
	if created.Last4 != plaintext[len(plaintext)-4:] {
		t.Fatalf("last4 = %q, want the value's last 4", created.Last4)
	}

	t.Run("the value is sealed on disk, never stored in clear", func(t *testing.T) {
		raw := l.onDisk(t)
		if strings.Contains(raw, plaintext) {
			t.Fatal("the store file holds the plaintext secret")
		}
		if !strings.Contains(raw, "DEPLOY_TOKEN") {
			t.Fatalf("the store file does not name the secret: %s", raw)
		}
	})

	t.Run("it is listed with its metadata and no value", func(t *testing.T) {
		got := l.list(t)
		if len(got) != 1 || got[0].ID != created.ID {
			t.Fatalf("list = %+v, want the created secret", got)
		}
		if got[0].Last4 != created.Last4 || got[0].Fingerprint == "" {
			t.Fatalf("list dropped the metadata: %+v", got[0])
		}
		if len(got[0].AllowedHosts) != 1 || got[0].AllowedHosts[0] != "api.example.com" {
			t.Fatalf("allowed hosts = %v, want the egress lock as created", got[0].AllowedHosts)
		}
	})

	t.Run("re-posting the same name rotates in place and keeps the egress lock", func(t *testing.T) {
		const rotated = "sk-rotated-value-99"
		status, body := l.call(t, http.MethodPost, "/api/local/secrets",
			`{"name":"DEPLOY_TOKEN","secret":"`+rotated+`"}`)
		if status != http.StatusOK {
			t.Fatalf("rotate = %d body=%s", status, body)
		}
		got := l.list(t)
		if len(got) != 1 {
			t.Fatalf("rotate duplicated the record: %+v", got)
		}
		if got[0].Last4 != rotated[len(rotated)-4:] {
			t.Fatalf("last4 = %q, want the rotated value's — the rotation did not land", got[0].Last4)
		}
		// A rotation that omits allowed_hosts must not silently broaden egress.
		if len(got[0].AllowedHosts) != 1 || got[0].AllowedHosts[0] != "api.example.com" {
			t.Fatalf("rotation widened egress to %v", got[0].AllowedHosts)
		}
		if raw := l.onDisk(t); strings.Contains(raw, rotated) {
			t.Fatal("the rotated value landed unsealed on disk")
		}
	})

	t.Run("delete removes it from the API and from the file", func(t *testing.T) {
		if status, body := l.call(t, http.MethodDelete, "/api/local/secrets/"+created.ID, ""); status != http.StatusNoContent {
			t.Fatalf("delete = %d body=%s, want 204", status, body)
		}
		if got := l.list(t); len(got) != 0 {
			t.Fatalf("deleted secret still listed: %+v", got)
		}
		if raw := l.onDisk(t); strings.Contains(raw, "DEPLOY_TOKEN") {
			t.Fatalf("deleted secret still on disk: %s", raw)
		}
	})
}

// The rename path guards a store-level invariant: two records sharing a Name
// in one layer make GetByName resolve nondeterministically, so the rename that
// would create the collision is refused (409) and neither record changes.
func TestLocalSecrets_RenameOntoAnExistingNameIsRefused(t *testing.T) {
	l := newLocalSecretsE2E(t)

	mk := func(name, value string) localSecretView {
		t.Helper()
		status, body := l.call(t, http.MethodPost, "/api/local/secrets", `{"name":"`+name+`","secret":"`+value+`"}`)
		if status != http.StatusOK {
			t.Fatalf("create %s = %d body=%s", name, status, body)
		}
		var v localSecretView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return v
	}
	first := mk("ALPHA", "value-alpha")
	second := mk("BETA", "value-beta")

	status, body := l.call(t, http.MethodPatch, "/api/local/secrets/"+second.ID, `{"name":"ALPHA"}`)
	if status != http.StatusConflict {
		t.Fatalf("colliding rename = %d body=%s, want 409", status, body)
	}
	got := l.list(t)
	names := map[string]string{}
	for _, v := range got {
		names[v.ID] = v.Name
	}
	if names[first.ID] != "ALPHA" || names[second.ID] != "BETA" {
		t.Fatalf("refused rename still mutated the store: %+v", got)
	}

	t.Run("a non-colliding rename lands", func(t *testing.T) {
		if status, body := l.call(t, http.MethodPatch, "/api/local/secrets/"+second.ID, `{"name":"GAMMA"}`); status != http.StatusOK {
			t.Fatalf("rename = %d body=%s", status, body)
		}
		for _, v := range l.list(t) {
			if v.ID == second.ID && v.Name != "GAMMA" {
				t.Fatalf("rename did not persist: %+v", v)
			}
		}
	})

	t.Run("an unknown id is a 404, not a silent success", func(t *testing.T) {
		if status, body := l.call(t, http.MethodPatch, "/api/local/secrets/does-not-exist", `{"name":"NOPE"}`); status != http.StatusNotFound {
			t.Fatalf("patch unknown id = %d body=%s, want 404", status, body)
		}
	})
}
