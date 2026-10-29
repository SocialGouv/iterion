package backendcaps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The image-presence cell answers one question: does an image iterion
// publishes put this backend's CLI on PATH? It is derived here from the
// manifests that build those images, because a cell seeded from a prose table
// certifies the prose, not the image.
//
// Two families differ, and the difference is the whole point:
//
//   - the runner family (the app image and the cloud runner) puts the pinned
//     node_modules/.bin on PATH, so every pinned CLI is reachable there;
//   - the stock sandbox family (the devbox-derived bases, finalized) does NOT
//     extend PATH — only its explicit symlinks and the iterion binary are
//     reachable.
//
// A CLI reachable in one family and absent from the other is partial, never
// shipped: the sandbox is where the failures of #1643 were reported.
const (
	pinnedTreeDir  = "/opt/iterion/llm-clis/node_modules"
	pathBinDir     = "/usr/local/bin"
	cliPinManifest = "docker/llm-clis/package.json"

	// repoRoot is where the image manifests live, relative to this package.
	repoRoot = "../../.."
)

// imageChain is one published image: the manifests its build stacks, outermost
// last. A CLI is reachable in the image when any layer of the chain puts it on
// PATH.
type imageChain struct {
	name      string
	manifests []string
}

// imageFamily groups the images whose CLI reachability must agree. A split
// inside a family is a fact a single cell cannot express, so the derivation
// names it instead of picking one side.
type imageFamily struct {
	name   string
	chains []imageChain
}

var imageFamilies = []imageFamily{
	{name: "runner", chains: []imageChain{
		{name: "app image", manifests: []string{"Dockerfile"}},
		{name: "cloud runner", manifests: []string{"Dockerfile.runner-devbox"}},
	}},
	// The bases are read with finalize on purpose: a base that starts putting
	// the pinned bin dir on PATH moves the cell instead of being invisible.
	{name: "sandbox", chains: []imageChain{
		{name: "stock sandbox", manifests: []string{
			"sandbox/slim/Dockerfile",
			"sandbox/full/Dockerfile",
			"sandbox/sec/Dockerfile",
			"sandbox/browser/Dockerfile",
			"sandbox/finalize/Dockerfile",
		}},
	}},
}

// cliDelivery is what a backend needs an image to provide: the bare executable
// its adapter hands to os/exec — resolveCLIBinary's DefaultBinary, documented
// in pkg/backend/delegate/cliagent.go as "what the published images ship on
// PATH" — and how that executable gets there.
//
// npmPackage empty means no npm package is pinned for the backend: kimi and
// grok have none upstream, and opencode's two live distribution lines
// (opencode-ai 1.19, @opencode/cli 2.x) are the bake choice #1643 reserves.
// firstParty means the executable is iterion's own binary.
//
// declaredIn/declaredAs locate the adapter's own declaration of that
// executable, so this table cannot quietly name a binary no adapter invokes:
// the guard rebuilds `<declaredAs>: "<binary>"` and demands it in the source.
type cliDelivery struct {
	binary     string
	npmPackage string
	firstParty bool
	declaredIn string
	declaredAs string
}

var cliDeliveries = map[string]cliDelivery{
	ClaudeCode: {binary: "claude", npmPackage: "@anthropic-ai/claude-code",
		declaredIn: "pkg/backend/detect/detect.go", declaredAs: "Name"},
	// claw is iterion driving itself (`iterion __claw-runner`): there is no
	// adapter-side binary name to cross-check, and the COPY line the
	// derivation reads is the whole requirement.
	Claw: {binary: "iterion", firstParty: true},
	Codex: {binary: "codex", npmPackage: "@openai/codex",
		declaredIn: "pkg/backend/detect/detect.go", declaredAs: "Name"},
	Pi: {binary: "pi", npmPackage: "@earendil-works/pi-coding-agent",
		declaredIn: "pkg/backend/delegate/pi.go", declaredAs: "DefaultBinary"},
	Grok: {binary: "grok",
		declaredIn: "pkg/backend/delegate/grok.go", declaredAs: "DefaultBinary"},
	Kimi: {binary: "kimi",
		declaredIn: "pkg/backend/delegate/kimi.go", declaredAs: "DefaultBinary"},
	OpenCode: {binary: "opencode",
		declaredIn: "pkg/backend/delegate/opencode.go", declaredAs: "DefaultBinary"},
}

