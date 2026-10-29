package bundle

import (
	"sort"
	"strings"
	"testing"

	yamlv2 "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// What the key reader answers is the DECLARATION, not the decoded value: a
// key with an empty value counts, a root merge resolves to the keys it
// brings in, a nested mapping is reached by its dotted path, and a sequence
// is a value whose elements are not key paths.
func TestDeclaredManifestKeysReadsDeclarationsNotValues(t *testing.T) {
	body := "" +
		"name: probe\n" +
		"category:\n" + // declared, empty
		"triggers: [refactor]\n" + // a sequence is a value
		"requires:\n  iterion: \">= " + CatalogTaxonomySince + "\"\n" +
		"exports:\n  workflows:\n    - id: shared\n      path: shared.bot\n" +
		"compat: &bag\n  nested:\n    deeper: 1\n" +
		"<<: *bag\n"
	keys, err := DeclaredManifestKeys([]byte(body))
	if err != nil {
		t.Fatalf("reading the keys: %v", err)
	}
	// `nested` arrives at the ROOT through the merge, so its own key is one
	// level further down and still read; under `compat` the same mapping is
	// one level deeper and stops at maxManifestKeyDepth.
	for _, want := range []string{"name", "category", "triggers", "requires", "requires.iterion", "exports", "exports.workflows", "compat", "compat.nested", "nested", "nested.deeper"} {
		if !keys[want] {
			t.Errorf("key %q is not read as declared: %s", want, declaredKeyList(keys))
		}
	}
	for _, unwanted := range []string{"<<", "tags", "exports.skills", "exports.workflows.id", "compat.nested.deeper"} {
		if keys[unwanted] {
			t.Errorf("key %q is read as declared, and is not: %s", unwanted, declaredKeyList(keys))
		}
	}
	if !keys.Declares("tags", "category") || keys.Declares("tags") {
		t.Errorf("Declares does not answer on the keys it was given: %s", declaredKeyList(keys))
	}
}

// A floor asked for by a manifest KEY travels on the manifest itself: the
// decoder records what the document declares, and the one predicate every
// floor surface reads (CheckSyntaxFloor) refuses a floor below the key's
// release and admits the release itself. A Manifest built in code declares
// no key, so it owes no floor for one it does not spell.
func TestTheFloorTableReadsTheManifestAndNotOnlyTheSources(t *testing.T) {
	declare := func(t *testing.T, floor string) ProfileFloor {
		t.Helper()
		body := "name: probe\ncategory: verify\n"
		if floor != "" {
			body += "requires:\n  iterion: \">= " + floor + "\"\n"
		}
		m, err := DecodeManifest([]byte(body), "probe")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !m.DeclaredKeys.Declares(keyCategory) {
			t.Fatalf("the decoder did not record the declared key: %s", declaredKeyList(m.DeclaredKeys))
		}
		return CheckSyntaxFloor(m, SyntaxRequirements{})
	}
	below := floortest.Below(t, CatalogTaxonomySince)
	for name, tc := range map[string]struct {
		floor  string
		wantOK bool
	}{
		"no floor at all":    {"", false},
		"a floor below":      {below, false},
		"the release itself": {CatalogTaxonomySince, true},
		"a release above":    {"9.999.0", true},
	} {
		t.Run(name, func(t *testing.T) {
			pf := declare(t, tc.floor)
			if pf.OK != tc.wantOK {
				t.Fatalf("floor OK = %v, want %v: %+v", pf.OK, tc.wantOK, pf)
			}
			if tc.wantOK {
				return
			}
			if pf.Need != CatalogTaxonomySince || !strings.Contains(pf.Uses, "`category`") {
				t.Fatalf("the verdict does not name the key and its release: %+v", pf)
			}
		})
	}
	// The same bundle described in code, with no document behind it: the
	// sources ask for nothing and neither does a key nobody spelled.
	if pf := CheckSyntaxFloor(&Manifest{Name: "probe", Category: "verify"}, SyntaxRequirements{}); !pf.OK {
		t.Fatalf("a Manifest built in code was asked for a manifest-key floor: %+v", pf)
	}
	// Both halves of the subject at once: the sources' release and the
	// key's, with the floor the HIGHEST of the two and both named.
	subject := FloorSubject{
		Syntax: SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}},
		Keys:   ManifestKeys{keyTags: true},
	}
	need, reason := RequiredReleaseOf(subject)
	highest := CatalogTaxonomySince
	if cmp, ok := CompareVersions(parser.ImportSince, highest); ok && cmp > 0 {
		highest = parser.ImportSince
	}
	if need != highest {
		t.Fatalf("a subject using `import` and declaring `tags` asks for %q (%s), want the highest of the two, %q", need, reason, highest)
	}
	if uses := subject.Describe(); !strings.Contains(uses, "`import`") || !strings.Contains(uses, "`tags`") {
		t.Fatalf("the subject does not describe both halves: %q", uses)
	}
}

// The key reader is a SECOND parser over the same bytes (yaml.v3, where the
// manifest contract is yaml.v2's strict decode), so it must not refuse a
// document the contract accepts: a manifest that loads today would stop
// loading, at the one door every load goes through. The shapes below are
// the ones the two readers could plausibly disagree on — a vendor bump that
// made them disagree reddens here.
func TestTheKeyReaderRefusesNothingTheStrictDecodeAccepts(t *testing.T) {
	for name, body := range map[string]string{
		"empty":            "",
		"comments only":    "# nothing\n",
		"multi document":   "name: a\n---\nname: b\n",
		"anchor and alias": "compat: &a\n  x: 1\nattachments:\n  y: z\n",
		"root merge":       "compat: &a\n  category: verify\n<<: *a\n",
		"yes as a value":   "enabled: yes\n",
		"tab in a value":   "description: \"a\tb\"\n",
		"crlf":             "name: a\r\nversion: 1\r\n",
		"byte order mark":  "\ufeffname: a\n",
		"quoted key":       "\"name\": a\n",
		"null document":    "null\n",
		"empty mapping":    "{}\n",
		"folded scalar":    "description: >\n  one\n  two\n",
	} {
		t.Run(name, func(t *testing.T) {
			var strict Manifest
			if err := yamlv2.UnmarshalStrict([]byte(body), &strict); err != nil {
				t.Skipf("the manifest contract refuses this shape first: %v", err)
			}
			if _, err := DeclaredManifestKeys([]byte(body)); err != nil {
				t.Fatalf("the strict decode accepts this manifest and the key reader refuses it: %v", err)
			}
		})
	}
}

// declaredKeyList renders a declared-key set for a failure message.
func declaredKeyList(keys ManifestKeys) string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
