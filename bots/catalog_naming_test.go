package bots

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The naming doctrine of the catalog — docs/agents/bots/naming.md. A persona
// carries a colour, or ends like iterion; the lists below are its named
// exceptions, and each one only shrinks.

// personaColours is the closed palette lexicon. Adding a colour is a change
// to the doctrine page and to this list together.
var personaColours = map[string]bool{
	"red": true, "orange": true, "yellow": true, "green": true, "blue": true,
	"purple": true, "violet": true, "pink": true, "black": true, "white": true,
	"grey": true, "gray": true, "brown": true, "gold": true, "golden": true,
	"silver": true,
}

// grandfatheredPersonas predate the rule, keyed by slug. Renaming one of
// these bots removes its entry: the new name answers to the rule.
var grandfatheredPersonas = map[string]string{
	"adr-cartograph":      "Adry",
	"app-dev":             "Appy",
	"assessment":          "Assessy",
	"bmady":               "Bmady",
	"branch-improve-loop": "Billy",
	"campaign":            "Campy",
	"copilot":             "Copi",
	"dep-update-guard":    "Vetty",
	"docs-refresh":        "Doki",
	"e2e-coverage":        "Endy",
	"feature-dev":         "Featurly",
	"feature-gap-fill":    "Fini",
	"feed-watch":          "Vigie",
	"gitops-warden":       "Warden",
	"golden-master":       "Goldy",
	"instrument":          "Obsy",
	"modernize":           "Morphy",
	"prod-watch":          "Argus",
	"product-docs":        "Prody",
	"review-pr":           "Revi",
	"revi-converse":       "Revi (converse)",
	"rgaa-audit":          "Acci",
	"sec-audit-deps":      "Depsy",
	"sec-audit-source":    "Seki",
	"secured-renovacy":    "Renovacy",
	"supply-shield":       "Shieldy",
	"test-coverage":       "Testy",
	"vuln-watch":          "Senti",
	"whats-next":          "Nexie",
	"whole-improve-loop":  "Willy",
	"wiki-gen":            "Wikky",
	"workflow-kick":       "Kicky",
}

// personaJokers carry no mark and keep their place on merit, keyed by slug.
var personaJokers = map[string]struct{ persona, why string }{
	"devbox-setup": {"Devboxing", "says devbox outright and puns on boxing; DevBoxer is an AI agent platform already"},
}

// grandfatheredNearPairs are the persona pairs within edit distance 2 that
// predate the rule, lower-cased and in lexical order.
var grandfatheredNearPairs = [][2]string{
	{"adry", "appy"},
	{"appy", "campy"},
	{"billy", "willy"},
	{"copi", "doki"},
	{"doki", "seki"},
	{"kicky", "wikky"},
	{"revi", "seki"},
	{"seki", "senti"},
	{"testy", "vetty"},
	{"wikky", "willy"},
}

// The grandfathered lists only shrink. Removing an entry lowers its count
// here; raising a count is a doctrine change, argued in review.
const (
	grandfatheredPersonaCount  = 32
	grandfatheredNearPairCount = 10
)

const (
	personaMaxWords      = 2
	personaMaxChars      = 16
	personaMaxLastLetter = 10
	personaNearDistance  = 2
)

type catalogPersona struct {
	slug     string
	persona  string
	commands []string // every command name and alias the bot answers to
}

// catalogPersonas loads the persona of every catalog bot under bots/ —
// examples/ is the operators' playground and stays free.
func catalogPersonas(t *testing.T) []catalogPersona {
	t.Helper()
	manifests := catalogRootFiles(t, bundleManifestRoot)
	if !slices.Contains(manifests, filepath.Join("review-pr", bundle.ManifestFile)) {
		t.Fatalf("the walk did not reach review-pr/%s; it found %v", bundle.ManifestFile, manifests)
	}
	// manifest.yml is the accepted second spelling: a bundle written that way
	// is a catalog bot too.
	alt, err := filepath.Glob(filepath.Join("*", bundle.ManifestFileAlt))
	if err != nil {
		t.Fatalf("glob */%s: %v", bundle.ManifestFileAlt, err)
	}
	manifests = append(manifests, alt...)
	var out []catalogPersona
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Fatalf("%s: load manifest: %v", path, err)
		}
		if m == nil {
			t.Fatalf("%s: no manifest loaded", path)
		}
		p := catalogPersona{slug: filepath.Base(filepath.Dir(path)), persona: m.DisplayName}
		for _, inv := range m.Invocations {
			if inv.Command != nil {
				p.commands = append(p.commands, inv.Command.Name)
				p.commands = append(p.commands, inv.Command.Aliases...)
			}
		}
		out = append(out, p)
	}
	return out
}

