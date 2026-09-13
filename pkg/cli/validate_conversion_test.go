package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

func TestValidateJSONLegacyConversionDraftIsIncomplete(t *testing.T) {
	const source = `tool publish:
  command: "echo hi"
workflow main:
  vars:
    outline: json
  entry: publish
  publish -> done
`
	path := writeFixture(t, t.TempDir(), "legacy.bot", source)
	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunValidate(path, p); err != nil {
		t.Fatal(err)
	}
	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.PublicView != nil || result.ConversionDraft == nil ||
		result.ConversionDraft.Status != "incomplete" ||
		len(result.ConversionDraft.CandidateInputs) != 1 ||
		result.ConversionDraft.CandidateInputs[0].LegacyType != "json" {
		t.Fatalf("CLI certified or omitted legacy draft: %+v", result)
	}
}
