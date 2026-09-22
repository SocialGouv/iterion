package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// A property the language ADDED in a profile is refused by the parser in
// the profile before it, and accepted from its own — the registry's Since
// mark and the parser agree, as they do for Until. No property carries a
// Since above 1 today: the first that does trips the loop below, which
// then demands the parser's refusal; until then the test proves the
// registry's marks are within the profiles this build reads, so a mark on
// a profile that does not exist cannot describe a refusal nobody makes.
func TestSinceMarksMatchTheParser(t *testing.T) {
	marked := 0
	for kind, tmpl := range probes {
		k, ok := spec.Lookup(kind)
		if !ok {
			continue
		}
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		at := strings.Index(line, "%s")
		indent := line[strings.LastIndex(line[:at], "\n")+1 : at]
		for _, p := range k.Properties {
			if p.Since > parser.MaxProfile || p.Until > parser.MaxProfile {
				t.Errorf("%s.%s: a profile mark (since %d, until %d) beyond the newest profile %d", kind, p.Name, p.Since, p.Until, parser.MaxProfile)
			}
			if p.Since > 0 && p.Until > 0 && p.Until < p.Since {
				t.Errorf("%s.%s: an empty window (since %d, until %d)", kind, p.Name, p.Since, p.Until)
			}
			if p.Since <= 1 {
				continue
			}
			marked++
			sample := strings.ReplaceAll(sampleValues(p)[0], "\n", "\n"+indent)
			doc := fmt.Sprintf(line, sample)
			if res := parser.Parse("probe.bot", fmt.Sprintf("dsl: %d\n", p.Since-1)+doc); len(res.Diagnostics) == 0 {
				t.Errorf("%s.%s: the registry says the property arrives in profile %d, yet profile %d accepts it", kind, p.Name, p.Since, p.Since-1)
			}
			if res := parser.Parse("probe.bot", fmt.Sprintf("dsl: %d\n", p.Since)+doc); len(res.Diagnostics) != 0 {
				t.Errorf("%s.%s: accepted from profile %d by the registry, refused by the parser: %v", kind, p.Name, p.Since, res.Diagnostics)
			}
		}
	}
	t.Logf("%d properties carry a Since above 1", marked)
}
