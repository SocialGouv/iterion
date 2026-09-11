package botscaffold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestShapesThatNeedJqShipAPinnedDevbox: a shape whose tool commands run
// jq declares it in the bundle's devbox.json, pinned by a committed
// devbox.lock, so the engine installs it on any image that ships devbox —
// the repo's rule for a bot that needs tools — instead of failing its gate
// at the first pass; a shape whose commands run no jq ships none, since
// the install has a cost. Which shapes need it is READ off their rendered
// tool commands, never listed by hand, so a third shape reaching for jq
// cannot ship without the pin. The config names exactly one jq, and the
// lock RESOLVES that same pin (a lock that merely mentioned it would
// re-resolve at every install, the thing the lock exists to prevent).
func TestShapesThatNeedJqShipAPinnedDevbox(t *testing.T) {
	jqRe := regexp.MustCompile(`(^|[^\w-])jq($|[^\w-])`)
	needs := 0
	for _, shape := range Shapes() {
		spec := templateForShape(t, shape).Spec
		spec.Slug = "jqbot"
		spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
		dir, w, _ := scaffoldAndCompile(t, spec)
		needsJq := false
		for _, n := range nodesOf[*ir.ToolNode](w) {
			if jqRe.MatchString(n.Command) {
				needsJq = true
			}
		}
		cfg, err := os.ReadFile(filepath.Join(dir, "devbox.json"))
		if !needsJq {
			if err == nil {
				t.Errorf("%s ships a devbox.json though no tool command runs jq", shape)
			}
			continue
		}
		needs++
		if err != nil {
			t.Fatalf("%s runs jq in a tool command but ships no devbox.json: %v", shape, err)
		}
		var parsed struct {
			Packages []string `json:"packages"`
		}
		if err := json.Unmarshal(cfg, &parsed); err != nil {
			t.Fatalf("%s: devbox.json is not JSON: %v", shape, err)
		}
		var jqPins []string
		for _, p := range parsed.Packages {
			if strings.HasPrefix(p, "jq@") {
				jqPins = append(jqPins, p)
			}
		}
		if len(jqPins) != 1 {
			t.Fatalf("%s: devbox.json pins jq %d times (%v), want exactly one pinned version", shape, len(jqPins), parsed.Packages)
		}
		lockBytes, err := os.ReadFile(filepath.Join(dir, "devbox.lock"))
		if err != nil {
			t.Fatalf("%s: the bundle ships no devbox.lock; the pin would re-resolve at every install: %v", shape, err)
		}
		var lock struct {
			Packages map[string]struct {
				Resolved string                     `json:"resolved"`
				Version  string                     `json:"version"`
				Systems  map[string]json.RawMessage `json:"systems"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(lockBytes, &lock); err != nil {
			t.Fatalf("%s: devbox.lock is not a lockfile: %v", shape, err)
		}
		entry, ok := lock.Packages[jqPins[0]]
		if !ok || entry.Resolved == "" || len(entry.Systems) == 0 {
			t.Fatalf("%s: devbox.lock does not RESOLVE %s (resolved=%q, systems=%d)", shape, jqPins[0], entry.Resolved, len(entry.Systems))
		}
		if want := strings.TrimPrefix(jqPins[0], "jq@"); entry.Version != want {
			t.Fatalf("%s: devbox.lock resolves jq %s, the config asks for %s", shape, entry.Version, want)
		}
	}
	if needs != 2 {
		t.Fatalf("%d shapes run jq, want the two that do (campaign-loop, scheduled-digest); update this count with the shape", needs)
	}
}
