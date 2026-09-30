package sandbox

import "strings"

// tarRaceWarnings are GNU tar's warnings for a tree that changed while it was
// archived: a file rewritten, or listed and then removed. The archive is
// complete — each file is the state tar caught — and tar exits 1 ("some files
// differ").
var tarRaceWarnings = []string{"file changed as we read it", "File removed before we read it"}

// KubectlRemoteExit1 is the line `kubectl exec` itself adds to stderr when the
// remote command exits 1.
const KubectlRemoteExit1 = "command terminated with exit code 1"

// TarLocale is the LC_ALL a tar whose stderr OnlyTarRaceWarnings reads runs
// under: its messages untranslated.
const TarLocale = "C"

// OnlyTarRaceWarnings reports whether stderr, from a GNU tar that exited 1
// under TarLocale, carries its warnings for a tree that changed while it was
// archived and nothing else but the exit-code trailer of `kubectl exec`,
// through which a kubernetes sandbox runs it. kubectl reports its own
// failures with exit 1 too, on other lines, so the content tells them apart.
func OnlyTarRaceWarnings(stderr string) bool {
	seen := false
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line == KubectlRemoteExit1:
		case strings.HasPrefix(line, "tar: ") && isTarRaceWarning(line):
			seen = true
		default:
			return false
		}
	}
	return seen
}

func isTarRaceWarning(line string) bool {
	for _, w := range tarRaceWarnings {
		if strings.HasSuffix(line, w) {
			return true
		}
	}
	return false
}