// underRule reports whether the persona answers to the full rule: neither
// grandfathered nor a joker.
func (p catalogPersona) underRule() bool {
	if _, ok := grandfatheredPersonas[p.slug]; ok {
		return false
	}
	_, joker := personaJokers[p.slug]
	return !joker
}

func (p catalogPersona) grandfathered() bool {
	_, ok := grandfatheredPersonas[p.slug]
	return ok
}

func personaWords(persona string) []string {
	return strings.Fields(strings.ToLower(persona))
}

// The alphabet of a persona: ASCII letters and digits, the first word
// starting with a letter. It keeps every word comparable with the others and
// the kebab form a valid command name (pkg/bundle commandNameRe is ASCII and
// starts with a letter).
var (
	personaFirstWordRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	personaWordRe      = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// personaShapeProblem says why a persona is spelled outside the doctrine's
// alphabet, or returns "" when it is inside.
func personaShapeProblem(persona string) string {
	words := strings.Fields(persona)
	if persona != strings.Join(words, " ") {
		return "separates its words with something other than one space"
	}
	for i, w := range words {
		if i == 0 && !personaFirstWordRe.MatchString(w) {
			return fmt.Sprintf("starts with the word %q: words are ASCII letters and digits, and the first one starts with a letter", w)
		}
		if !personaWordRe.MatchString(w) {
			return fmt.Sprintf("holds the word %q: words are ASCII letters and digits", w)
		}
	}
	return ""
}

// colourSpellings folds the lexicon's variant spellings onto one key, so two
// bots cannot share a colour by spelling it differently.
var colourSpellings = map[string]string{"gray": "grey", "golden": "gold"}

// personaWordKeys returns the words a persona claims against other bots.
func personaWordKeys(words []string) []string {
	keys := make([]string, 0, len(words))
	for _, w := range words {
		if k, ok := colourSpellings[w]; ok {
			w = k
		}
		keys = append(keys, w)
	}
	return keys
}

// sharedWords returns the words of p's persona that another bot already
// claims through its slug or its persona, a colour counted once whatever its
// spelling.
func sharedWords(p, other catalogPersona) []string {
	taken := personaWordKeys(append(strings.Split(other.slug, "-"), personaWords(other.persona)...))
	var shared []string
	words := personaWords(p.persona)
	for i, k := range personaWordKeys(words) {
		if slices.Contains(taken, k) {
			shared = append(shared, words[i])
		}
	}
	return shared
}

// answersTo reports whether one of the commands is the persona's kebab form,
// compared the way routing compares them: trimmed and lower-cased.
func answersTo(commands []string, persona string) bool {
	want := strings.Join(personaWords(persona), "-")
	for _, c := range commands {
		if strings.ToLower(strings.TrimSpace(c)) == want {
			return true
		}
	}
	return false
}

// paletteMark: two words, exactly one of them a colour of the lexicon.
func paletteMark(persona string) bool {
	words := personaWords(persona)
	if len(words) != 2 {
		return false
	}
	colours := 0
	for _, w := range words {
		if personaColours[w] {
			colours++
		}
	}
	return colours == 1
}

// ionMark: the last word ends like iterion.
func ionMark(persona string) bool {
	words := personaWords(persona)
	return len(words) > 0 && strings.HasSuffix(words[len(words)-1], "ion")
}

func TestCatalogPersonaCarriesAMark(t *testing.T) {
	for _, p := range catalogPersonas(t) {
		if !p.underRule() {
			continue
		}
		if !paletteMark(p.persona) && !ionMark(p.persona) {
			t.Errorf("%s: persona %q carries no mark — two words with exactly one colour of the lexicon, or a last word ending in -ion (docs/agents/bots/naming.md); a name too good to lose goes in personaJokers with its reason", p.slug, p.persona)
		}
	}
}

func TestCatalogPersonaBounds(t *testing.T) {
	for _, p := range catalogPersonas(t) {
		if p.grandfathered() {
			continue
		}
		words := personaWords(p.persona)
		if len(words) == 0 {
			t.Errorf("%s: no display_name — a catalog bot carries a persona (docs/agents/bots/naming.md)", p.slug)
			continue
		}
		if why := personaShapeProblem(p.persona); why != "" {
			t.Errorf("%s: persona %q %s (docs/agents/bots/naming.md)", p.slug, p.persona, why)
		}
		if len(words) > personaMaxWords {
			t.Errorf("%s: persona %q has %d words; the doctrine allows %d", p.slug, p.persona, len(words), personaMaxWords)
		}
		if n := utf8.RuneCountInString(p.persona); n > personaMaxChars {
			t.Errorf("%s: persona %q is %d characters long; the doctrine allows %d", p.slug, p.persona, n, personaMaxChars)
		}
		letters := 0
		for _, r := range words[len(words)-1] {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters > personaMaxLastLetter {
			t.Errorf("%s: persona %q ends on a %d-letter word; the doctrine allows %d", p.slug, p.persona, letters, personaMaxLastLetter)
		}
	}
}

func TestCatalogPersonaSharesNoWord(t *testing.T) {
	personas := catalogPersonas(t)
	for _, p := range personas {
		if p.grandfathered() {
			continue
		}
		for _, other := range personas {
			if other.slug == p.slug {
				continue
			}
			for _, w := range sharedWords(p, other) {
				t.Errorf("%s: persona %q shares the word %q with bot %s (persona %q) — two bots never share a word, a colour counts once whatever its spelling (docs/agents/bots/naming.md)", p.slug, p.persona, w, other.slug, other.persona)
			}
		}
	}
}

func TestCatalogPersonasAreNotNearNeighbours(t *testing.T) {
	personas := catalogPersonas(t)
	for i, a := range personas {
		for _, b := range personas[i+1:] {
			pair := nearPair(a.persona, b.persona)
			if editDistance(pair[0], pair[1]) > personaNearDistance {
				continue
			}
			if !slices.Contains(grandfatheredNearPairs, pair) {
				t.Errorf("personas %q (%s) and %q (%s) are within edit distance %d — one of them reads as a typo of the other (docs/agents/bots/naming.md)", a.persona, a.slug, b.persona, b.slug, personaNearDistance)
			}
		}
	}
}

func TestCatalogPersonaAnswersToItsCommand(t *testing.T) {
	for _, p := range catalogPersonas(t) {
		if p.grandfathered() || len(p.commands) == 0 {
			continue
		}
		want := strings.Join(personaWords(p.persona), "-")
		if !answersTo(p.commands, p.persona) {
			t.Errorf("%s: persona %q declares commands %v but not /%s — a persona answers to its kebab-case form, as the command or an alias (docs/agents/bots/naming.md)", p.slug, p.persona, p.commands, want)
		}
	}
}

// TestCatalogNamingExceptionsOnlyShrink fails on an exception that no longer
// matches the catalog, so a renamed or retired bot takes its entry with it.
func TestCatalogNamingExceptionsOnlyShrink(t *testing.T) {
	personas := catalogPersonas(t)
	bySlug := map[string]string{}
	for _, p := range personas {
		bySlug[p.slug] = p.persona
	}
	for _, list := range []struct {
		name      string
		len, want int
	}{
		{"grandfatheredPersonas", len(grandfatheredPersonas), grandfatheredPersonaCount},
		{"grandfatheredNearPairs", len(grandfatheredNearPairs), grandfatheredNearPairCount},
	} {
		switch {
		case list.len > list.want:
			t.Errorf("%s holds %d entries but its count is %d — the list only shrinks: a new persona answers to the rule (docs/agents/bots/naming.md)", list.name, list.len, list.want)
		case list.len < list.want:
			t.Errorf("%s holds %d entries but its count is %d — lower the count with the removal", list.name, list.len, list.want)
		}
	}
	for slug, persona := range grandfatheredPersonas {
		if got, ok := bySlug[slug]; !ok || got != persona {
			t.Errorf("grandfatheredPersonas[%q] = %q but the catalog carries %q — remove the stale entry; a new name answers to the rule", slug, persona, got)
		}
	}
	for slug, j := range personaJokers {
		if strings.TrimSpace(j.why) == "" {
			t.Errorf("personaJokers[%q] gives no reason", slug)
		}
		if got, ok := bySlug[slug]; !ok || got != j.persona {
			t.Errorf("personaJokers[%q] = %q but the catalog carries %q — remove the stale entry", slug, j.persona, got)
		}
	}
	for _, pair := range grandfatheredNearPairs {
		var a, b bool
		for _, p := range personas {
			a = a || strings.ToLower(p.persona) == pair[0]
			b = b || strings.ToLower(p.persona) == pair[1]
		}
		if !a || !b || editDistance(pair[0], pair[1]) > personaNearDistance {
			t.Errorf("grandfatheredNearPairs holds %v, which is no longer a near pair of the catalog — remove it", pair)
		}
	}
}

// TestPersonaPredicatesHoldTheDoctrine pins each predicate the catalog guards
// call on names the catalog does not carry yet, accepted and refused alike.
func TestPersonaPredicatesHoldTheDoctrine(t *testing.T) {
	for _, tc := range []struct {
		persona   string
		wantShape bool // inside the alphabet
	}{
		{"Code Red", true},
		{"Right Direction", true},
		{"Devboxing", true},
		{"R2 Unit", true},
		{"Pink 5", true},           // a later word may be digits only: the kebab form still starts with a letter
		{"5 Pink", false},          // the first one may not: commandNameRe starts with a letter
		{"Red-Alert Union", false}, // a hyphen hides "red" from the shared-word check
		{"Rеd Union", false},       // Cyrillic "е": reads as "Red", compares as another word
		{"Évasion", false},         // no command can spell it: commandNameRe is ASCII
		{"Mr. Pink", false},        // nor this one
		{"Code  Red", false},       // the kebab form would be "code--red"
		{" Code Red", false},       // padding is not part of a name
		{"Code Red (beta)", false}, // a qualifier is a second bot, not a persona
	} {
		if got := personaShapeProblem(tc.persona) == ""; got != tc.wantShape {
			t.Errorf("personaShapeProblem(%q) accepts=%v, want %v (%q)", tc.persona, got, tc.wantShape, personaShapeProblem(tc.persona))
		}
	}

	for _, tc := range []struct {
		p, other catalogPersona
		want     []string
	}{
		// one grey bot and one gold bot, whatever the spelling, in both directions
		{catalogPersona{slug: "zz-a", persona: "Gray Matter"}, catalogPersona{slug: "arbitrate", persona: "Grey Area"}, []string{"gray"}},
		{catalogPersona{slug: "zz-a", persona: "Grey Matter"}, catalogPersona{slug: "zz-b", persona: "Gray Area"}, []string{"grey"}},
		// a slug claims its words too: golden-master holds "golden", so "gold"
		{catalogPersona{slug: "zz-a", persona: "Gold Rush"}, catalogPersona{slug: "golden-master", persona: "Goldy"}, []string{"gold"}},
		{catalogPersona{slug: "zz-a", persona: "Golden Hour"}, catalogPersona{slug: "golden-master", persona: "Goldy"}, []string{"golden"}},
		{catalogPersona{slug: "zz-a", persona: "Deep Blue"}, catalogPersona{slug: "zz-b", persona: "Blue Sky"}, []string{"blue"}},
		{catalogPersona{slug: "zz-a", persona: "Code Red"}, catalogPersona{slug: "review-env", persona: "Green Room"}, nil},
	} {
		if got := sharedWords(tc.p, tc.other); !slices.Equal(got, tc.want) {
			t.Errorf("sharedWords(%q, %s %q) = %q, want %q", tc.p.persona, tc.other.slug, tc.other.persona, got, tc.want)
		}
	}

	for _, tc := range []struct {
		persona          string
		palette, ionMark bool
	}{
		{"Code Red", true, false},
		{"Right Direction", false, true},
		{"Red Black", false, false},  // two colours: a palette name carries exactly one
		{"Red", false, false},        // one word is no palette name
		{"Onion Ring", false, false}, // -ion inside a word that is not the last one
		{"Ionic Blue", true, false},  // nor at the start of a word
	} {
		if got := paletteMark(tc.persona); got != tc.palette {
			t.Errorf("paletteMark(%q) = %v, want %v", tc.persona, got, tc.palette)
		}
		if got := ionMark(tc.persona); got != tc.ionMark {
			t.Errorf("ionMark(%q) = %v, want %v", tc.persona, got, tc.ionMark)
		}
	}

	for _, tc := range []struct {
		commands []string
		persona  string
		want     bool
	}{
		{[]string{"white-elephant", "rearchi"}, "White Elephant", true},
		{[]string{"Pink-Panther"}, "Pink Panther", true}, // routing lower-cases command names
		{[]string{" deep-blue "}, "Deep Blue", true},     // and trims them
		{[]string{"rearchi"}, "White Elephant", false},
	} {
		if got := answersTo(tc.commands, tc.persona); got != tc.want {
			t.Errorf("answersTo(%q, %q) = %v, want %v", tc.commands, tc.persona, got, tc.want)
		}
	}
}

// nearPair returns the two personas lower-cased and in lexical order, the
// shape grandfatheredNearPairs stores.
func nearPair(a, b string) [2]string {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if b < a {
		a, b = b, a
	}
	return [2]string{a, b}
}

// editDistance is the Levenshtein distance over runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			best := prev[j] + 1 // deletion
			if ins := cur[j-1] + 1; ins < best {
				best = ins
			}
			if sub := prev[j-1] + cost; sub < best {
				best = sub
			}
			cur[j] = best
		}
		prev = cur
	}
	return prev[len(rb)]
}
