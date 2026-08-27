package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestLocalSkills_CRUD(t *testing.T) {
	// Isolate the GLOBAL skill layer: without this the store's global dir is
	// the operator's real ~/.iterion/skills, and any skill installed there
	// leaks into the list assertion below.
	t.Setenv("ITERION_HOME", t.TempDir())
	_, hs := newTestServer(t)

	// Create (POST).
	body := `{"name":"changelog-writer","body":"---\nname: changelog-writer\ndescription: Writes changelogs\n---\n# body\n","scope":"project"}`
	resp, err := http.Post(hs.URL+"/api/local/skills", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status = %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// List (GET) — should include it, project scope.
	resp, err = http.Get(hs.URL + "/api/local/skills")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Skills []struct {
			Name        string `json:"name"`
			Scope       string `json:"scope"`
			Description string `json:"description"`
		} `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Skills) != 1 || list.Skills[0].Name != "changelog-writer" {
		t.Fatalf("list = %+v, want one changelog-writer", list.Skills)
	}
	if list.Skills[0].Scope != "project" || list.Skills[0].Description != "Writes changelogs" {
		t.Errorf("scope/desc = %q/%q", list.Skills[0].Scope, list.Skills[0].Description)
	}

	// Get one (GET {name}) — body present.
	resp, err = http.Get(hs.URL + "/api/local/skills/changelog-writer")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Body string `json:"body"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Body == "" {
		t.Error("expected non-empty body from GET {name}")
	}

	// Delete (DELETE {name}).
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/api/local/skills/changelog-writer?scope=project", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List again — empty.
	resp, _ = http.Get(hs.URL + "/api/local/skills")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Skills) != 0 {
		t.Errorf("after delete, list = %+v, want empty", list.Skills)
	}
}

func TestLocalSkills_InvalidName(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Post(hs.URL+"/api/local/skills", "application/json",
		bytes.NewBufferString(`{"name":"../evil","body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid name", resp.StatusCode)
	}
}

func TestServerInfo_SkillsEnabledLocal(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/server/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		SkillsEnabled bool `json:"skills_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if !info.SkillsEnabled {
		t.Error("skills_enabled should be true in local mode")
	}
}
