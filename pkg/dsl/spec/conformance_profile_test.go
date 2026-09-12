package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// A property the language removed from a profile is accepted up to that
// profile and refused by name (E043) from the next one — the registry's
// Until mark and the parser agree in both directions, kind by kind; every
// unmarked property is accepted in the newest profile. A mark the parser
// does not enforce, or a refusal the registry does not announce, fails
// here, so the rendered documents never send an author to write a line the
// parser refuses.
func TestProfileMarksMatchTheParser(t *testing.T) {
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
			sample := strings.ReplaceAll(sampleValues(p)[0], "\n", "\n"+indent)
			doc := fmt.Sprintf(line, sample)
			if p.Until == 0 {
				res := parser.Parse("probe.bot", fmt.Sprintf("dsl: %d\n", parser.MaxProfile)+doc)
				for _, d := range res.Diagnostics {
					if d.Code == parser.DiagRemovedInProfile || d.Code == parser.DiagUnknownProperty {
						t.Errorf("%s.%s: unmarked, yet profile %d refuses it: %v", kind, p.Name, parser.MaxProfile, d)
					}
				}
				continue
			}
			marked++
			res := parser.Parse("probe.bot", fmt.Sprintf("dsl: %d\n", p.Until)+doc)
			if len(res.Diagnostics) > 0 {
				t.Errorf("%s.%s: accepted until profile %d by the registry, refused by the parser: %v", kind, p.Name, p.Until, res.Diagnostics)
			}
			if p.Until+1 > parser.MaxProfile {
				continue
			}
			res = parser.Parse("probe.bot", fmt.Sprintf("dsl: %d\n", p.Until+1)+doc)
			if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagRemovedInProfile {
				t.Errorf("%s.%s: removed from profile %d by the registry; the parser says %v (want exactly one E043)", kind, p.Name, p.Until+1, res.Diagnostics)
			}
		}
	}
	if marked == 0 {
		t.Fatalf("no property carries an Until mark — the test proves nothing")
	}
}
