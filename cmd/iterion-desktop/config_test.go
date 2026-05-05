package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadConfig_Missing_ReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if c.Version != configSchemaVersion {
		t.Errorf("Version = %d, want %d", c.Version, configSchemaVersion)
	}
	if c.Window.Width <= 0 || c.Window.Height <= 0 {
		t.Errorf("default window size missing: %+v", c.Window)
	}
	if c.Updater.Channel != ChannelStable {
		t.Errorf("default channel = %q, want %q", c.Updater.Channel, ChannelStable)
	}
	if !c.Updater.AutoCheck {
		t.Error("default AutoCheck should be true")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := NewConfig()
	c.path = path
	c.AddProject(filepath.Join(dir, "project-a"))
	c.AddProject(filepath.Join(dir, "project-b"))
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if len(got.RecentProjects) != 2 {
		t.Fatalf("RecentProjects len = %d, want 2", len(got.RecentProjects))
	}
	// MRU order: project-b (added last) is first.
	if filepath.Base(got.RecentProjects[0].Dir) != "project-b" {
		t.Errorf("MRU head = %q, want project-b", got.RecentProjects[0].Dir)
	}
	if got.CurrentProjectID != got.RecentProjects[0].ID {
		t.Errorf("CurrentProjectID = %q, want MRU head %q",
			got.CurrentProjectID, got.RecentProjects[0].ID)
	}
}

func TestAddProject_DuplicateRefreshes(t *testing.T) {
	c := NewConfig()
	dir := "/tmp/foo"
	p1 := c.AddProject(dir)
	p2 := c.AddProject(dir)
	if p1.ID != p2.ID {
		t.Errorf("expected stable ID across re-add, got %q vs %q", p1.ID, p2.ID)
	}
	if len(c.RecentProjects) != 1 {
		t.Errorf("expected single entry, got %d", len(c.RecentProjects))
	}
}

func TestRemoveProject_PromotesNewCurrent(t *testing.T) {
	c := NewConfig()
	a := c.AddProject("/tmp/a")
	b := c.AddProject("/tmp/b")
	if c.CurrentProjectID != b.ID {
		t.Fatalf("setup: current should be b, got %q", c.CurrentProjectID)
	}
	if !c.RemoveProject(b.ID) {
		t.Fatal("RemoveProject returned false")
	}
	if c.CurrentProjectID != a.ID {
		t.Errorf("CurrentProjectID after remove = %q, want %q", c.CurrentProjectID, a.ID)
	}
}

func TestSetCurrentProject_BumpsLastOpened(t *testing.T) {
	c := NewConfig()
	a := c.AddProject("/tmp/a")
	b := c.AddProject("/tmp/b")
	if c.RecentProjects[0].ID != b.ID {
		t.Fatalf("setup: MRU head should be b")
	}
	if !c.SetCurrentProject(a.ID) {
		t.Fatal("SetCurrentProject returned false")
	}
	if c.RecentProjects[0].ID != a.ID {
		t.Errorf("MRU head after switch = %q, want %q", c.RecentProjects[0].ID, a.ID)
	}
}

func TestMigrateConfig_FromV0(t *testing.T) {
	// v0 docs have Version=0 and possibly missing defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
		"recent_projects": [],
		"current_project_id": ""
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if c.Version != 1 {
		t.Errorf("Version after migrate = %d, want 1", c.Version)
	}
	if c.Window.Width != 1400 || c.Window.Height != 900 {
		t.Errorf("window defaults not applied: %+v", c.Window)
	}
	if c.Updater.Channel != ChannelStable {
		t.Errorf("updater channel default not applied: %q", c.Updater.Channel)
	}
}

func TestSave_AtomicConcurrent(t *testing.T) {
	// Two parallel Save calls must never produce a corrupt file. We don't
	// verify the final content (the winner is non-deterministic), only
	// that the file always parses cleanly.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := NewConfig()
	c.path = path
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.AddProject(filepath.Join(dir, "p"))
			_ = c.Save()
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed Config
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Errorf("file unparseable after concurrent saves: %v", err)
	}
}