var (
	envPathRe    = regexp.MustCompile(`(?m)^\s*(?:ENV\s+)?PATH="([^"]*)"`)
	symlinkRe    = regexp.MustCompile(`ln -s[a-z]*\s+(\S+)\s+` + regexp.QuoteMeta(pathBinDir) + `/(\S+)`)
	copyOnPathRe = regexp.MustCompile(`(?m)^COPY\s+(?:--\S+\s+)*\S+\s+` + regexp.QuoteMeta(pathBinDir) + `/(\S+)\s*$`)
	copyPinnedRe = regexp.MustCompile(`(?m)^COPY\s+(?:--\S+\s+)*\S+\s+` + regexp.QuoteMeta(pinnedTreeDir) + `/?\s*$`)
)

// imageLayer is what one Dockerfile contributes to CLI reachability.
type imageLayer struct {
	pathHasPinnedBin bool            // PATH carries the pinned node_modules/.bin
	pinnedTree       bool            // the pinned node_modules tree is copied in
	onPath           map[string]bool // bare names landed in /usr/local/bin
}

func readImageLayer(t *testing.T, root, manifest string) imageLayer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, manifest))
	if err != nil {
		t.Fatalf("image manifest unreadable, the derivation certifies nothing: %v", err)
	}
	layer := imageLayer{onPath: map[string]bool{}}
	text := string(data)
	for _, m := range envPathRe.FindAllStringSubmatch(text, -1) {
		for _, entry := range strings.Split(m[1], ":") {
			if entry == pinnedTreeDir+"/.bin" {
				layer.pathHasPinnedBin = true
			}
		}
	}
	layer.pinnedTree = copyPinnedRe.MatchString(text)
	for _, m := range symlinkRe.FindAllStringSubmatch(text, -1) {
		// The source must be a binary of the pinned tree: a symlink from
		// anywhere else is some other tool, not a pinned CLI.
		if strings.HasPrefix(m[1], pinnedTreeDir+"/.bin/") {
			layer.onPath[m[2]] = true
		}
	}
	for _, m := range copyOnPathRe.FindAllStringSubmatch(text, -1) {
		layer.onPath[m[1]] = true
	}
	return layer
}

// reachableIn reports, per backend, whether the chain puts its CLI on PATH.
func reachableIn(t *testing.T, root string, chain imageChain, pinned map[string]bool, deliveries map[string]cliDelivery) map[string]bool {
	t.Helper()
	merged := imageLayer{onPath: map[string]bool{}}
	for _, manifest := range chain.manifests {
		layer := readImageLayer(t, root, manifest)
		merged.pathHasPinnedBin = merged.pathHasPinnedBin || layer.pathHasPinnedBin
		merged.pinnedTree = merged.pinnedTree || layer.pinnedTree
		for name := range layer.onPath {
			merged.onPath[name] = true
		}
	}
	// Positive controls: a parse that finds nothing must never read as "the
	// image ships nothing". If either of these is genuinely false, the cells
	// change AND this control changes with them — deliberately, not silently.
	if !merged.pinnedTree {
		t.Fatalf("%s: no layer copies %s — either the image stopped carrying the pinned CLIs, or this parse is broken",
			chain.name, pinnedTreeDir)
	}
	if !merged.pathHasPinnedBin && len(merged.onPath) == 0 {
		t.Fatalf("%s: no layer puts anything on %s and PATH carries no pinned bin dir — this parse is broken",
			chain.name, pathBinDir)
	}
	out := map[string]bool{}
	for backend, cli := range deliveries {
		if cli.firstParty {
			// iterion's own binary: present when the chain copies it onto PATH.
			out[backend] = merged.onPath[cli.binary]
			continue
		}
		if cli.npmPackage == "" || !pinned[cli.npmPackage] || !merged.pinnedTree {
			// Nothing pinned, or no pinned tree in the image, means a symlink
			// would dangle: the CLI is not reachable, whatever PATH says.
			out[backend] = false
			continue
		}
		out[backend] = merged.onPath[cli.binary] || merged.pathHasPinnedBin
	}
	return out
}

func readPinnedCLIs(t *testing.T, root string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, cliPinManifest))
	if err != nil {
		t.Fatalf("CLI pin manifest unreadable, the derivation certifies nothing: %v", err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("CLI pin manifest unparseable: %v", err)
	}
	if len(manifest.Dependencies) == 0 {
		t.Fatalf("%s pins no CLI at all: either the image ships none, or this derivation reads the wrong shape", cliPinManifest)
	}
	pinned := map[string]bool{}
	for pkg := range manifest.Dependencies {
		pinned[pkg] = true
	}
	return pinned
}

