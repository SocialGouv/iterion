package ciguard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/treeskip"
)

// versionPin matches a Dockerfile line that pins a tool version through a
// build argument or an environment variable: `ARG GH_VERSION=2.105.0`.
var versionPin = regexp.MustCompile(`^\s*(?:ARG|ENV)\s+([A-Z0-9_]*VERSION)=(\S*)`)

// unmanagedPins are the version pins Renovate deliberately does not move,
// each with the reason. Keys are "<repo-relative path>:<NAME>". An entry that
// no longer matches a line fails TestEveryDockerfileVersionPinIsManaged too:
// a stale exception would silently cover the next pin of that name.
var unmanagedPins = map[string]string{
	"Dockerfile:VERSION":                         "release build argument the image workflow stamps, not a pin",
	"Dockerfile:ITERION_VERSION":                 "derived from the VERSION build argument",
	"sandbox/sec/Dockerfile:GOSEC_VERSION":       "`latest` by design: every sec image build installs the current scanner",
	"sandbox/sec/Dockerfile:GOVULNCHECK_VERSION": "`latest` by design: every sec image build installs the current scanner",
	"sandbox/sec/Dockerfile:JSXRAY_VERSION":      "an npm range (^15) that floats within the major js-x-ray's ESM shim is written for",
	"Dockerfile.runner-devbox:ULTRA11Y_VERSION":  "moves only through bots/rgaa-audit/ultra11y.pin, which internal/sweep/sandboximages/rgaa_audit_engine_pin_test.go holds to this ARG",
	"sandbox/rgaa/Dockerfile:ULTRA11Y_VERSION":   "moves only through bots/rgaa-audit/ultra11y.pin, which internal/sweep/sandboximages/rgaa_audit_sandbox_rgaa_image_test.go holds to this ARG",
}

// goModAlignedPins name the pins that follow go.mod instead of Renovate;
// TestDockerfileGoAndWailsPinsFollowGoMod holds them there.
var goModAlignedPins = map[string]bool{"GO_VERSION": true, "WAILS_VERSION": true}

// renovateConfig is the part of renovate.json these guards read.
type renovateConfig struct {
	IgnorePaths    []string `json:"ignorePaths"`
	CustomManagers []struct {
		ManagerFilePatterns []string `json:"managerFilePatterns"`
		MatchStrings        []string `json:"matchStrings"`
	} `json:"customManagers"`
}

// inlinePinFilePatterns returns the file patterns of the custom manager that
// reads `# renovate: datasource=…` comments, compiled from Renovate's
// `/regex/` notation.
func inlinePinFilePatterns(t *testing.T, cfg renovateConfig) []*regexp.Regexp {
	t.Helper()
	var out []*regexp.Regexp
	for _, m := range cfg.CustomManagers {
		inline := false
		for _, s := range m.MatchStrings {
			if strings.Contains(s, "# renovate: datasource=") {
				inline = true
			}
		}
		if !inline {
			continue
		}
		for _, p := range m.ManagerFilePatterns {
			if len(p) < 2 || p[0] != '/' || p[len(p)-1] != '/' {
				t.Fatalf("renovate.json managerFilePatterns entry %q is not a /regex/ this guard can read", p)
			}
			re, err := regexp.Compile(p[1 : len(p)-1])
			if err != nil {
				t.Fatalf("renovate.json managerFilePatterns entry %q: %v", p, err)
			}
			out = append(out, re)
		}
	}
	if len(out) == 0 {
		t.Fatal("renovate.json has no custom manager reading `# renovate: datasource=` comments — the inline pins lost their manager, or this guard lost track of it")
	}
	return out
}

func readRenovateConfig(t *testing.T) renovateConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "renovate.json"))
	if err != nil {
		t.Fatalf("read renovate.json: %v", err)
	}
	var cfg renovateConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse renovate.json: %v", err)
	}
	return cfg
}

// dockerfilePin is one version pin found in a Dockerfile.
type dockerfilePin struct {
	file, name, value string
	line              int
	annotated         bool // the line above is a `# renovate:` comment
}

