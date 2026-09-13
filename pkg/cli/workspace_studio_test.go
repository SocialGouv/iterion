package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/server/projects"
)

func TestImportLegacyInstancesPinsProjectConfiguration(t *testing.T) {
	townRoot := t.TempDir()
	tabarriaRoot := t.TempDir()
	townStore := filepath.Join(townRoot, ".iterion")
	tabarriaStore := filepath.Join(t.TempDir(), "tabarria-store")
	for _, dir := range []string{townStore, tabarriaStore} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".env"), []byte("TOWN=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "instances.conf")
	contents := fmt.Sprintf(
		"LOAD_PROJECT_ENV=true\nTown %s 4892 local-store --bots-path %s\nTabarria %s 4893 --store-dir %s no-env\n",
		townRoot,
		filepath.Join(townRoot, "bots"),
		tabarriaRoot,
		tabarriaStore,
	)
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_INSTANCES_CONFIG", configPath)

	registry := &projects.Config{Version: 1}
	if err := importLegacyInstances(registry, iterlog.Nop()); err != nil {
		t.Fatal(err)
	}
	if len(registry.RecentProjects) != 2 {
		t.Fatalf("projects = %+v", registry.RecentProjects)
	}
	byDir := map[string]projects.Project{}
	for _, project := range registry.RecentProjects {
		byDir[project.Dir] = project
	}
	town := byDir[townRoot]
	if town.StoreDir != townStore || town.EnvFile != filepath.Join(townRoot, ".env") ||
		len(town.BotsPaths) != 1 || town.BotsPaths[0] != filepath.Join(townRoot, "bots") {
		t.Fatalf("town = %+v", town)
	}
	tabarria := byDir[tabarriaRoot]
	if tabarria.StoreDir != tabarriaStore || tabarria.EnvFile != "" {
		t.Fatalf("tabarria = %+v", tabarria)
	}
}
