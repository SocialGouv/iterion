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

// tarNeutralNotices are GNU tar's notices that leave the archive as whole
// as it can be: a socket, which no archive holds.
var tarNeutralNotices = []string{"socket ignored"}

// TarRace reads the stderr of a GNU tar that exited 1 under TarLocale: the
// members it named as changed or removed while it read them, and whether
// nothing else is there but notices that leave the archive whole and the
// exit-code trailer of `kubectl exec`, through which a kubernetes sandbox
// runs it. kubectl reports its own failures with exit 1 too, on other
// lines, so the content tells them apart. The archive then holds what tar
// caught of those members, which may be no state they were ever in.
func TarRace(stderr string) (members []string, onlyRace bool) {
	only := true
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		body, isTar := strings.CutPrefix(line, "tar: ")
		switch {
		case line == "" || line == KubectlRemoteExit1:
		case !isTar:
			only = false
		default:
			if member, ok := cutNotice(body, tarRaceWarnings); ok {
				members = append(members, member)
			} else if _, ok := cutNotice(body, tarNeutralNotices); !ok {
				only = false
			}
		}
	}
	return members, only && len(members) > 0
}

// OnlyTarRaceWarnings reports whether stderr, from a GNU tar that exited 1
// under TarLocale, names members that changed while it read them and
// nothing that failed (TarRace).
func OnlyTarRaceWarnings(stderr string) bool {
	_, only := TarRace(stderr)
	return only
}

// cutNotice returns the member a tar notice names, when body is one of
// notices about it.
func cutNotice(body string, notices []string) (member string, ok bool) {
	for _, n := range notices {
		if member, ok = strings.CutSuffix(body, ": "+n); ok {
			return member, true
		}
	}
	return "", false
}