// deriveImagePresence returns the verdict each image-presence cell must carry,
// measured from the manifests under root. deliveries is a parameter, not the
// package variable, so a test can measure a hypothetical bake — pinning a CLI
// also means naming the package that delivers it.
func deriveImagePresence(t *testing.T, root string, deliveries map[string]cliDelivery) map[string]string {
	t.Helper()
	pinned := readPinnedCLIs(t, root)
	byFamily := map[string]map[string]bool{}
	for _, family := range imageFamilies {
		for _, chain := range family.chains {
			got := reachableIn(t, root, chain, pinned, deliveries)
			first, seen := byFamily[family.name]
			if !seen {
				byFamily[family.name] = got
				continue
			}
			for backend, reach := range got {
				if first[backend] != reach {
					t.Fatalf("%s family: image %q disagrees with its siblings on %s — one cell cannot say both; split the family or the cell",
						family.name, chain.name, backend)
				}
			}
		}
	}
	verdicts := map[string]string{}
	for backend := range deliveries {
		runner, sandbox := byFamily["runner"][backend], byFamily["sandbox"][backend]
		switch {
		case runner && sandbox:
			verdicts[backend] = Shipped
		case runner || sandbox:
			verdicts[backend] = Partial
		default:
			verdicts[backend] = UnwiredNoImage
		}
	}
	return verdicts
}

func imagePresenceVerdicts() map[string]string {
	got := map[string]string{}
	for _, b := range Snapshot().Backends {
		for _, c := range b.Cells {
			if c.Name == ImagePresence {
				got[b.Name] = c.Verdict
			}
		}
	}
	return got
}

// TestImagePresenceIsDerivedFromTheImageManifests keeps the cell honest: it
// compares every committed image-presence verdict with what the manifests that
// BUILD the images say, so the registry cannot certify a claim no image backs.
func TestImagePresenceIsDerivedFromTheImageManifests(t *testing.T) {
	if got, want := len(cliDeliveries), len(Names()); got != want {
		t.Fatalf("%d backends declare a CLI delivery, the registry has %d: a new backend needs its image requirement here", got, want)
	}
	for _, name := range Names() {
		cli, ok := cliDeliveries[name]
		if !ok {
			t.Fatalf("backend %s declares no CLI delivery: name the executable an image must ship", name)
		}
		if cli.declaredAs == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, cli.declaredIn))
		if err != nil {
			t.Fatal(err)
		}
		declared := regexp.MustCompile(regexp.QuoteMeta(cli.declaredAs) + `:\s*"` + regexp.QuoteMeta(cli.binary) + `"`)
		if !declared.Match(data) {
			t.Errorf("%s: %s no longer declares %s %q — this table names a binary the adapter does not invoke",
				name, cli.declaredIn, cli.declaredAs, cli.binary)
		}
	}
	// A pin nobody claims is a renamed package about to flip a verdict
	// silently; name it here rather than reading the move as an image change.
	claimed := map[string]bool{}
	for _, cli := range cliDeliveries {
		if cli.npmPackage != "" {
			claimed[cli.npmPackage] = true
		}
	}
	for pkg := range readPinnedCLIs(t, repoRoot) {
		if !claimed[pkg] {
			t.Errorf("%s pins %q, which no backend claims: map it to its backend or drop the pin", cliPinManifest, pkg)
		}
	}

	want := deriveImagePresence(t, repoRoot, cliDeliveries)
	got := imagePresenceVerdicts()
	for _, name := range Names() {
		if got[name] != want[name] {
			t.Errorf("%s image-presence says %q, the image manifests say %q", name, got[name], want[name])
		}
	}
}