// dockerfilePins walks the tree and returns every version pin of every
// Dockerfile Renovate scans: the shared skip set and renovate.json's own
// ignorePaths are left out.
func dockerfilePins(t *testing.T, cfg renovateConfig) []dockerfilePin {
	t.Helper()
	var pins []dockerfilePin
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(repoRoot, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (treeskip.Dir(d.Name()) || renovateIgnores(cfg, rel+"/")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isDockerfile(d.Name()) || renovateIgnores(cfg, rel) {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(raw), "\n")
		for i, l := range lines {
			m := versionPin.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			annotated := i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "# renovate:")
			pins = append(pins, dockerfilePin{file: rel, name: m[1], value: m[2], line: i + 1, annotated: annotated})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for Dockerfiles: %v", err)
	}
	if len(pins) == 0 {
		t.Fatal("no Dockerfile version pin found — the walk read nothing, so this guard proves nothing")
	}
	return pins
}

func isDockerfile(name string) bool {
	return name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || name == "Containerfile"
}

// renovateIgnores reports whether renovate.json's ignorePaths cover rel. Its
// entries in this repository are `<dir>/**` globs.
func renovateIgnores(cfg renovateConfig, rel string) bool {
	for _, g := range cfg.IgnorePaths {
		if prefix, ok := strings.CutSuffix(g, "**"); ok && strings.HasPrefix(rel, prefix) {
			return true
		}
		if ok, _ := path.Match(g, rel); ok {
			return true
		}
	}
	return false
}

// TestEveryDockerfileVersionPinIsManaged fails on a tool version pinned in a
// Dockerfile that Renovate will never move. Renovate's inline-pin manager
// reads a version only on the line right after a `# renovate:` comment, and
// only in the files its managerFilePatterns list; a pin missing either goes
// stale with no signal. That is how the sec sandbox image kept gitleaks
// 8.21.2 and trivy 0.70.0 while the security bots ran on it. A pin Renovate
// must not move is listed in unmanagedPins with its reason.
func TestEveryDockerfileVersionPinIsManaged(t *testing.T) {
	cfg := readRenovateConfig(t)
	patterns := inlinePinFilePatterns(t, cfg)
	listed := func(file string) bool {
		for _, re := range patterns {
			if re.MatchString(file) {
				return true
			}
		}
		return false
	}

	seen := map[string]bool{}
	var problems []string
	for _, p := range dockerfilePins(t, cfg) {
		key := p.file + ":" + p.name
		if _, ok := unmanagedPins[key]; ok {
			seen[key] = true
			continue
		}
		if goModAlignedPins[p.name] {
			continue
		}
		switch {
		case !p.annotated:
			problems = append(problems, fmt.Sprintf("%s:%d %s=%s: no `# renovate: datasource=… depName=…` comment on the line above, so Renovate never moves it — annotate it, or list it in unmanagedPins with the reason", p.file, p.line, p.name, p.value))
		case !listed(p.file):
			problems = append(problems, fmt.Sprintf("%s:%d %s=%s: annotated, but renovate.json's inline-pin managerFilePatterns do not list %s, so Renovate never reads the comment — add the file there", p.file, p.line, p.name, p.value, p.file))
		}
	}
	for key := range unmanagedPins {
		if !seen[key] {
			problems = append(problems, key+": listed in unmanagedPins but no such pin exists any more — drop the entry")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

var goModWailsRequire = regexp.MustCompile(`(?m)^\s*github\.com/wailsapp/wails/v2 (v\S+)`)

// TestDockerfileGoAndWailsPinsFollowGoMod holds every Go toolchain and Wails
// CLI a Dockerfile installs to the version go.mod asks for. Renovate moves
// go.mod, not these lines, so a bump that leaves one behind builds with the
// old toolchain and nothing turns red: the 2026-11-24 Go bump found three
// such pins by hand.
func TestDockerfileGoAndWailsPinsFollowGoMod(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	want := map[string]string{}
	if m := goModToolchainLine.FindSubmatch(gomod); m != nil {
		want["GO_VERSION"] = string(m[1])
	} else if m := goModGoLine.FindSubmatch(gomod); m != nil {
		want["GO_VERSION"] = string(m[1])
	} else {
		t.Fatal("go.mod has neither a `toolchain` nor a `go` line this guard can read")
	}
	if m := goModWailsRequire.FindSubmatch(gomod); m != nil {
		want["WAILS_VERSION"] = string(m[1])
	} else {
		t.Fatal("go.mod requires no github.com/wailsapp/wails/v2 — drop WAILS_VERSION from goModAlignedPins if the desktop build left Wails")
	}

	checked := map[string]int{}
	for _, p := range dockerfilePins(t, readRenovateConfig(t)) {
		w, ok := want[p.name]
		if !ok {
			continue
		}
		checked[p.name]++
		if strings.TrimPrefix(p.value, "v") != strings.TrimPrefix(w, "v") {
			t.Errorf("%s:%d %s=%s but go.mod asks for %s: move the pin with go.mod", p.file, p.line, p.name, p.value, w)
		}
	}
	for name := range want {
		if checked[name] == 0 {
			t.Errorf("no Dockerfile pins %s any more — drop it from goModAlignedPins", name)
		}
	}
}
