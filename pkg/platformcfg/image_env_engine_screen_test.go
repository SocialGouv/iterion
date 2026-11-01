package platformcfg

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// The image ENV screen's answer for the engine's OWN namespace, held to the
// engine's own inventory (#2859).
//
// The screen (pkg/sandbox/devcontainer's ScreenImageEnvName) classifies a
// name an IMAGE bakes into its `ENV`: the credential/routing sub-class is
// overridden with an empty value in the container, everything else is
// reported and keeps the image's value (ARB-121). `ITERION_` is the one
// MIXED namespace of the class — iterion's own published images bake three
// of its names on purpose, and a handful of the others choose which
// credential a run uses or which host it is presented at — so the namespace
// cannot answer wholesale, and the partition has to be a measurement over
// the names the engine actually spells rather than one reviewer's list.
//
// The inventory is the one engineEnvNames already scans for the bot-var
// gate: the string literals of the engine's Go source and of the vendored
// claw it links, plus the manifests and scripts it embeds under pkg. One
// scan, two gates — a second walk of the same tree would drift from this
// one the day a read moves.

// TestEveryEngineEnvNameTheImageScreenSeesIsClassified is the sweep the
// partition owes: every `ITERION_` name the engine spells is in the deny
// class, and the side it lands on agrees with the SHARED credential-routing
// shape (internal/envtrust, the one the dotenv plant gate asks) unless a
// row of the screen says, with its reason, why this name is the shape's
// over-reach or the shape's blind spot.
//
// So a new engine name cannot land unclassified: it is neutralised the day
// it reads as a credential or a route, reported otherwise, and a
// disagreement between the shape and the screen arrives here red until
// somebody writes the reason on the row.
func TestEveryEngineEnvNameTheImageScreenSeesIsClassified(t *testing.T) {
	seen := engineEnvNames(t)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	neutralised := 0
	for _, name := range names {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			t.Errorf("%s (%s) is outside the image deny class: the `ITERION_` namespace must at least TELL the operator about a name its own runner reads", name, seen[name][0])
			continue
		}
		if neutralize {
			neutralised++
		}
		shaped := envtrust.RoutingShapedName(name) || envtrust.CredentialShapedName(name)
		if shaped == neutralize {
			continue
		}
		if reason := strings.TrimSpace(devcontainer.EngineImageEnvReason(name)); reason != "" {
			continue
		}
		if shaped {
			t.Errorf("%s (%s) reads as a credential or a route to the shared shape class and the image screen only REPORTS it: an image that bakes it still contributes it to every process iterion starts in the container — neutralise it, or give it a row saying why the name chooses neither a credential nor a route (pkg/sandbox/devcontainer's engine rows)", name, seen[name][0])
			continue
		}
		t.Errorf("%s (%s) is NEUTRALISED by the image screen and the shared shape class reads neither a credential nor a route in it: say why on its row, or let the shape answer (pkg/sandbox/devcontainer's engine rows)", name, seen[name][0])
	}
	// Anti-vacuity: the sweep is only worth its name while the screen still
	// empties a real part of this namespace. Measured: 94 names neutralised
	// of the 416 the scan reaches, 322 reported — the endpoints, the stores'
	// credentials, the OAuth wires, the private-network lifts, the
	// credential switches and the guard lifts the name shape cannot spell.
	if neutralised < 40 {
		t.Errorf("the image screen neutralises %d of the %d engine names: the credential/routing sub-class of this namespace has stopped being emptied", neutralised, len(names))
	}
}

// TestTheEngineNamesOurOwnImagesBakeStayReported is the measurement that
// keeps the namespace off the neutralised side wholesale: iterion's own
// published images bake `ITERION_VERSION`, `ITERION_COMMIT` and
// `ITERION_BOTS_PATH`, so an `--env NAME=` for them would empty three
// variables our own artefacts set on purpose.
//
// Read from the Dockerfiles rather than from a list of three names: the
// next ENV line our own images gain is judged the day it is written, and a
// name this scan finds must be REPORTED (told, and kept).
func TestTheEngineNamesOurOwnImagesBakeStayReported(t *testing.T) {
	baked := engineEnvNamesOurImagesBake(t)
	for _, name := range baked {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			t.Errorf("%s is baked by an image iterion publishes and the deny class does not even see it: the operator reads nothing about it", name)
			continue
		}
		if neutralize {
			t.Errorf("%s is baked by an image iterion publishes and the screen EMPTIES it: our own runner would start with the variable our own Dockerfile sets, blanked", name)
		}
	}
}

// engineEnvNamesOurImagesBake are the `ITERION_` names the Dockerfiles of
// this repository put in an image's `ENV`, sorted. Continuation lines are
// joined first, so an `ENV A=1 \` + `    B=2` pair is read as the two
// assignments it is.
//
// The inventory of Dockerfiles is git's, not a pair of globs: every TRACKED
// path whose base name spells `dockerfile`, minus `vendor/` (whose images
// are a dependency's, not ours). An `ENV` line added under `ci/` or
// `scripts/` is then judged the day it is written, where a shape-matching
// glob answers for the two directories it happens to name.
//
// The three names the class's doc stands on are the anchors: finding fewer
// means the scan has stopped reading the Dockerfiles, which would make the
// guard above pass by reading nothing.
func engineEnvNamesOurImagesBake(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []string
	for _, line := range strings.Split(gittest.Run(t, root, "ls-files"), "\n") {
		tracked := strings.TrimSpace(line)
		if tracked == "" || strings.HasPrefix(tracked, "vendor/") {
			continue
		}
		if !strings.Contains(strings.ToLower(filepath.Base(tracked)), "dockerfile") {
			continue
		}
		files = append(files, filepath.Join(root, tracked))
	}
	if len(files) < 8 {
		t.Fatalf("git ls-files found %d Dockerfiles (%v): the repository ships the root pair and six sandbox images at least, so the scan is reading a tree it does not own", len(files), files)
	}
	envLine := regexp.MustCompile(`(?m)^[ \t]*ENV[ \t]+(.*)$`)
	assignment := regexp.MustCompile(`\b(ITERION_[A-Z0-9_]+)=`)
	var names []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		joined := strings.ReplaceAll(string(raw), "\\\n", " ")
		for _, line := range envLine.FindAllStringSubmatch(joined, -1) {
			for _, m := range assignment.FindAllStringSubmatch(line[1], -1) {
				if !slices.Contains(names, m[1]) {
					names = append(names, m[1])
				}
			}
		}
	}
	sort.Strings(names)
	for _, anchor := range []string{"ITERION_VERSION", "ITERION_COMMIT", "ITERION_BOTS_PATH"} {
		if !slices.Contains(names, anchor) {
			t.Fatalf("the Dockerfile scan did not find %s (found %v): it is not reading the ENV lines of the images iterion publishes", anchor, names)
		}
	}
	return names
}
