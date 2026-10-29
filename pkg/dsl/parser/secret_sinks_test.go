package parser

import (
	"strings"
	"testing"
)

// `sinks:` reads a bracket list of bare sink names (or quoted strings); the
// EMPTY list reads as no list at all — every writer and the JSON transport
// drop an empty list, so the empty form means what its absence means.
func TestASecretSinksListReadsBareNamesAndTheEmptyFormAsNoList(t *testing.T) {
	pr, err := Parse("t.bot", `secrets:
  tok:
    sinks: [prompt, "egress"]

tool k:
  command: `+"`echo hi`"+`

workflow w:
  entry: k
  k -> done
`)
	if err != nil || len(pr.Diagnostics) > 0 {
		t.Fatalf("a well-formed sinks list produced diagnostics: %v %+v", err, pr.Diagnostics)
	}
	if pr.File.Secrets == nil || len(pr.File.Secrets.Fields) != 1 || strings.Join(pr.File.Secrets.Fields[0].Sinks, ",") != "prompt,egress" {
		t.Fatalf("sinks not read: %+v", pr.File.Secrets)
	}

	empty, _ := Parse("t.bot", `secrets:
  tok:
    sinks: []

tool k:
  command: `+"`echo hi`"+`

workflow w:
  entry: k
  k -> done
`)
	if len(empty.Diagnostics) > 0 {
		t.Fatalf("the empty form drew a diagnostic: %+v", empty.Diagnostics)
	}
	if got := empty.File.Secrets.Fields[0].Sinks; got != nil {
		t.Fatalf("sinks: [] read as %#v, want nil — no list", got)
	}
}
