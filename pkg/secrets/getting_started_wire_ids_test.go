package secrets

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The cloud walkthrough names the provider wire ids a member may pass to
// `iterion remote api-keys create --provider`. Nothing else ties that prose
// to the vocabulary ParseProvider accepts: a provider rename that misses the
// page leaves a reader, or an agent following it, typing a dead id, and a
// provider added without the page leaves the list short.
func TestGettingStartedWireIdsAreValidProviders(t *testing.T) {
	const page = "../../docs/getting-started.md"
	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	// One list, whatever its case or its separator: a second one would be read by no one,
	// dead ids included.
	labels := regexp.MustCompile(`(?i)wire[\s-]+ids:`).FindAllStringIndex(string(raw), -1)
	if len(labels) != 1 {
		t.Fatalf("%s carries %d `wire ids:` lists, want exactly one — update this test together with the page", page, len(labels))
	}
	list, _, ok := strings.Cut(string(raw)[labels[0][1]:], ")")
	if !ok {
		t.Fatalf("%s: the `wire ids:` list is not closed by a parenthesis", page)
	}
	// Every item is exactly one backticked id: a gloss, a bare word or a
	// second id glued to the first would otherwise pass unread.
	item := regexp.MustCompile("^`([^`]+)`$")
	var ids []string
	for _, raw := range strings.Split(list, ",") {
		m := item.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			t.Errorf("%s: wire-id item %q is not one backticked id", page, strings.TrimSpace(raw))
			continue
		}
		if _, err := ParseProvider(m[1]); err != nil {
			t.Errorf("%s names provider id %q, which ParseProvider refuses: %v", page, m[1], err)
		}
		ids = append(ids, m[1])
	}
	// openai_compatible is left out of the page's list on purpose: it needs
	// --base-url, which the page's credential row does not cover.
	want := slices.DeleteFunc(ProviderIDs(), func(id string) bool { return id == string(ProviderOpenAICompatible) })
	slices.Sort(want)
	slices.Sort(ids)
	if !slices.Equal(ids, want) {
		t.Errorf("%s lists wire ids %v, want every vendor provider %v", page, ids, want)
	}
}
