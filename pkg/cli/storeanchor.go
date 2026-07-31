package cli

import (
	"os"
	"path/filepath"
)

// storeAnchorDir returns the directory a command should hand to
// store.ResolveStoreDir.
//
// The working directory wins. A .bot that lives inside the project it drives
// is the common case, and anchoring on the bot's own directory keyed the store
// on that subdirectory: `iterion run project/bots/x/main.bot` wrote to
// ~/.iterion/projects/<project-bots-x-key>/ while resume, inspect, issue,
// dispatch and the studio all resolved <project>/.iterion. Launching worked
// and every follow-up reported "run not found".
//
// The bot's directory stays as the fallback for the case the working
// directory is genuinely unavailable, and for an empty iterFile the caller
// still gets a usable anchor.
func storeAnchorDir(iterFile string) string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if iterFile == "" {
		return ""
	}
	return filepath.Dir(iterFile)
}
