package bundle

import "testing"

func TestLoadManifest_ParsesConfigShareBlock(t *testing.T) {
	body := `name: feedy
schema_version: 1
config_share:
  config_path: feed-watch.json
  editable_paths:
    - "categories.{category}.feeds"
    - "categories.{category}.editorial"
  visible_paths:
    - "categories.{category}.digest_title"
`
	m, err := LoadManifest(writeManifestForTest(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ConfigShare == nil {
		t.Fatal("config_share block not parsed")
	}
	if m.ConfigShare.ConfigPath != "feed-watch.json" {
		t.Errorf("config_path = %q", m.ConfigShare.ConfigPath)
	}
	if len(m.ConfigShare.EditablePaths) != 2 || m.ConfigShare.EditablePaths[0] != "categories.{category}.feeds" {
		t.Errorf("editable_paths = %v", m.ConfigShare.EditablePaths)
	}
	if len(m.ConfigShare.VisiblePaths) != 1 || m.ConfigShare.VisiblePaths[0] != "categories.{category}.digest_title" {
		t.Errorf("visible_paths = %v", m.ConfigShare.VisiblePaths)
	}
}

func TestLoadManifest_NoConfigShareBlockIsNil(t *testing.T) {
	m, err := LoadManifest(writeManifestForTest(t, "name: plain\nschema_version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ConfigShare != nil {
		t.Errorf("expected nil ConfigShare, got %+v", m.ConfigShare)
	}
}
