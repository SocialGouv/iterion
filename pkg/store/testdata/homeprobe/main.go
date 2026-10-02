// Command homeprobe prints how a production binary — one `go build` made,
// where testing.Testing() is false — resolves the iterion home. The store's
// tests build and run it: nothing else exercises the resolution outside a
// test binary.
package main

import (
	"encoding/json"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
)

func main() {
	home, err := store.IterionHome()
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	inheritedHome, inheritedErr := store.InheritedIterionHome()
	inheritedErrText := ""
	if inheritedErr != nil {
		inheritedErrText = inheritedErr.Error()
	}
	processDir, underTest := store.TestProcessDir()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"iterion_home":         home,
		"iterion_home_error":   errText,
		"global":               store.GlobalIterionDataDir(),
		"inherited":            store.InheritedIterionDataDir(),
		"inherited_home":       inheritedHome,
		"inherited_home_error": inheritedErrText,
		"test_process_dir":     processDir,
		"under_test":           underTest,
	}); err != nil {
		os.Exit(1)
	}
}
