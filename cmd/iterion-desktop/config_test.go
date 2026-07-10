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

func TestProjectByID(t *testing.T) {
	c := NewConfig()
	a := c.AddProject("/tmp/a")
	b := c.AddProject("/tmp/b")
	got := c.ProjectByID(a.ID)
	if got == nil || got.Dir != "/tmp/a" {
		t.Fatalf("ProjectByID(a) = %+v, want Dir=/tmp/a", got)
	}
	got = c.ProjectByID(b.ID)
	if got == nil || got.Dir != "/tmp/b" {
		t.Fatalf("ProjectByID(b) = %+v, want Dir=/tmp/b", got)
	}
	if c.ProjectByID("nonexistent") != nil {
		t.Errorf("ProjectByID(nonexistent) should return nil")
	}
	// Returned value must be a copy: mutating it does not affect the slice.
	got.Dir = "/tmp/MUTATED"
	again := c.ProjectByID(a.ID)
	if again.Dir != "/tmp/a" {
		t.Errorf("ProjectByID returned a live pointer, got Dir=%q after mutation", again.Dir)
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
	// A v0 doc migrates all the way to the current schema version.
	if c.Version != configSchemaVersion {
		t.Errorf("Version after migrate = %d, want %d", c.Version, configSchemaVersion)
	}
	if c.Window.Width != 1400 || c.Window.Height != 900 {
		t.Errorf("window defaults not applied: %+v", c.Window)
	}
	if c.Updater.Channel != ChannelStable {
		t.Errorf("updater channel default not applied: %q", c.Updater.Channel)
	}
}

func TestMigrateConfig_V1ToV2_StampsLocalKind(t *testing.T) {
	// A v1 doc held only local projects with no Kind field. Migration must
	// stamp them local and bump to v2, without touching other fields.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
		"version": 1,
		"recent_projects": [
			{"id": "p1", "name": "alpha", "dir": "/tmp/alpha"},
			{"id": "p2", "name": "beta", "dir": "/tmp/beta"}
		],
		"current_project_id": "p1"
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if c.Version != configSchemaVersion {
		t.Errorf("Version after migrate = %d, want %d", c.Version, configSchemaVersion)
	}
	for _, p := range c.RecentProjects {
		if p.Kind != ProjectKindLocal {
			t.Errorf("project %q Kind = %q, want %q", p.ID, p.Kind, ProjectKindLocal)
		}
		if p.IsCloud() {
			t.Errorf("migrated local project %q reports IsCloud()", p.ID)
		}
	}
	if c.CurrentProjectID != "p1" {
		t.Errorf("CurrentProjectID = %q, want p1 (unchanged)", c.CurrentProjectID)
	}
}

func TestAddCloudConnection(t *testing.T) {
	c := NewConfig()
	p := c.AddCloudConnection("https://cloud.example.io", "user-1", "a@b.io", "")
	if !p.IsCloud() {
		t.Fatalf("AddCloudConnection produced non-cloud entry: %+v", p)
	}
	if p.Name != "cloud.example.io" {
		t.Errorf("derived name = %q, want host cloud.example.io", p.Name)
	}
	if p.CloudURL != "https://cloud.example.io" || p.CloudUserID != "user-1" || p.CloudEmail != "a@b.io" {
		t.Errorf("cloud fields not set: %+v", p)
	}
	if c.CurrentProjectID != p.ID {
		t.Errorf("cloud connection did not become current: %q vs %q", c.CurrentProjectID, p.ID)
	}
	// Re-adding the same (url,user) refreshes rather than duplicating.
	p2 := c.AddCloudConnection("https://cloud.example.io", "user-1", "new@b.io", "My Cloud")
	if p2.ID != p.ID {
		t.Errorf("re-add produced new ID %q, want stable %q", p2.ID, p.ID)
	}
	if len(c.RecentProjects) != 1 {
		t.Errorf("re-add duplicated entry: %d projects", len(c.RecentProjects))
	}
	if got := c.ProjectByID(p.ID); got == nil || got.CloudEmail != "new@b.io" || got.Name != "My Cloud" {
		t.Errorf("re-add did not refresh email/name: %+v", got)
	}
	// A different user on the same URL is a distinct connection.
	p3 := c.AddCloudConnection("https://cloud.example.io", "user-2", "c@b.io", "")
	if p3.ID == p.ID || len(c.RecentProjects) != 2 {
		t.Errorf("distinct user should add a second connection; got %d projects", len(c.RecentProjects))
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