// copyManifests mirrors the manifests the derivation reads into a scratch tree
// we own, so a mutation proves the derivation reads them.
func copyManifests(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{cliPinManifest}
	for _, family := range imageFamilies {
		for _, chain := range family.chains {
			files = append(files, chain.manifests...)
		}
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func editManifest(t *testing.T, root, rel string, edit func(string) (string, error)) {
	t.Helper()
	path := filepath.Join(root, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := edit(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if next == string(data) {
		t.Fatalf("%s: the edit changed nothing — the mutation proves nothing", rel)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withDelivery copies the delivery table with one entry replaced; the package
// variable stays untouched so cases cannot leak into each other.
func withDelivery(base map[string]cliDelivery, backend string, cli cliDelivery) map[string]cliDelivery {
	out := make(map[string]cliDelivery, len(base))
	for name, entry := range base {
		out[name] = entry
	}
	out[backend] = cli
	return out
}

func dropLine(containing string) func(string) (string, error) {
	return func(text string) (string, error) {
		lines := strings.Split(text, "\n")
		kept := make([]string, 0, len(lines))
		dropped := 0
		for _, line := range lines {
			if strings.Contains(line, containing) {
				dropped++
				continue
			}
			kept = append(kept, line)
		}
		if dropped != 1 {
			return "", fmt.Errorf("dropped %d lines containing %q, want exactly 1", dropped, containing)
		}
		return strings.Join(kept, "\n"), nil
	}
}

// TestImagePresenceMovesWithTheManifests drives the derivation in both
// directions on a copy we own: taking a CLI out of an image must move its
// verdict down, and putting one in must move it up. A verdict that cannot move
// is a constant dressed as a measurement.
func TestImagePresenceMovesWithTheManifests(t *testing.T) {
	base := deriveImagePresence(t, copyManifests(t), cliDeliveries)
	if !slices.Contains([]string{Shipped, Partial}, base[ClaudeCode]) {
		t.Fatalf("claude_code reads %q on an unmutated copy: the baseline itself is wrong", base[ClaudeCode])
	}
	// deliveries, when set, replaces the package mapping: a bake lands the pin
	// AND the mapping, so measuring one without the other proves nothing.
	for _, tc := range []struct {
		name       string
		mutate     func(t *testing.T, root string)
		deliveries map[string]cliDelivery
		backend    string
		want       string
	}{
		{
			name: "the sandbox stops symlinking pi",
			mutate: func(t *testing.T, root string) {
				editManifest(t, root, "sandbox/finalize/Dockerfile", dropLine(pinnedTreeDir+"/.bin/pi"))
			},
			backend: Pi,
			want:    Partial, // still on the runner's PATH, gone from the sandbox
		},
		{
			name: "the pin manifest drops codex",
			mutate: func(t *testing.T, root string) {
				editManifest(t, root, cliPinManifest, func(text string) (string, error) {
					const pin = ",\n    \"@openai/codex\": \"0.156.1\""
					next := strings.Replace(text, pin, "", 1)
					if next == text {
						return "", fmt.Errorf("the codex pin is not where this mutation looks for it")
					}
					return next, nil
				})
			},
			backend: Codex,
			want:    UnwiredNoImage, // the symlinks stay, and they dangle
		},
		{
			name: "the sandbox stops baking the iterion binary",
			mutate: func(t *testing.T, root string) {
				editManifest(t, root, "sandbox/finalize/Dockerfile",
					dropLine(pathBinDir+"/iterion "+pathBinDir+"/iterion"))
			},
			backend: Claw,
			want:    Partial, // the runner still carries it
		},
		{
			name: "the sandbox starts symlinking codex",
			mutate: func(t *testing.T, root string) {
				editManifest(t, root, "sandbox/finalize/Dockerfile", func(text string) (string, error) {
					return strings.Replace(text,
						" && ln -sf "+pinnedTreeDir+"/.bin/pi "+pathBinDir+"/pi",
						" && ln -sf "+pinnedTreeDir+"/.bin/pi "+pathBinDir+"/pi \\\n && ln -sf "+pinnedTreeDir+"/.bin/codex "+pathBinDir+"/codex", 1), nil
				})
			},
			backend: Codex,
			want:    Shipped, // the upward direction: closing the sandbox gap
		},
		{
			// The absent three are absent because no package delivers them,
			// not because their names are on a deny-list: pin one and map it,
			// and the derivation admits it like any other.
			name: "both families bake opencode",
			mutate: func(t *testing.T, root string) {
				editManifest(t, root, cliPinManifest, func(text string) (string, error) {
					return strings.Replace(text,
						"\"@openai/codex\": \"0.156.1\"",
						"\"@openai/codex\": \"0.156.1\",\n    \"opencode-ai\": \"1.19.0\"", 1), nil
				})
				editManifest(t, root, "sandbox/finalize/Dockerfile", func(text string) (string, error) {
					return strings.Replace(text,
						"RUN ln -sf "+pinnedTreeDir+"/.bin/claude",
						"RUN ln -sf "+pinnedTreeDir+"/.bin/opencode "+pathBinDir+"/opencode \\\n && ln -sf "+pinnedTreeDir+"/.bin/claude", 1), nil
				})
			},
			deliveries: withDelivery(cliDeliveries, OpenCode,
				cliDelivery{binary: "opencode", npmPackage: "opencode-ai"}),
			backend: OpenCode,
			want:    Shipped, // pinned: on the runner's PATH, symlinked in the sandbox
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if base[tc.backend] == tc.want {
				t.Fatalf("%s already reads %q before the mutation: this direction proves nothing", tc.backend, tc.want)
			}
			deliveries := tc.deliveries
			if deliveries == nil {
				deliveries = cliDeliveries
			}
			root := copyManifests(t)
			tc.mutate(t, root)
			got := deriveImagePresence(t, root, deliveries)
			if got[tc.backend] != tc.want {
				t.Errorf("%s moved to %q, want %q", tc.backend, got[tc.backend], tc.want)
			}
			for _, name := range Names() {
				if name != tc.backend && got[name] != base[name] {
					t.Errorf("%s moved to %q too (was %q) — the mutation is not scoped",
						name, got[name], base[name])
				}
			}
		})
	}
}
