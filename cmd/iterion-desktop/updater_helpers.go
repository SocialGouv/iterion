//go:build desktop

package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// readEnv is split out so updater.go has a single dependency edge to it.
func readEnv(k string) string { return os.Getenv(k) }

// currentVersion returns the running binary's version string. The build
// system injects this via -ldflags; the leading "v" is stripped before
// semver comparison.
func currentVersion() string {
	v := strings.TrimPrefix(cli.RawVersion(), "v")
	if v == "" || v == "dev" {
		return "0.0.0"
	}
	return v
}

// versionGreater reports whether a > b in semver dotted-component order.
// Lightweight: tolerates non-numeric suffixes by lexicographic compare on
// the suffix. Good enough for our linear release stream; not a full semver.
func versionGreater(a, b string) bool {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	if a == b {
		return false
	}
	as := strings.SplitN(a, "-", 2)
	bs := strings.SplitN(b, "-", 2)
	if cmp := compareDotted(as[0], bs[0]); cmp != 0 {
		return cmp > 0
	}
	// Equal numeric parts: a release without a pre-release suffix
	// trumps one with (1.0.0 > 1.0.0-rc1).
	switch {
	case len(as) == 1 && len(bs) == 2:
		return true
	case len(as) == 2 && len(bs) == 1:
		return false
	case len(as) == 2 && len(bs) == 2:
		return as[1] > bs[1]
	}
	return false
}

func compareDotted(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	for i := 0; i < max(len(ap), len(bp)); i++ {
		var ax, bx int
		if i < len(ap) {
			ax, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bx, _ = strconv.Atoi(bp[i])
		}
		if ax != bx {
			if ax > bx {
				return 1
			}
			return -1
		}
	}
	return 0
}
